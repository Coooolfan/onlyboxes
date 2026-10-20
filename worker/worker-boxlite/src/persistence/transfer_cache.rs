//! Cache copies use independent inodes and the downloader's publication lock.
use super::*;

impl Transfer {
    /// Caller proves that source belongs to a verified shared-snapshot cache.
    /// Both files remain locked through actual blocking I/O, including if the
    /// awaiting future is dropped. No network grant is used on this path.
    pub async fn copy_cached_archive(
        &self,
        source: PathBuf,
        archive: &Archive,
        destination: PathBuf,
        cancel: CancellationToken,
    ) -> Result<Downloaded> {
        archive.validate().map_err(|_| TransferError::Integrity)?;
        if source == destination {
            return Err(TransferError::Integrity);
        }
        let token = cancel.child_token();
        let _guard = CancelOnDrop(token.clone());
        let expected = archive.clone();
        self.io_task(move || {
            if token.is_cancelled() {
                return Err(TransferError::Retry);
            }
            let cached = open_cached(source, &expected)?.ok_or(TransferError::Local)?;
            let identity_before = verify_file(&cached.file, &expected, &token)?;
            if token.is_cancelled() {
                return Err(TransferError::Retry);
            }
            let local = prepare(destination, &expected)?;
            if local
                .file
                .metadata()
                .map_err(|_| TransferError::Local)?
                .ino()
                == identity_before.ino
                && local
                    .file
                    .metadata()
                    .map_err(|_| TransferError::Local)?
                    .dev()
                    == identity_before.dev
            {
                return Err(TransferError::Integrity);
            }
            if !local.cached {
                local.file.set_len(0).map_err(|_| TransferError::Local)?;
                let mut data = vec![0; CHUNK];
                let mut offset = 0;
                while offset < expected.size_bytes as u64 {
                    if token.is_cancelled() {
                        return Err(TransferError::Retry);
                    }
                    let count = cached
                        .file
                        .read_at(
                            &mut data
                                [..((expected.size_bytes as u64 - offset) as usize).min(CHUNK)],
                            offset,
                        )
                        .map_err(|_| TransferError::Local)?;
                    if count == 0 {
                        return Err(TransferError::Integrity);
                    }
                    local
                        .file
                        .write_all_at(&data[..count], offset)
                        .map_err(|_| TransferError::Local)?;
                    offset += count as u64;
                }
                if identity(&cached.file)? != identity_before {
                    discard_partial(&local)?;
                    return Err(TransferError::Integrity);
                }
            }
            if let Err(error) = verify_file(&local.file, &expected, &token) {
                if error == TransferError::Integrity {
                    discard_corrupt(&local)?;
                }
                return Err(error);
            }
            if token.is_cancelled() {
                return Err(TransferError::Retry);
            }
            local.file.sync_all().map_err(|_| TransferError::Local)?;
            if !local.cached {
                fs::hard_link(&local.partial, &local.destination)
                    .map_err(|_| TransferError::Local)?;
                sync_parent(&local.destination)?;
            }
            cleanup_aliases(&local)?;
            // Persist LRU access even on noatime filesystems. Only the optional
            // source cache is touched; archive bytes and mtime stay immutable.
            cached
                .file
                .set_times(std::fs::FileTimes::new().set_accessed(std::time::SystemTime::now()))
                .map_err(|_| TransferError::Local)?;
            let file = private_file(
                &local.destination,
                false,
                false,
                expected.size_bytes as u64,
                1,
            )?;
            Ok(Downloaded {
                file,
                reused: true,
                resumed_from: expected.size_bytes,
            })
        })
        .await
    }

    /// Optional cache files only. The caller excludes held operations and
    /// uncommitted sources. Busy locks preserve the file for a later sweep.
    pub async fn evict_cached_archive(
        &self,
        path: PathBuf,
        archive: &Archive,
    ) -> Result<Option<u64>> {
        archive.validate().map_err(|_| TransferError::Integrity)?;
        let expected = archive.clone();
        self.io_task(move || {
            let Some(local) = open_cached(path, &expected)? else {
                return Ok(None);
            };
            let bytes = local
                .file
                .metadata()
                .map_err(|_| TransferError::Local)?
                .len();
            discard_corrupt(&local)?;
            Ok(Some(bytes))
        })
        .await
    }
}

fn open_cached(destination: PathBuf, archive: &Archive) -> Result<Option<LocalFile>> {
    if !destination.is_absolute() {
        return Err(TransferError::Integrity);
    }
    let parent = destination.parent().ok_or(TransferError::Integrity)?;
    let name = destination
        .file_name()
        .and_then(|v| v.to_str())
        .filter(|v| !v.is_empty() && v.len() <= 160 && !v.contains(['/', '\\']))
        .ok_or(TransferError::Integrity)?;
    let meta = fs::symlink_metadata(parent).map_err(|_| TransferError::Local)?;
    // SAFETY: geteuid reads process identity.
    if !meta.is_dir() || meta.mode() & 0o077 != 0 || meta.uid() != unsafe { libc::geteuid() } {
        return Err(TransferError::Local);
    }
    let lock = private_file(&parent.join(format!("{name}.lock")), true, true, 0, 1)?;
    lock.try_lock().map_err(|_| TransferError::Retry)?;
    match fs::symlink_metadata(&destination) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(TransferError::Local),
        Ok(_) => (),
    }
    let partial = parent.join(format!("{name}.partial"));
    let marker = parent.join(format!("{name}.partial.json"));
    let saved = read_marker(&marker)?;
    if saved.as_ref().is_some_and(|saved| saved != archive) {
        return Err(TransferError::Integrity);
    }
    let file = private_file(&destination, false, false, u64::MAX, 2)?;
    if fs::symlink_metadata(&partial).is_ok() && !same_file(&file, &partial)? {
        return Err(TransferError::Integrity);
    }
    if file.metadata().map_err(|_| TransferError::Local)?.nlink() == 2
        && (saved.as_ref() != Some(archive) || !same_file(&file, &partial)?)
    {
        return Err(TransferError::Integrity);
    }
    Ok(Some(LocalFile {
        file,
        _lock: lock,
        destination,
        partial,
        marker,
        cached: true,
    }))
}
