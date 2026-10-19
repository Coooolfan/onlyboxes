//! Host process evidence survives SDK PID-file removal and Worker restarts.
//! Queries never signal a process; unavailable OS evidence is not an exit.
use super::Error;
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
}
impl HostProcess {
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
    if let Some(start) = boxlite::util::process_start_time(pid) {
        return Observation::Present(start);
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
