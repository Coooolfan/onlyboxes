//! Only pre-boot SDK preparation bytes may be reclaimed as an abandoned create.
use super::*;
use std::io::{Read, Seek, SeekFrom};

pub(crate) fn validate_unstarted_directory(home: &Path, id: &str) -> Result<()> {
    if !valid_id(id) {
        return Err(Error::Conflict);
    }
    let root = home.join("boxes").join(id);
    match fs::symlink_metadata(&root) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(Error::Unavailable),
        Ok(_) => (),
    }
    Directory::inspect(&root, true)?;
    let entries = fs::read_dir(&root)
        .map_err(|_| Error::Unavailable)?
        .take(2)
        .collect::<std::io::Result<Vec<_>>>()
        .map_err(|_| Error::Unavailable)?;
    if entries.is_empty() {
        return Ok(());
    }
    if entries.len() != 1 || entries[0].file_name() != "bin" {
        return Err(Error::Conflict);
    }
    Directory::inspect(&root.join("bin"), true)?;
    if fs::read_dir(root.join("bin"))
        .map_err(|_| Error::Unavailable)?
        .next()
        .is_none()
    {
        return Ok(());
    }
    let source = boxlite::util::find_binary("boxlite-shim").map_err(|_| Error::Unavailable)?;
    validate_with_source(home, id, &source)
}

fn stamp(file: &File) -> Result<(u64, u64, u64, u32, u64, i64, i64, i64, i64)> {
    let meta = file.metadata().map_err(|_| Error::Unavailable)?;
    if !meta.is_file() || meta.uid() != unsafe { libc::geteuid() } || meta.mode() & 0o7077 != 0 {
        return Err(Error::Conflict);
    }
    Ok((
        meta.dev(),
        meta.ino(),
        meta.len(),
        meta.mode(),
        meta.nlink(),
        meta.mtime(),
        meta.mtime_nsec(),
        meta.ctime(),
        meta.ctime_nsec(),
    ))
}

fn validate_with_source(home: &Path, id: &str, source: &Path) -> Result<()> {
    if !valid_id(id) {
        return Err(Error::Conflict);
    }
    let root = home.join("boxes").join(id);
    Directory::inspect(&root, true)?;
    let roots = fs::read_dir(&root)
        .map_err(|_| Error::Unavailable)?
        .take(2)
        .collect::<std::io::Result<Vec<_>>>()
        .map_err(|_| Error::Unavailable)?;
    if roots.len() != 1 || roots[0].file_name() != "bin" {
        return Err(Error::Conflict);
    }
    let bin = root.join("bin");
    Directory::inspect(&bin, true)?;
    let entries = fs::read_dir(&bin)
        .map_err(|_| Error::Unavailable)?
        .take(3)
        .collect::<std::io::Result<Vec<_>>>()
        .map_err(|_| Error::Unavailable)?;
    if entries.len() > 2 {
        return Err(Error::Conflict);
    }
    let mut targets = Vec::new();
    for entry in entries {
        let name = entry
            .file_name()
            .into_string()
            .map_err(|_| Error::Conflict)?;
        let complete = name == "boxlite-shim";
        if !complete {
            let value = name
                .strip_prefix("shim-")
                .and_then(|value| value.strip_suffix(".pending"))
                .ok_or(Error::Conflict)?;
            let uuid = uuid::Uuid::parse_str(value).map_err(|_| Error::Conflict)?;
            if uuid.is_nil() || uuid.to_string() != value || targets.iter().any(|(c, _, _)| !c) {
                return Err(Error::Conflict);
            }
        }
        let file = private_options()
            .read(true)
            .open(entry.path())
            .map_err(|_| Error::Conflict)?;
        let info = stamp(&file)?;
        let mode = info.3 & 0o777;
        if (complete && mode != 0o500)
            || (!complete && !matches!(mode, 0o500 | 0o600))
            || !(1..=2).contains(&info.4)
        {
            return Err(Error::Conflict);
        }
        targets.push((complete, file, info));
    }
    let source = fs::canonicalize(source).map_err(|_| Error::Unavailable)?;
    let mut trusted = private_options()
        .read(true)
        .open(source)
        .map_err(|_| Error::Unavailable)?;
    let original = Image::of(&trusted)?;
    for (complete, file, info) in &mut targets {
        if info.2 > original.size || (*complete && info.2 != original.size) {
            return Err(Error::Conflict);
        }
        trusted
            .seek(SeekFrom::Start(0))
            .map_err(|_| Error::Unavailable)?;
        let mut remaining = info.2;
        let mut expected = vec![0; 1 << 20];
        let mut actual = vec![0; 1 << 20];
        while remaining > 0 {
            let count = remaining.min(expected.len() as u64) as usize;
            trusted
                .read_exact(&mut expected[..count])
                .map_err(|_| Error::Unavailable)?;
            file.read_exact(&mut actual[..count])
                .map_err(|_| Error::Unavailable)?;
            if expected[..count] != actual[..count] {
                return Err(Error::Conflict);
            }
            remaining -= count as u64;
        }
        if stamp(file)? != *info || Image::of(&trusted)? != original {
            return Err(Error::Conflict);
        }
    }
    for (_, _, info) in &targets {
        if info.4 == 2
            && (targets.len() != 2
                || targets
                    .iter()
                    .any(|(_, _, other)| other.0 != info.0 || other.1 != info.1 || other.4 != 2))
        {
            return Err(Error::Conflict);
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    struct Fixture {
        root: PathBuf,
        home: PathBuf,
        bin: PathBuf,
    }
    impl Fixture {
        fn new() -> Self {
            let root = std::env::temp_dir()
                .join(format!("onlyboxes-unbooted-files-{}", uuid::Uuid::new_v4()));
            let home = root.join("home");
            let bin = home.join("boxes/initial/bin");
            fs::create_dir_all(&bin).unwrap();
            for path in [
                &root,
                &home,
                &home.join("boxes"),
                &home.join("boxes/initial"),
                &bin,
            ] {
                fs::set_permissions(path, fs::Permissions::from_mode(0o700)).unwrap();
            }
            Self { root, home, bin }
        }
        fn write(&self, name: &str, bytes: &[u8], mode: u32) -> PathBuf {
            let path = self.bin.join(name);
            fs::write(&path, bytes).unwrap();
            fs::set_permissions(&path, fs::Permissions::from_mode(mode)).unwrap();
            path
        }
        fn verify(&self) -> Result<()> {
            validate_with_source(&self.home, "initial", Path::new("/bin/sleep"))
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.root);
        }
    }
    #[test]
    fn unstarted_files_accept_only_empty_roots_and_trusted_complete_or_partial_preparation() {
        let f = Fixture::new();
        validate_unstarted_directory(&f.home, "initial").unwrap();
        let bytes = fs::read("/bin/sleep").unwrap();
        let complete = f.write("boxlite-shim", &bytes, 0o500);
        f.verify().unwrap();
        let partial = format!("shim-{}.pending", uuid::Uuid::new_v4());
        fs::hard_link(&complete, f.bin.join(&partial)).unwrap();
        f.verify().unwrap();
        fs::remove_file(&complete).unwrap();
        fs::remove_file(f.bin.join(&partial)).unwrap();
        let partial = f.write(&partial, &bytes[..bytes.len() / 2], 0o600);
        f.verify().unwrap();
        fs::write(&partial, b"untrusted-data").unwrap();
        assert_eq!(f.verify(), Err(Error::Conflict));
        assert_eq!(fs::read(&partial).unwrap(), b"untrusted-data");
    }
    #[test]
    fn unstarted_files_reject_guest_data_links_and_unrecognized_files_without_deleting_anything() {
        let f = Fixture::new();
        let data = f.home.join("boxes/initial/disks");
        fs::create_dir(&data).unwrap();
        fs::write(data.join("disk.qcow2"), b"unique-data").unwrap();
        assert!(validate_unstarted_directory(&f.home, "initial").is_err());
        assert_eq!(fs::read(data.join("disk.qcow2")).unwrap(), b"unique-data");
        fs::remove_dir_all(data).unwrap();
        let complete = f.write("boxlite-shim", &fs::read("/bin/sleep").unwrap(), 0o500);
        fs::hard_link(&complete, f.root.join("outside-link")).unwrap();
        assert!(f.verify().is_err());
        fs::remove_file(f.root.join("outside-link")).unwrap();
        f.write("unexpected", b"unique-data", 0o600);
        assert!(f.verify().is_err());
        fs::remove_file(f.bin.join("unexpected")).unwrap();
        fs::remove_file(complete).unwrap();
        let outside = f.root.join("outside");
        fs::create_dir(&outside).unwrap();
        fs::write(outside.join("keep"), b"unique-data").unwrap();
        fs::remove_dir(&f.bin).unwrap();
        symlink(&outside, &f.bin).unwrap();
        assert!(validate_unstarted_directory(&f.home, "initial").is_err());
        assert_eq!(fs::read(outside.join("keep")).unwrap(), b"unique-data");
    }
}
