//! A native writer can exist before the SDK publishes shim.pid. Pin the
//! box-scoped executable before boot and discover positive kernel evidence.
use super::journal::private_options;
use super::types::valid_id;
use super::{Error, HostProcess};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fs::{self, File};
use std::io::{Read, Write};
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};

type Result<T> = std::result::Result<T, Error>;
const MAX_EXECUTABLE: u64 = 512 << 20;
const MAX_PROCESSES: usize = 16384;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Directory {
    device: u64,
    inode: u64,
}
impl Directory {
    fn inspect(path: &Path, private: bool) -> Result<Self> {
        let metadata = fs::symlink_metadata(path).map_err(|_| Error::Unavailable)?;
        // SAFETY: geteuid reads the current identity.
        if !metadata.is_dir()
            || metadata.uid() != unsafe { libc::geteuid() }
            || metadata.mode() & if private { 0o077 } else { 0o022 } != 0
        {
            return Err(Error::Unavailable);
        }
        Ok(Self {
            device: metadata.dev(),
            inode: metadata.ino(),
        })
    }
}
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Image {
    device: u64,
    inode: u64,
    size: u64,
    modified: i64,
    modified_ns: i64,
    changed: i64,
    changed_ns: i64,
}
impl Image {
    fn of(file: &File) -> Result<Self> {
        let m = file.metadata().map_err(|_| Error::Unavailable)?;
        if !m.is_file()
            || m.nlink() != 1
            || m.mode() & 0o022 != 0
            || m.mode() & 0o111 == 0
            || m.len() == 0
            || m.len() > MAX_EXECUTABLE
        {
            return Err(Error::Unavailable);
        }
        Ok(Self {
            device: m.dev(),
            inode: m.ino(),
            size: m.len(),
            modified: m.mtime(),
            modified_ns: m.mtime_nsec(),
            changed: m.ctime(),
            changed_ns: m.ctime_nsec(),
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BootExecutable {
    home: PathBuf,
    box_id: String,
    directories: Vec<Directory>,
    image: Image,
    sha256: String,
}
impl BootExecutable {
    /// Called only for a stopped, journal-owned native instance. Existing
    /// executable bytes are reused only when they match the trusted SDK asset.
    pub(crate) fn prepare(home: &Path, id: &str, source: &Path, create: bool) -> Result<Self> {
        if !home.is_absolute() || !valid_id(id) {
            return Err(Error::Conflict);
        }
        let home = fs::canonicalize(home).map_err(|_| Error::Unavailable)?;
        Directory::inspect(&home, true)?;
        Directory::inspect(&home.join("boxes"), false)?;
        let native = home.join("boxes").join(id);
        Directory::inspect(&native, false)?;
        let source = fs::canonicalize(source).map_err(|_| Error::Unavailable)?;
        let mut input = private_options()
            .read(true)
            .open(source)
            .map_err(|_| Error::Unavailable)?;
        let original = Image::of(&input)?;
        let hash = hash_file(&mut input)?;
        if Image::of(&input)? != original {
            return Err(Error::Unavailable);
        }
        let bin = native.join("bin");
        match fs::symlink_metadata(&bin) {
            Ok(_) => {
                Directory::inspect(&bin, false)?;
            }
            Err(error) if create && error.kind() == std::io::ErrorKind::NotFound => {
                use std::os::unix::fs::DirBuilderExt;
                fs::DirBuilder::new()
                    .mode(0o700)
                    .create(&bin)
                    .map_err(|_| Error::Unavailable)?;
                File::open(&native)
                    .and_then(|f| f.sync_all())
                    .map_err(|_| Error::Unavailable)?;
            }
            Err(_) => return Err(Error::Unavailable),
        }
        let executable = bin.join("boxlite-shim");
        match private_options().read(true).open(&executable) {
            Ok(mut existing) => {
                let before = Image::of(&existing)?;
                if hash_file(&mut existing)? != hash || Image::of(&existing)? != before {
                    return Err(Error::Conflict);
                }
            }
            Err(error) if create && error.kind() == std::io::ErrorKind::NotFound => {
                use std::io::{Seek, SeekFrom};
                input
                    .seek(SeekFrom::Start(0))
                    .map_err(|_| Error::Unavailable)?;
                let temp = bin.join(format!("shim-{}.pending", uuid::Uuid::new_v4()));
                let result = (|| {
                    let mut output = private_options()
                        .write(true)
                        .create_new(true)
                        .open(&temp)
                        .map_err(|_| Error::Unavailable)?;
                    let copied = std::io::copy(
                        &mut std::io::Read::by_ref(&mut input).take(MAX_EXECUTABLE + 1),
                        &mut output,
                    )
                    .map_err(|_| Error::Unavailable)?;
                    if copied != original.size || Image::of(&input)? != original {
                        return Err(Error::Unavailable);
                    }
                    output
                        .set_permissions(fs::Permissions::from_mode(0o500))
                        .map_err(|_| Error::Unavailable)?;
                    output
                        .set_times(
                            fs::FileTimes::new().set_modified(
                                input
                                    .metadata()
                                    .and_then(|m| m.modified())
                                    .map_err(|_| Error::Unavailable)?,
                            ),
                        )
                        .map_err(|_| Error::Unavailable)?;
                    output
                        .flush()
                        .and_then(|_| output.sync_all())
                        .map_err(|_| Error::Unavailable)?;
                    // The scope owner serializes boot preparation; preserve an
                    // unexpected existing target instead of overwriting it.
                    fs::hard_link(&temp, &executable).map_err(|_| Error::Conflict)?;
                    fs::remove_file(&temp).map_err(|_| Error::Unavailable)?;
                    File::open(&bin)
                        .and_then(|f| f.sync_all())
                        .map_err(|_| Error::Unavailable)
                })();
                let _ = fs::remove_file(&temp);
                result?;
            }
            Err(_) => return Err(Error::Unavailable),
        }
        let directories = Self::paths(&home, id)
            .iter()
            .enumerate()
            .map(|(index, path)| Directory::inspect(path, index == 0))
            .collect::<Result<Vec<_>>>()?;
        let mut executable_file = private_options()
            .read(true)
            .open(&executable)
            .map_err(|_| Error::Unavailable)?;
        let image = Image::of(&executable_file)?;
        if hash_file(&mut executable_file)? != hash || Image::of(&executable_file)? != image {
            return Err(Error::Unavailable);
        }
        Ok(Self {
            home,
            box_id: id.into(),
            directories,
            image,
            sha256: hash,
        })
    }

    fn paths(home: &Path, id: &str) -> [PathBuf; 4] {
        [
            home.to_owned(),
            home.join("boxes"),
            home.join("boxes").join(id),
            home.join("boxes").join(id).join("bin"),
        ]
    }
    fn path(&self) -> PathBuf {
        self.home
            .join("boxes")
            .join(&self.box_id)
            .join("bin/boxlite-shim")
    }
    pub fn verify(&self) -> Result<()> {
        self.validate_metadata(&self.box_id)?;
        if fs::canonicalize(&self.home).map_err(|_| Error::Unavailable)? != self.home {
            return Err(Error::Conflict);
        }
        let current = Self::paths(&self.home, &self.box_id)
            .iter()
            .enumerate()
            .map(|(index, path)| Directory::inspect(path, index == 0))
            .collect::<Result<Vec<_>>>()?;
        if current != self.directories {
            return Err(Error::Unavailable);
        }
        let file = private_options()
            .read(true)
            .open(self.path())
            .map_err(|_| Error::Unavailable)?;
        if Image::of(&file)? != self.image {
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    pub(crate) fn validate_metadata(&self, id: &str) -> Result<()> {
        if self.box_id != id
            || !valid_id(id)
            || !self.home.is_absolute()
            || self.directories.len() != 4
            || self
                .directories
                .iter()
                .any(|directory| directory.inode == 0)
            || self.image.inode == 0
            || self.image.size == 0
            || self.image.size > MAX_EXECUTABLE
            || !(0..1_000_000_000).contains(&self.image.modified_ns)
            || !(0..1_000_000_000).contains(&self.image.changed_ns)
            || self.sha256.len() != 64
            || !self
                .sha256
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
        {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    pub(crate) fn validate_home(&self, home: &Path, id: &str) -> Result<()> {
        self.validate_metadata(id)?;
        if self.home != home {
            return Err(Error::Conflict);
        }
        Ok(())
    }

    /// Positive evidence only. An empty result is never proof that a start
    /// did not happen or that pending native tasks cannot create writers later.
    pub fn discover(&self) -> Result<Vec<HostProcess>> {
        self.verify()?;
        let mut found = Vec::new();
        for pid in candidate_pids()? {
            let mut process = match HostProcess::capture(pid) {
                Ok(value) => value,
                Err(_) => continue,
            };
            if process.bind_writer_group(&self.box_id).is_err() {
                continue;
            }
            if executable_of(&process).is_ok_and(|path| path == self.path())
                && process.verify_current_writer(&self.box_id).is_ok()
            {
                found.push(process);
            }
        }
        self.verify()?;
        Ok(found)
    }
}
fn hash_file(file: &mut File) -> Result<String> {
    let mut hash = Sha256::new();
    let mut buffer = [0u8; 64 << 10];
    let mut total = 0u64;
    loop {
        let n = file.read(&mut buffer).map_err(|_| Error::Unavailable)?;
        if n == 0 {
            break;
        }
        total += n as u64;
        if total > MAX_EXECUTABLE {
            return Err(Error::Unavailable);
        }
        hash.update(&buffer[..n]);
    }
    Ok(format!("{:x}", hash.finalize()))
}
#[cfg(target_os = "macos")]
fn candidate_pids() -> Result<Vec<u32>> {
    let mut pids = vec![0i32; MAX_PROCESSES + 1];
    // SAFETY: the buffer has the supplied byte capacity; PROC_UID_ONLY is the
    // public libproc selector 4. The query does not inspect process environments.
    let bytes = unsafe {
        libc::proc_listpids(
            4,
            libc::geteuid(),
            pids.as_mut_ptr().cast(),
            std::mem::size_of_val(pids.as_slice()) as i32,
        )
    };
    if bytes <= 0
        || bytes as usize >= std::mem::size_of_val(pids.as_slice())
        || !(bytes as usize).is_multiple_of(std::mem::size_of::<i32>())
    {
        return Err(Error::Unavailable);
    }
    pids.truncate(bytes as usize / std::mem::size_of::<i32>());
    Ok(pids
        .into_iter()
        .filter(|pid| *pid > 1)
        .map(|pid| pid as u32)
        .collect())
}
#[cfg(target_os = "macos")]
fn executable_of(process: &HostProcess) -> Result<PathBuf> {
    super::host_signal::executable(
        process.pid,
        process.signal_version.ok_or(Error::Unavailable)?,
    )
}
#[cfg(target_os = "linux")]
fn candidate_pids() -> Result<Vec<u32>> {
    let mut pids = Vec::new();
    // SAFETY: geteuid only queries process identity.
    let uid = unsafe { libc::geteuid() };
    for entry in fs::read_dir("/proc").map_err(|_| Error::Unavailable)? {
        let entry = entry.map_err(|_| Error::Unavailable)?;
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|value| value.parse::<u32>().ok())
            .filter(|pid| *pid > 1)
        else {
            continue;
        };
        if entry.metadata().is_ok_and(|m| m.uid() == uid) {
            pids.push(pid);
            if pids.len() > MAX_PROCESSES {
                return Err(Error::Unavailable);
            }
        }
    }
    Ok(pids)
}
#[cfg(target_os = "linux")]
fn executable_of(process: &HostProcess) -> Result<PathBuf> {
    fs::read_link(format!("/proc/{}/exe", process.pid)).map_err(|_| Error::Unavailable)
}
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
fn candidate_pids() -> Result<Vec<u32>> {
    Err(Error::Unavailable)
}
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
fn executable_of(_process: &HostProcess) -> Result<PathBuf> {
    Err(Error::Unavailable)
}

#[cfg(test)]
#[path = "boot_executable_tests.rs"]
mod tests;
