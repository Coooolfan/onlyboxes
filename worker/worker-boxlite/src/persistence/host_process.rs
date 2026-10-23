//! Host process evidence survives SDK PID-file removal and Worker restarts.
//! Exit queries never signal; explicit stops use pinned kernel identities.
//! Unavailable OS evidence is not an exit.
use super::{Error, WriterCgroup};
use serde::{Deserialize, Serialize};

type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HostProcess {
    pub host_id: String,
    pub boot_id: String,
    pub process_namespace: String,
    pub pid: u32,
    pub start_time: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub writer_cgroup: Option<WriterCgroup>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub signal_version: Option<u32>,
}
impl HostProcess {
    /// Capture the calling Worker/helper lifecycle without writer privileges.
    pub fn current() -> Result<Self> {
        Self::capture(std::process::id())
    }

    /// Read-only capture of a native writer selected by a trusted SDK caller.
    /// Signals still require the pinned kernel identity; this grants no lease.
    pub fn inspect_writer(pid: u32, box_id: &str) -> Result<Self> {
        if !super::types::valid_id(box_id) {
            return Err(Error::Conflict);
        }
        let mut process = Self::capture(pid)?;
        process.bind_writer_group(box_id)?;
        process.verify_current_writer(box_id)?;
        Ok(process)
    }

    pub(crate) fn verify_current_writer(&self, box_id: &str) -> Result<()> {
        self.validate_native_id(box_id)?;
        let mut current = Self::capture(self.pid)?;
        current.bind_writer_group(box_id)?;
        if current != *self {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    pub(crate) fn validate_native_id(&self, id: &str) -> Result<()> {
        self.validate()?;
        if let Some(group) = &self.writer_cgroup {
            group.validate_native_id(id)?;
        }
        Ok(())
    }

    pub(crate) fn validate(&self) -> Result<()> {
        let host = uuid::Uuid::parse_str(&self.host_id).map_err(|_| Error::Conflict)?;
        if host.is_nil() || host.to_string() != self.host_id {
            return Err(Error::Conflict);
        }
        let uuid = uuid::Uuid::parse_str(&self.boot_id).map_err(|_| Error::Conflict)?;
        if !valid_namespace(&self.process_namespace)
            || uuid.is_nil()
            || uuid.to_string() != self.boot_id
            || self.pid <= 1
            || self.pid > i32::MAX as u32
            || self.start_time == 0
            || self.signal_version == Some(0)
        {
            return Err(Error::Conflict);
        }
        if let Some(group) = &self.writer_cgroup {
            group.validate()?;
        }
        if (self.signal_version.is_some() && self.process_namespace != "host")
            || (self.writer_cgroup.is_some() && self.process_namespace == "host")
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    pub(crate) fn capture(pid: u32) -> Result<Self> {
        if pid <= 1 || pid > i32::MAX as u32 {
            return Err(Error::Conflict);
        }
        let host_id = host_id()?;
        let boot_id = boot_id()?;
        let process_namespace = process_namespace()?;
        let start_time = match observe(pid) {
            Observation::Present(start) => start,
            Observation::Gone | Observation::Unavailable => return Err(Error::Unavailable),
        };
        let process = Self {
            host_id,
            boot_id,
            process_namespace,
            pid,
            start_time,
            writer_cgroup: None,
            signal_version: None,
        };
        process.validate()?;
        if process.host_id != self::host_id()?
            || process.boot_id != self::boot_id()?
            || process.process_namespace != self::process_namespace()?
        {
            return Err(Error::Unavailable);
        }
        Ok(process)
    }

    /// Called while the exact native instance still runs, before its PID file
    /// can be removed. Linux must bind the entire detached VM subtree.
    pub(crate) fn bind_writer_group(&mut self, box_id: &str) -> Result<()> {
        #[cfg(target_os = "linux")]
        {
            let group = WriterCgroup::capture(self.pid, box_id)?;
            if Self::capture(self.pid)? != *self {
                return Err(Error::Unavailable);
            }
            self.writer_cgroup = Some(group);
        }
        #[cfg(target_os = "macos")]
        {
            let _ = box_id;
            let version = super::host_signal::version(self.pid)?;
            if Self::capture(self.pid)? != *self {
                return Err(Error::Unavailable);
            }
            self.signal_version = Some(version);
        }
        #[cfg(not(any(target_os = "linux", target_os = "macos")))]
        return Err(Error::Unavailable);
        Ok(())
    }

    /// Requests a hard stop of the pinned writer identity without touching its
    /// disk or SDK database. Success is only signal delivery, not exit proof.
    pub fn request_writer_stop(&self) -> Result<()> {
        self.validate()?;
        if self.host_id != host_id()? {
            return Err(Error::Unavailable);
        }
        if self.boot_id != boot_id()? {
            return Ok(());
        }
        if self.process_namespace != process_namespace()? {
            return Err(Error::Unavailable);
        }
        #[cfg(target_os = "linux")]
        {
            self.writer_cgroup
                .as_ref()
                .ok_or(Error::Unavailable)?
                .request_kill()
        }
        #[cfg(target_os = "macos")]
        {
            if self.has_exited()? {
                return Ok(());
            }
            super::host_signal::kill(self.pid, self.signal_version.ok_or(Error::Unavailable)?)
        }
        #[cfg(not(any(target_os = "linux", target_os = "macos")))]
        Err(Error::Unavailable)
    }

    /// Publication/deletion proof includes descendants; ordinary process exit
    /// queries intentionally retain their narrower semantics.
    pub fn writers_exited(&self) -> Result<bool> {
        if !self.has_exited()? {
            return Ok(false);
        }
        // A different boot on the same validated host ends every old writer.
        if self.boot_id != boot_id()? {
            return Ok(true);
        }
        if let Some(group) = &self.writer_cgroup {
            return group.is_empty();
        }
        #[cfg(target_os = "linux")]
        return Err(Error::Unavailable);
        #[cfg(not(target_os = "linux"))]
        Ok(true)
    }

    /// On the same host, a different boot or start time proves this lifecycle ended. This
    /// never kills, attaches to, or reaps a process occupying a recycled PID.
    pub fn has_exited(&self) -> Result<bool> {
        self.validate()?;
        let host = host_id()?;
        if host != self.host_id {
            return Err(Error::Unavailable);
        }
        let boot = boot_id()?;
        if boot != self.boot_id {
            return Ok(true);
        }
        let namespace = process_namespace()?;
        if namespace != self.process_namespace {
            return Err(Error::Unavailable);
        }
        let observed = observe(self.pid);
        if boot != boot_id()? {
            return Ok(true);
        }
        if namespace != process_namespace()? {
            return Err(Error::Unavailable);
        }
        classify(self, &host, &boot, &namespace, observed)
    }
}

enum Observation {
    Present(u64),
    Gone,
    Unavailable,
}
fn classify(
    expected: &HostProcess,
    host: &str,
    boot: &str,
    namespace: &str,
    observed: Observation,
) -> Result<bool> {
    if host != expected.host_id {
        return Err(Error::Unavailable);
    }
    if boot != expected.boot_id {
        return Ok(true);
    }
    if namespace != expected.process_namespace {
        return Err(Error::Unavailable);
    }
    match observed {
        Observation::Present(start) if start > 0 => Ok(start != expected.start_time),
        Observation::Gone => Ok(true),
        Observation::Present(_) | Observation::Unavailable => Err(Error::Unavailable),
    }
}
fn observe(pid: u32) -> Observation {
    if let Some(observed) = native_observation(pid) {
        return observed;
    }
    // SAFETY: PID is validated positive and representable; signal 0 only queries
    // existence. Permission errors and unavailable start-time reads fail closed.
    let result = unsafe { libc::kill(pid as i32, 0) };
    if result < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH) {
        Observation::Gone
    } else {
        Observation::Unavailable
    }
}
#[cfg(target_os = "macos")]
fn native_observation(pid: u32) -> Option<Observation> {
    let mut info = std::mem::MaybeUninit::<libc::proc_bsdinfo>::zeroed();
    let size = std::mem::size_of::<libc::proc_bsdinfo>();
    // SAFETY: libproc receives an exactly sized writable BSD-info buffer.
    // arg=1 includes unreaped zombies; XNU proc_pidinfo otherwise omits them.
    let read = unsafe {
        libc::proc_pidinfo(
            pid as i32,
            libc::PROC_PIDTBSDINFO,
            1,
            info.as_mut_ptr().cast(),
            size as i32,
        )
    };
    if read != size as i32 {
        return None;
    }
    // SAFETY: the kernel returned the complete initialized structure above.
    let info = unsafe { info.assume_init() };
    if info.pbi_pid != pid {
        return None;
    }
    let start = info
        .pbi_start_tvsec
        .checked_mul(1_000_000)?
        .checked_add(info.pbi_start_tvusec)?;
    if start == 0 {
        return None;
    }
    Some(if info.pbi_status == libc::SZOMB {
        Observation::Gone
    } else {
        Observation::Present(start)
    })
}
#[cfg(target_os = "linux")]
fn native_observation(pid: u32) -> Option<Observation> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let tail = stat.get(stat.rfind(')')? + 1..)?;
    let mut fields = tail.split_whitespace();
    let state = fields.next()?;
    // State is field 3; starttime is field 22, after 18 intervening fields.
    let start = fields.nth(18)?.parse::<u64>().ok()?;
    if start == 0 {
        return None;
    }
    Some(if matches!(state, "Z" | "X") {
        Observation::Gone
    } else {
        Observation::Present(start)
    })
}
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
fn native_observation(pid: u32) -> Option<Observation> {
    boxlite::util::process_start_time(pid).map(Observation::Present)
}

fn valid_namespace(value: &str) -> bool {
    if value == "host" {
        return true;
    }
    let Some((dev, ino)) = value
        .strip_prefix("linux-pid:")
        .and_then(|s| s.split_once(':'))
    else {
        return false;
    };
    match (dev.parse::<u64>(), ino.parse::<u64>()) {
        (Ok(dev), Ok(ino)) if ino > 0 => value == format!("linux-pid:{dev}:{ino}"),
        _ => false,
    }
}
fn process_namespace() -> Result<String> {
    #[cfg(target_os = "linux")]
    {
        use std::os::unix::fs::MetadataExt;
        let meta = std::fs::metadata("/proc/self/ns/pid").map_err(|_| Error::Unavailable)?;
        Ok(format!("linux-pid:{}:{}", meta.dev(), meta.ino()))
    }
    #[cfg(target_os = "macos")]
    {
        Ok("host".into())
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        Err(Error::Unavailable)
    }
}

fn host_id() -> Result<String> {
    #[cfg(target_os = "linux")]
    {
        use std::io::Read;
        let mut value = String::new();
        std::fs::File::open("/etc/machine-id")
            .map_err(|_| Error::Unavailable)?
            .take(129)
            .read_to_string(&mut value)
            .map_err(|_| Error::Unavailable)?;
        if value.len() > 128 {
            return Err(Error::Unavailable);
        }
        let uuid = uuid::Uuid::parse_str(value.trim()).map_err(|_| Error::Unavailable)?;
        if uuid.is_nil() {
            return Err(Error::Unavailable);
        }
        Ok(uuid.to_string())
    }
    #[cfg(target_os = "macos")]
    {
        let mut value = [0u8; 16];
        let timeout = libc::timespec {
            tv_sec: 1,
            tv_nsec: 0,
        };
        // SAFETY: gethostuuid writes exactly 16 bytes; timeout is a live const
        // timespec. A failed host-identity query never proves a remote VM died.
        if unsafe { libc::gethostuuid(value.as_mut_ptr(), &timeout) } != 0 {
            return Err(Error::Unavailable);
        }
        let uuid = uuid::Uuid::from_bytes(value);
        if uuid.is_nil() {
            return Err(Error::Unavailable);
        }
        Ok(uuid.to_string())
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        Err(Error::Unavailable)
    }
}

fn boot_id() -> Result<String> {
    #[cfg(target_os = "linux")]
    let value = {
        use std::io::Read;
        let mut bytes = String::new();
        std::fs::File::open("/proc/sys/kernel/random/boot_id")
            .map_err(|_| Error::Unavailable)?
            .take(129)
            .read_to_string(&mut bytes)
            .map_err(|_| Error::Unavailable)?;
        if bytes.len() > 128 {
            return Err(Error::Unavailable);
        }
        bytes
    };
    #[cfg(target_os = "macos")]
    let value = {
        let mut buffer = [0u8; 128];
        let mut length = buffer.len();
        // SAFETY: constant NUL-terminated name and writable buffer of length
        // `length`. sysctl writes at most that capacity and updates the length.
        let result = unsafe {
            libc::sysctlbyname(
                c"kern.bootsessionuuid".as_ptr(),
                buffer.as_mut_ptr().cast(),
                &mut length,
                std::ptr::null_mut(),
                0,
            )
        };
        if result != 0 || length == 0 || length > buffer.len() {
            return Err(Error::Unavailable);
        }
        let bytes = &buffer[..length];
        let bytes = bytes.strip_suffix(&[0]).unwrap_or(bytes);
        std::str::from_utf8(bytes)
            .map_err(|_| Error::Unavailable)?
            .to_owned()
    };
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    return Err(Error::Unavailable);
    #[cfg(any(target_os = "linux", target_os = "macos"))]
    {
        let uuid = uuid::Uuid::parse_str(value.trim()).map_err(|_| Error::Unavailable)?;
        if uuid.is_nil() {
            return Err(Error::Unavailable);
        }
        Ok(uuid.to_string())
    }
}

#[cfg(test)]
#[path = "host_process_tests.rs"]
mod tests;
