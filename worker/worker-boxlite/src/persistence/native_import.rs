//! Native imports remain stopped. Readiness, leases and activation belong to the
//! lifecycle controller; this helper never opens execution or proxy admission.
use super::export::{archive_error, blocking, Workspace};
use super::*;
use crate::persistence::archive::prepare_import_with_identity;
use crate::persistence::{ArchiveLimits, Operation};
use std::fs::{File, OpenOptions};
use std::io::Read;
use std::os::unix::fs::OpenOptionsExt;

impl NativeRuntime {
    /// Caller reserves both SDK and journal staging space, serializes this scope
    /// and retains the task until SDK I/O finishes, including after cancellation.
    /// No retry replaces a selected native ID or starts an existing candidate.
    pub async fn import_target(
        &self,
        journal: Arc<Journal>,
        operation: &str,
        limits: ArchiveLimits,
    ) -> Result<NativeInstance> {
        limits.validate().map_err(archive_error)?;
        let op = target_operation(&journal, operation)?;
        if !op.box_id.is_empty() {
            if op.phase == Phase::Prepared {
                return Err(NativeError::Conflict);
            }
            let native = self.inspect_target(&journal, operation).await?;
            if native.status == BoxStatus::Running && op.lease_expires_unix_ms <= now_ms()? {
                return Err(NativeError::Conflict);
            }
            return Ok(native);
        }
        if !matches!(op.phase, Phase::Prepared | Phase::Importing) {
            return Err(NativeError::Conflict);
        }
        let spec = op
            .restore_spec
            .as_ref()
            .ok_or(NativeError::Conflict)?
            .clone();
        let archive = op.archive.as_ref().ok_or(NativeError::Conflict)?.clone();
        if archive.size_bytes > limits.max_bytes
            || archive.expanded_bytes > limits.max_expanded_bytes
        {
            return Err(NativeError::Budget);
        }
        let required = limits
            .max_bytes
            .checked_add(limits.max_expanded_bytes)
            .ok_or(NativeError::Budget)?;
        if op.staging_bytes < required {
            return Err(NativeError::Budget);
        }
        let path = journal
            .artifact_path(&op.scoped_session_id, operation, op.generation)
            .map_err(|_| NativeError::Unavailable)?;
        let workspace = Workspace::new(
            path.parent().ok_or(NativeError::Conflict)?,
            path.file_stem()
                .and_then(|s| s.to_str())
                .ok_or(NativeError::Conflict)?,
        )?;
        let staged = workspace.0.join("import.boxlite");
        let output = staged.clone();
        let scope = op.scoped_session_id.clone();
        let expected = spec.clone();
        let (disk_size, disk_hash) = blocking(move || {
            let mut input = private_archive(&path)?;
            let mut output = OpenOptions::new()
                .read(true)
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(output)
                .map_err(|_| NativeError::Unavailable)?;
            let (_, size, hash) = prepare_import_with_identity(
                &mut input,
                &mut output,
                &expected,
                &scope,
                &archive,
                limits,
            )
            .map_err(archive_error)?;
            Ok((size, hash))
        })
        .await?;
        // This durable transition precedes SDK extraction or any native import.
        journal
            .update(operation, |current| {
                if !matches!(current.phase, Phase::Prepared | Phase::Importing) {
                    return Err(crate::persistence::Error::Conflict);
                }
                current.verified = true;
                current.phase = Phase::Importing;
                Ok(())
            })
            .map_err(journal_error)?;
        let name = target_name(&op.scoped_session_id, operation, op.generation);
        let candidate = match self
            .runtime
            .get(&name)
            .await
            .map_err(|_| NativeError::Unavailable)?
        {
            Some(native) => native,
            None => match self
                .runtime
                .import_box(boxlite::BoxArchive::new(&staged), Some(name.clone()))
                .await
            {
                Ok(native) => native,
                // A concurrent retry or a lost SDK acknowledgement may already
                // have installed this exact operation name. Never select by the
                // stable session name or fall back to a new blank instance.
                Err(_) => self
                    .runtime
                    .get(&name)
                    .await
                    .map_err(|_| NativeError::Unavailable)?
                    .ok_or(NativeError::Unavailable)?,
            },
        };
        let id = candidate.id().as_str().to_owned();
        let inspected = self
            .inspect_known(&op.scoped_session_id, &id, &name, Some(&spec))
            .await?;
        if inspected.status != BoxStatus::Stopped {
            return Err(NativeError::Conflict);
        }
        let home = self.home.clone();
        let selected = id.clone();
        blocking(move || verify_unselected_disk(&home, &selected, disk_size, &disk_hash)).await?;
        journal
            .update(operation, |current| {
                if current.phase != Phase::Importing || !current.verified {
                    return Err(crate::persistence::Error::Conflict);
                }
                current.box_id = id;
                Ok(())
            })
            .map_err(journal_error)?;
        let selected = self.inspect_target(&journal, operation).await?;
        if selected.status != BoxStatus::Stopped {
            return Err(NativeError::Conflict);
        }
        Ok(selected)
    }
}

fn target_operation(journal: &Journal, operation: &str) -> Result<Operation> {
    let state = journal.snapshot().map_err(journal_error)?;
    let op = state
        .operations
        .get(operation)
        .ok_or(NativeError::Conflict)?;
    if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
        || op.kind != Kind::Target
        || !matches!(
            op.phase,
            Phase::Prepared | Phase::Importing | Phase::Ready | Phase::Active
        )
    {
        return Err(NativeError::Conflict);
    }
    Ok(op.clone())
}
fn journal_error(error: crate::persistence::Error) -> NativeError {
    match error {
        crate::persistence::Error::Conflict => NativeError::Conflict,
        crate::persistence::Error::Unavailable => NativeError::Unavailable,
    }
}
fn now_ms() -> Result<i64> {
    let elapsed = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|_| NativeError::Unavailable)?;
    i64::try_from(elapsed.as_millis()).map_err(|_| NativeError::Unavailable)
}
fn private_archive(path: &Path) -> Result<File> {
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|_| NativeError::Unavailable)?;
    let meta = file.metadata().map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads the current process identity.
    if !meta.is_file()
        || meta.nlink() != 1
        || meta.uid() != unsafe { libc::geteuid() }
        || meta.mode() & 0o077 != 0
    {
        return Err(NativeError::Unsupported);
    }
    Ok(file)
}

/// Until a native ID is durably selected, even the right operation name and
/// config are insufficient evidence. An unstarted import must have exactly the
/// standalone disk bytes validated from the committed archive.
pub(super) fn verify_unselected_disk(
    home: &Path,
    id: &str,
    size: u64,
    expected: &str,
) -> Result<()> {
    let _config = read_config(home, id)?;
    let directory = home.join("boxes").join(id).join("disks");
    let meta = fs::symlink_metadata(&directory).map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads the current process identity.
    if !meta.is_dir() || meta.mode() & 0o022 != 0 || meta.uid() != unsafe { libc::geteuid() } {
        return Err(NativeError::Unsupported);
    }
    let mut file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(directory.join("disk.qcow2"))
        .map_err(|_| NativeError::Unavailable)?;
    let before = file.metadata().map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads the current process identity.
    if !before.is_file()
        || before.nlink() != 1
        || before.uid() != unsafe { libc::geteuid() }
        || before.mode() & 0o022 != 0
        || before.len() != size
    {
        return Err(NativeError::Conflict);
    }
    let mut hash = Sha256::new();
    let mut buffer = [0; 64 << 10];
    let mut total = 0;
    loop {
        let n = file
            .read(&mut buffer)
            .map_err(|_| NativeError::Unavailable)?;
        if n == 0 {
            break;
        }
        total += n as u64;
        if total > size {
            return Err(NativeError::Conflict);
        }
        hash.update(&buffer[..n]);
    }
    let after = file.metadata().map_err(|_| NativeError::Unavailable)?;
    if total != size
        || format!("sha256:{:x}", hash.finalize()) != expected
        || before.dev() != after.dev()
        || before.ino() != after.ino()
        || before.len() != after.len()
        || before.mtime() != after.mtime()
        || before.mtime_nsec() != after.mtime_nsec()
        || before.ctime() != after.ctime()
        || before.ctime_nsec() != after.ctime_nsec()
    {
        return Err(NativeError::Conflict);
    }
    Ok(())
}
