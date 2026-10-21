//! A detached Linux launcher exiting does not prove its inner VM exited.
//! Pin the containing cgroup before stop, then require an empty whole subtree.
use super::Error;
use serde::{Deserialize, Serialize};
use std::fs::{self, File, OpenOptions};
use std::io::Read;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Component, Path, PathBuf};

type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WriterCgroup {
    path: PathBuf,
    device: u64,
    inode: u64,
}

impl WriterCgroup {
    pub(crate) fn validate(&self) -> Result<()> {
        if !self.path.starts_with("/sys/fs/cgroup")
            || self.path.as_os_str().len() > 4096
            || self.inode == 0
            || self
                .path
                .components()
                .any(|c| !matches!(c, Component::RootDir | Component::Normal(_)))
        {
            return Err(Error::Conflict);
        }
        let id = self
            .path
            .file_name()
            .and_then(|s| s.to_str())
            .ok_or(Error::Conflict)?;
        if !valid_id(id)
            || self.path.parent().and_then(Path::file_name) != Some(std::ffi::OsStr::new("boxlite"))
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    pub(crate) fn validate_native_id(&self, id: &str) -> Result<()> {
        self.validate()?;
        if self.path.file_name().and_then(|s| s.to_str()) != Some(id) {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    #[cfg(target_os = "linux")]
    pub(crate) fn capture(pid: u32, id: &str) -> Result<Self> {
        let value = read_bounded(&PathBuf::from(format!("/proc/{pid}/cgroup")), 8192)?;
        let relative = unified_group(&value, id)?;
        let path = Path::new("/sys/fs/cgroup").join(relative.trim_start_matches('/'));
        let directory = open_directory(&path)?;
        require_cgroupfs(&directory)?;
        let meta = directory.metadata().map_err(|_| Error::Unavailable)?;
        let group = Self {
            path,
            device: meta.dev(),
            inode: meta.ino(),
        };
        group.validate()?;
        // No path redirection, including through ancestor symlinks.
        if fs::canonicalize(&group.path).map_err(|_| Error::Unavailable)? != group.path {
            return Err(Error::Unavailable);
        }
        // cgroup.events is hierarchical: descendants count even if the outer
        // launcher's process group or PID namespace has already disappeared.
        if !populated(&read_events(&directory)?)? {
            return Err(Error::Unavailable);
        }
        Ok(group)
    }

    pub(crate) fn is_empty(&self) -> Result<bool> {
        self.validate()?;
        self.is_empty_at(&self.path)
    }

    fn is_empty_at(&self, path: &Path) -> Result<bool> {
        let directory = open_directory(path)?;
        require_cgroupfs(&directory)?;
        let before = directory.metadata().map_err(|_| Error::Unavailable)?;
        if before.dev() != self.device || before.ino() != self.inode {
            return Err(Error::Unavailable);
        }
        let empty = !populated(&read_events(&directory)?)?;
        let after = fs::symlink_metadata(path).map_err(|_| Error::Unavailable)?;
        if !after.is_dir() || after.dev() != self.device || after.ino() != self.inode {
            return Err(Error::Unavailable);
        }
        Ok(empty)
    }
}

fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

#[cfg(any(target_os = "linux", test))]
fn unified_group(value: &str, id: &str) -> Result<String> {
    if !valid_id(id) || value.len() > 8192 {
        return Err(Error::Unavailable);
    }
    let mut group = None;
    for line in value.lines() {
        let Some(path) = line.strip_prefix("0::") else {
            return Err(Error::Unavailable);
        };
        if group.is_some()
            || !path.starts_with('/')
            || path.len() > 4096
            || path.split('/').skip(1).any(|part| {
                part.is_empty() || part == "." || part == ".." || part.chars().any(char::is_control)
            })
            || !path.ends_with(&format!("/boxlite/{id}"))
        {
            return Err(Error::Unavailable);
        }
        group = Some(path.to_owned());
    }
    group.ok_or(Error::Unavailable)
}

fn populated(value: &str) -> Result<bool> {
    let mut result = None;
    for line in value.lines() {
        let mut parts = line.split_whitespace();
        let key = parts.next().ok_or(Error::Unavailable)?;
        let value = parts.next().ok_or(Error::Unavailable)?;
        if parts.next().is_some() || value.parse::<u64>().is_err() {
            return Err(Error::Unavailable);
        }
        if key == "populated" {
            if result.is_some() || !matches!(value, "0" | "1") {
                return Err(Error::Unavailable);
            }
            result = Some(value == "1");
        }
    }
    result.ok_or(Error::Unavailable)
}

#[cfg(target_os = "linux")]
fn read_bounded(path: &Path, limit: u64) -> Result<String> {
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| Error::Unavailable)?;
    read_file(file, limit)
}

fn read_events(directory: &File) -> Result<String> {
    use std::os::fd::{AsRawFd, FromRawFd};
    // SAFETY: directory owns a valid descriptor; the fixed leaf cannot escape
    // it. O_NOFOLLOW prevents replacing events with a symlink.
    let descriptor = unsafe {
        libc::openat(
            directory.as_raw_fd(),
            c"cgroup.events".as_ptr(),
            libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC,
        )
    };
    if descriptor < 0 {
        return Err(Error::Unavailable);
    }
    // SAFETY: openat returned a new descriptor, transferred exactly once.
    read_file(unsafe { File::from_raw_fd(descriptor) }, 4096)
}

fn read_file(file: File, limit: u64) -> Result<String> {
    if !file.metadata().map_err(|_| Error::Unavailable)?.is_file() {
        return Err(Error::Unavailable);
    }
    let mut value = String::new();
    file.take(limit + 1)
        .read_to_string(&mut value)
        .map_err(|_| Error::Unavailable)?;
    if value.len() as u64 > limit {
        return Err(Error::Unavailable);
    }
    Ok(value)
}

fn open_directory(path: &Path) -> Result<File> {
    OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| Error::Unavailable)
}

#[cfg(target_os = "linux")]
fn require_cgroupfs(directory: &File) -> Result<()> {
    use std::os::fd::AsRawFd;
    let mut status = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: fstatfs writes a complete statfs to a valid output pointer.
    if unsafe { libc::fstatfs(directory.as_raw_fd(), status.as_mut_ptr()) } != 0 {
        return Err(Error::Unavailable);
    }
    // SAFETY: a successful fstatfs initialized the complete value.
    if unsafe { status.assume_init() }.f_type != 0x6367_7270 {
        return Err(Error::Unavailable);
    }
    Ok(())
}

#[cfg(not(target_os = "linux"))]
fn require_cgroupfs(_directory: &File) -> Result<()> {
    Err(Error::Unavailable)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unified_membership_must_be_exact_and_unambiguous() {
        assert_eq!(
            unified_group("0::/user.slice/boxlite/box_1\n", "box_1").unwrap(),
            "/user.slice/boxlite/box_1"
        );
        for value in [
            "",
            "0::/boxlite/other",
            "0::/boxlite/box_1/child",
            "0::/boxlite/../boxlite/box_1",
            "0::/boxlite//box_1",
            "0::/boxlite/box_1\n0::/boxlite/box_1",
            "1:cpu:/boxlite/box_1",
            "0::relative/boxlite/box_1",
        ] {
            assert!(unified_group(value, "box_1").is_err(), "{value}");
        }
    }
    #[test]
    fn hierarchical_population_is_required_not_only_direct_pids() {
        assert_eq!(populated("populated 1\nfrozen 0\n"), Ok(true));
        assert_eq!(populated("populated 0\nfrozen 0\n"), Ok(false));
        for value in [
            "",
            "frozen 0",
            "populated 2",
            "populated 0\npopulated 1",
            "populated 0 ignored",
            "populated -1",
        ] {
            assert!(populated(value).is_err());
        }
    }
    #[test]
    fn private_regular_files_cannot_impersonate_kernel_writer_evidence() {
        let root = std::env::temp_dir().join(format!("onlyboxes-cgroup-{}", uuid::Uuid::new_v4()));
        fs::create_dir(&root).unwrap();
        fs::write(root.join("cgroup.events"), "populated 0\n").unwrap();
        let meta = fs::metadata(&root).unwrap();
        let group = WriterCgroup {
            path: PathBuf::from("/sys/fs/cgroup/boxlite/owned"),
            device: meta.dev(),
            inode: meta.ino(),
        };
        assert_eq!(group.validate_native_id("owned"), Ok(()));
        assert_eq!(group.validate_native_id("foreign"), Err(Error::Conflict));
        assert!(group.is_empty_at(&root).is_err());
        fs::remove_dir_all(root).unwrap();
    }

    /// Uses only a uniquely owned temporary cgroup and child processes; no VM
    /// or existing BoxLite cgroup is touched. Requires a delegated cgroup v2 root.
    #[cfg(target_os = "linux")]
    #[test]
    fn native_launcher_exit_does_not_hide_a_live_descendant() {
        if std::env::var("BOXLITE_CGROUP_INTEGRATION").as_deref() != Ok("1") {
            return;
        }
        use std::io::{BufRead, Write};
        use std::process::{Command, Stdio};
        struct OwnedGroup {
            root: PathBuf,
            group: PathBuf,
        }
        impl Drop for OwnedGroup {
            fn drop(&mut self) {
                let _ = fs::write(self.group.join("cgroup.kill"), "1");
                for _ in 0..100 {
                    if fs::remove_dir(&self.group).is_ok() {
                        break;
                    }
                    std::thread::sleep(std::time::Duration::from_millis(10));
                }
                let _ = fs::remove_dir(self.root.join("boxlite"));
                let _ = fs::remove_dir(&self.root);
            }
        }
        let root = PathBuf::from("/sys/fs/cgroup")
            .join(format!("onlyboxes-writer-test-{}", uuid::Uuid::new_v4()));
        fs::create_dir(&root).expect("writable delegated cgroup v2 root required");
        fs::create_dir(root.join("boxlite")).unwrap();
        let group = OwnedGroup {
            group: root.join("boxlite/owned_child"),
            root,
        };
        fs::create_dir(&group.group).unwrap();
        let mut launcher = Command::new("sh")
            .args(["-c", "read gate; sleep 300 & echo $!; wait"])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        fs::write(group.group.join("cgroup.procs"), launcher.id().to_string()).unwrap();
        launcher
            .stdin
            .take()
            .unwrap()
            .write_all(b"start\n")
            .unwrap();
        let mut descendant = String::new();
        std::io::BufReader::new(launcher.stdout.take().unwrap())
            .read_line(&mut descendant)
            .unwrap();
        assert!(descendant.trim().parse::<u32>().unwrap() > 1);
        let evidence = WriterCgroup::capture(launcher.id(), "owned_child").unwrap();
        assert_eq!(evidence.is_empty(), Ok(false));
        launcher.kill().unwrap();
        launcher.wait().unwrap();
        // The launcher is reaped; its independently running child still holds
        // the subtree populated. Empty direct-parent/PID evidence is insufficient.
        assert_eq!(evidence.is_empty(), Ok(false));
        fs::write(group.group.join("cgroup.kill"), "1").unwrap();
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        while evidence.is_empty() == Ok(false) && std::time::Instant::now() < deadline {
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        assert_eq!(evidence.is_empty(), Ok(true));
        let mut replaced = evidence.clone();
        replaced.inode += 1;
        assert!(replaced.is_empty().is_err());
        fs::remove_dir(&group.group).unwrap();
        assert!(
            evidence.is_empty().is_err(),
            "missing cgroup is not empty-group evidence"
        );
        fs::create_dir(&group.group).unwrap();
        assert!(
            evidence.is_empty().is_err(),
            "same path cannot replace the pinned group"
        );
    }
}
