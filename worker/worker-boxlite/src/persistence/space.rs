//! Read capacity from a checked private directory without following a replaced
//! root. This is an admission estimate, not a filesystem/project quota.
use std::fs::OpenOptions;
use std::os::fd::AsRawFd;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::Path;

pub(super) fn available(path: &Path) -> std::io::Result<u64> {
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)?;
    let meta = file.metadata()?;
    // SAFETY: geteuid reads process identity.
    if !meta.is_dir() || meta.mode() & 0o077 != 0 || meta.uid() != unsafe { libc::geteuid() } {
        return Err(std::io::Error::other(
            "private staging directory unavailable",
        ));
    }
    let mut stats = std::mem::MaybeUninit::<libc::statvfs>::uninit();
    // SAFETY: the directory FD is live and stats points to writable storage.
    if unsafe { libc::fstatvfs(file.as_raw_fd(), stats.as_mut_ptr()) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: successful fstatvfs initialized stats.
    let stats = unsafe { stats.assume_init() };
    let block = if stats.f_frsize == 0 {
        stats.f_bsize
    } else {
        stats.f_frsize
    };
    Ok(u64::try_from(u128::from(stats.f_bavail) * u128::from(block)).unwrap_or(u64::MAX))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{symlink, PermissionsExt};
    #[test]
    fn staging_capacity_rejects_redirected_or_public_roots() {
        let root = std::env::temp_dir().join(format!("onlyboxes-space-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir(&root).unwrap();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        let owned = root.join("owned");
        std::fs::create_dir(&owned).unwrap();
        std::fs::set_permissions(&owned, std::fs::Permissions::from_mode(0o700)).unwrap();
        assert!(available(&owned).is_ok());
        let alias = root.join("redirect");
        symlink(&owned, &alias).unwrap();
        assert!(available(&alias).is_err());
        std::fs::set_permissions(&owned, std::fs::Permissions::from_mode(0o755)).unwrap();
        assert!(available(&owned).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }
}
