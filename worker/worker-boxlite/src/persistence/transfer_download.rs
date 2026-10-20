use super::*;

struct LocalFile {
    file: File,
    _lock: File,
    destination: PathBuf,
    partial: PathBuf,
    marker: PathBuf,
    cached: bool,
}

impl Transfer {
    /// Destination is an operation-bound path in a private, owned directory.
    /// Partial bytes never become a published archive before complete SHA-256.
    pub async fn download(
        &self,
        grant: &TransferGrant,
        archive: &Archive,
        destination: PathBuf,
        cancel: CancellationToken,
    ) -> Result<Downloaded> {
        full_grant(grant, archive)?;
        if cancel.is_cancelled() {
            return Err(TransferError::Retry);
        }
        let token = cancel.child_token();
        let _guard = CancelOnDrop(token.clone());
        let expected = archive.clone();
        let local = self
            .io_task(move || prepare(destination, &expected))
            .await?;
        let local = Arc::new(local);
        let offset = local
            .file
            .metadata()
            .map_err(|_| TransferError::Local)?
            .len();
        let mut resumed_from = offset;
        if !local.cached && offset < archive.size_bytes as u64 {
            let context = local.clone();
            let result = self
                .exchange(grant, &token, |progress, child| async move {
                    let mut request = self
                        .client
                        .get(&grant.url)
                        .headers(checked_grant(grant, "GET")?);
                    if offset > 0 {
                        request = request.header(RANGE, format!("bytes={offset}-"));
                    }
                    let mut response = request.send().await.map_err(|_| TransferError::Retry)?;
                    check_encoding(&response)?;
                    let start = match response.status() {
                        StatusCode::OK => 0,
                        StatusCode::PARTIAL_CONTENT if offset > 0 => {
                            if response
                                .headers()
                                .get("content-range")
                                .and_then(|v| v.to_str().ok())
                                != Some(
                                    format!(
                                        "bytes {}-{}/{}",
                                        offset,
                                        archive.size_bytes - 1,
                                        archive.size_bytes
                                    )
                                    .as_str(),
                                )
                            {
                                return Err(TransferError::Integrity);
                            }
                            offset
                        }
                        StatusCode::PARTIAL_CONTENT => return Err(TransferError::Integrity),
                        _ => return Err(TransferError::Retry),
                    };
                    if (response.status() == StatusCode::OK
                        && response.headers().contains_key("content-range"))
                        || response
                            .content_length()
                            .is_some_and(|n| n != archive.size_bytes as u64 - start)
                    {
                        return Err(TransferError::Integrity);
                    }
                    if start == 0 {
                        let context = context.clone();
                        self.io_task(move || {
                            context.file.set_len(0).map_err(|_| TransferError::Local)
                        })
                        .await?;
                    }
                    let mut position = start;
                    while let Some(chunk) =
                        response.chunk().await.map_err(|_| TransferError::Retry)?
                    {
                        if chunk.len() as u64 > archive.size_bytes as u64 - position {
                            return Err(TransferError::Integrity);
                        }
                        let _ = progress.send(Instant::now());
                        for data in chunk.chunks(CHUNK) {
                            if child.is_cancelled() {
                                return Err(TransferError::Retry);
                            }
                            let data = data.to_vec();
                            let count = data.len();
                            let captured = context.clone();
                            self.io_task(move || {
                                captured
                                    .file
                                    .write_all_at(&data, position)
                                    .map_err(|_| TransferError::Local)
                            })
                            .await?;
                            position += count as u64;
                        }
                    }
                    if position != archive.size_bytes as u64 {
                        return Err(TransferError::Retry);
                    }
                    Ok(start)
                })
                .await;
            if let Ok(start) = result {
                resumed_from = start;
            } else if let Err(error) = result {
                if error == TransferError::Integrity {
                    let captured = local.clone();
                    self.io_task(move || discard_partial(&captured)).await?;
                }
                return Err(error);
            }
        }
        let captured = local.clone();
        let expected = archive.clone();
        let child = token.clone();
        let verify = self.io_task(move || verify_file(&captured.file, &expected, &child));
        let verified = tokio::select! {
            biased;
            _ = token.cancelled() => return Err(TransferError::Retry),
            result = verify => result,
        };
        if let Err(error) = verified {
            if error == TransferError::Integrity {
                let captured = local.clone();
                self.io_task(move || discard_corrupt(&captured)).await?;
            }
            return Err(error);
        }
        let captured = local.clone();
        let child = token.clone();
        let file = self
            .io_task(move || {
                if child.is_cancelled() {
                    return Err(TransferError::Retry);
                }
                captured.file.sync_all().map_err(|_| TransferError::Local)?;
                if !captured.cached {
                    match fs::hard_link(&captured.partial, &captured.destination) {
                        Ok(()) => {}
                        Err(_) => return Err(TransferError::Local),
                    }
                    sync_parent(&captured.destination)?;
                }
                cleanup_aliases(&captured)?;
                private_file(&captured.destination, false, false, u64::MAX, 1)
            })
            .await?;
        Ok(Downloaded {
            file,
            reused: local.cached,
            resumed_from: resumed_from as i64,
        })
    }
}

fn prepare(destination: PathBuf, expected: &Archive) -> Result<LocalFile> {
    if !destination.is_absolute() {
        return Err(TransferError::Integrity);
    }
    let parent = destination.parent().ok_or(TransferError::Integrity)?;
    let name = destination
        .file_name()
        .and_then(|s| s.to_str())
        .filter(|s| !s.is_empty() && s.len() <= 160 && !s.contains(['/', '\\']))
        .ok_or(TransferError::Integrity)?;
    let meta = fs::symlink_metadata(parent).map_err(|_| TransferError::Local)?;
    // SAFETY: geteuid reads the current process identity.
    if !meta.is_dir() || meta.mode() & 0o077 != 0 || meta.uid() != unsafe { libc::geteuid() } {
        return Err(TransferError::Local);
    }
    let partial = parent.join(format!("{name}.partial"));
    let marker = parent.join(format!("{name}.partial.json"));
    let lock = private_file(&parent.join(format!("{name}.lock")), true, true, 0, 1)?;
    lock.try_lock().map_err(|_| TransferError::Retry)?;
    let saved = read_marker(&marker)?;
    if fs::symlink_metadata(&destination).is_ok() {
        if saved.as_ref().is_some_and(|saved| saved != expected) {
            return Err(TransferError::Integrity);
        }
        let file = private_file(&destination, false, false, expected.size_bytes as u64, 2)?;
        if fs::symlink_metadata(&partial).is_ok() && !same_file(&file, &partial)? {
            return Err(TransferError::Integrity);
        }
        if file.metadata().map_err(|_| TransferError::Local)?.nlink() == 2
            && (saved.as_ref() != Some(expected) || !same_file(&file, &partial)?)
        {
            return Err(TransferError::Integrity);
        }
        return Ok(LocalFile {
            file,
            _lock: lock,
            destination,
            partial,
            marker,
            cached: true,
        });
    }
    if let Some(saved) = saved {
        if &saved != expected {
            return Err(TransferError::Integrity);
        }
    } else {
        if fs::symlink_metadata(&partial).is_ok() {
            return Err(TransferError::Integrity);
        }
        write_marker(&marker, expected)?;
    }
    let file = private_file(&partial, true, true, expected.size_bytes as u64, 1)?;
    Ok(LocalFile {
        file,
        _lock: lock,
        destination,
        partial,
        marker,
        cached: false,
    })
}
fn private_file(path: &Path, write: bool, create: bool, limit: u64, links: u64) -> Result<File> {
    let file = OpenOptions::new()
        .read(true)
        .write(write)
        .create(create)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| TransferError::Local)?;
    let meta = file.metadata().map_err(|_| TransferError::Local)?;
    // SAFETY: geteuid reads the current process identity.
    if !meta.is_file()
        || meta.mode() & 0o077 != 0
        || meta.uid() != unsafe { libc::geteuid() }
        || meta.nlink() > links
        || meta.len() > limit
    {
        return Err(TransferError::Integrity);
    }
    Ok(file)
}
fn read_marker(path: &Path) -> Result<Option<Archive>> {
    match fs::symlink_metadata(path) {
        Ok(_) => {}
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(TransferError::Local),
    }
    let mut file = private_file(path, false, false, 4096, 1)?;
    let mut data = Vec::new();
    std::io::Read::by_ref(&mut file)
        .take(4097)
        .read_to_end(&mut data)
        .map_err(|_| TransferError::Local)?;
    let archive: Archive = serde_json::from_slice(&data).map_err(|_| TransferError::Integrity)?;
    archive.validate().map_err(|_| TransferError::Integrity)?;
    Ok(Some(archive))
}
fn write_marker(path: &Path, archive: &Archive) -> Result<()> {
    let temp = path.with_extension(format!("{}.tmp", uuid::Uuid::new_v4()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&temp)
            .map_err(|_| TransferError::Local)?;
        file.write_all(&serde_json::to_vec(archive).map_err(|_| TransferError::Integrity)?)
            .map_err(|_| TransferError::Local)?;
        file.sync_all().map_err(|_| TransferError::Local)?;
        drop(file);
        fs::rename(&temp, path).map_err(|_| TransferError::Local)?;
        sync_parent(path)
    })();
    let _ = fs::remove_file(temp);
    result
}
fn same_file(file: &File, path: &Path) -> Result<bool> {
    let meta = match fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err(TransferError::Local),
    };
    let original = file.metadata().map_err(|_| TransferError::Local)?;
    Ok(meta.is_file() && meta.dev() == original.dev() && meta.ino() == original.ino())
}
fn cleanup_aliases(local: &LocalFile) -> Result<()> {
    if same_file(&local.file, &local.partial)? {
        fs::remove_file(&local.partial).map_err(|_| TransferError::Local)?;
    }
    if local
        .marker
        .try_exists()
        .map_err(|_| TransferError::Local)?
    {
        fs::remove_file(&local.marker).map_err(|_| TransferError::Local)?;
    }
    sync_parent(&local.destination)
}
fn discard_partial(local: &LocalFile) -> Result<()> {
    if same_file(&local.file, &local.partial)? {
        fs::remove_file(&local.partial).map_err(|_| TransferError::Local)?;
        fs::remove_file(&local.marker).map_err(|_| TransferError::Local)?;
        sync_parent(&local.destination)?;
    }
    Ok(())
}
fn discard_corrupt(local: &LocalFile) -> Result<()> {
    if local.cached {
        if !same_file(&local.file, &local.destination)? {
            return Err(TransferError::Integrity);
        }
        fs::remove_file(&local.destination).map_err(|_| TransferError::Local)?;
        cleanup_aliases(local)
    } else {
        discard_partial(local)
    }
}
fn sync_parent(path: &Path) -> Result<()> {
    File::open(path.parent().ok_or(TransferError::Integrity)?)
        .and_then(|file| file.sync_all())
        .map_err(|_| TransferError::Local)
}

#[cfg(test)]
#[path = "transfer_tests.rs"]
mod tests;

#[path = "transfer_cache.rs"]
mod cache;
