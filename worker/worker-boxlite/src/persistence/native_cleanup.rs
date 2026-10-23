//! Cleanup acknowledgements require native, physical and process evidence.
use super::stop::read_shim_process;
use super::*;
use crate::persistence::{Error, HostProcess, JournalState, Operation};
use std::os::unix::fs::FileTypeExt;

impl NativeRuntime {
    /// Caller holds the source gate, has joined all SDK/transfer tasks and has
    /// Console's immutable shared snapshot commit. Cache retention is separate;
    /// this removes the original writable native root, never the archive.
    pub async fn finalize_source(&self, journal: &Journal, operation: &str) -> Result<()> {
        let (state, op) = cleanup_operation(journal, operation)?;
        if op.kind != Kind::Source
            || !matches!(op.phase, Phase::Committed | Phase::Finalized)
            || !op.snapshot_committed
            || !op.remote_verified
            || !op.source_stop_confirmed
        {
            return Err(NativeError::Conflict);
        }
        let process = op.source_process.as_ref().ok_or(NativeError::Conflict)?;
        let retired = if op.lease_retirements.contains_key(&op.box_id) {
            self.retire_native_lease(journal, operation, &op.box_id)
                .await?
        } else {
            None
        };
        if retired.as_ref().map(|receipt| &receipt.retirement.process) != Some(process) {
            require_exited(process).await?;
        }
        let name = source_name(&state, &op)?;
        self.cleanup_named(journal, &op, &name).await?;
        journal
            .update(operation, |current| {
                current.phase = Phase::Finalized;
                Ok(())
            })
            .map_err(journal_error)
    }

    /// Never cancels an activated target. The caller closes admission, drains
    /// accepted work and joins every owned import/start task before calling.
    /// The operation name stays reserved even if a late SDK import finishes.
    pub async fn cancel_target(&self, journal: &Journal, operation: &str) -> Result<()> {
        self.cancel_controlled_target(journal, operation, false)
            .await
    }

    /// Console has explicitly revoked a previously activated target. The caller
    /// must close execution/preview gates and drain accepted work first. This is
    /// not permanent session deletion and never changes generation ownership.
    pub async fn revoke_target(&self, journal: &Journal, operation: &str) -> Result<()> {
        self.cancel_controlled_target(journal, operation, true)
            .await
    }

    async fn cancel_controlled_target(
        &self,
        journal: &Journal,
        operation: &str,
        revoke: bool,
    ) -> Result<()> {
        let (_, op) = cleanup_operation(journal, operation)?;
        if op.kind != Kind::Target
            || !(matches!(
                op.phase,
                Phase::Prepared
                    | Phase::Importing
                    | Phase::Ready
                    | Phase::Canceling
                    | Phase::Canceled
            ) || (revoke && op.phase == Phase::Active))
        {
            return Err(NativeError::Conflict);
        }
        if op.phase != Phase::Canceled {
            journal
                .update(operation, |current| {
                    if current.phase == Phase::Active && !revoke {
                        return Err(Error::Conflict);
                    }
                    current.phase = Phase::Canceling;
                    Ok(())
                })
                .map_err(journal_error)?;
        }
        let (_, op) = cleanup_operation(journal, operation)?;
        self.cleanup_named(
            journal,
            &op,
            &target_name(&op.scoped_session_id, operation, op.generation),
        )
        .await?;
        journal
            .update(operation, |current| {
                current.phase = Phase::Canceled;
                Ok(())
            })
            .map_err(journal_error)
    }

    pub(super) async fn cleanup_named(
        &self,
        journal: &Journal,
        op: &Operation,
        name: &str,
    ) -> Result<()> {
        // Replay a database-first SDK deletion using the durable physical IDs.
        if op.kind != Kind::Delete && !op.box_id.is_empty() {
            self.cleanup_selected(journal, op, name, &op.box_id).await?;
        }
        for id in &op.cleanup_box_ids {
            if op.kind != Kind::Delete || op.cleanup_names.get(id).map(String::as_str) == Some(name)
            {
                self.cleanup_selected(journal, op, name, id).await?;
            }
        }
        for _ in 0..4 {
            let (_, current) = cleanup_operation(journal, &op.operation_id)?;
            if current.kind != op.kind || current.generation != op.generation {
                return Err(NativeError::Conflict);
            }
            let info = self
                .runtime
                .get_info(name)
                .await
                .map_err(|_| NativeError::Unavailable)?;
            if let Some(info) = info {
                let id = info.id.as_str();
                let state = journal.snapshot().map_err(journal_error)?;
                let mut marker = self.inspect_name_fence(op, name, &info)?;
                if op.kind == Kind::Delete && !marker {
                    for prior in state.operations.values().filter(|prior| {
                        prior.operation_id != op.operation_id
                            && prior.scoped_session_id == op.scoped_session_id
                            && prior.generation < op.generation
                    }) {
                        if self.inspect_name_fence(prior, name, &info)? {
                            marker = true;
                            break;
                        }
                    }
                }
                if marker {
                    if current.name_fences.get(name).is_some_and(|pin| pin != id) {
                        return Err(NativeError::Conflict);
                    }
                    journal
                        .update(&op.operation_id, |current| {
                            current.name_fences.insert(name.to_owned(), id.to_owned());
                            Ok(())
                        })
                        .map_err(journal_error)?;
                    return Ok(());
                }
                // Never delete a replacement after the marker was acknowledged.
                if current.name_fences.contains_key(name) {
                    return Err(NativeError::Conflict);
                }
                self.cleanup_selected(journal, &current, name, id).await?;
                continue;
            }
            if current.name_fences.contains_key(name) {
                return Err(NativeError::Conflict);
            }
            // Unique native name is the fence. Resolve ambiguous create results
            // by rereading its exact immutable marker configuration next time.
            let _ = self
                .runtime
                .create(fence_options(op, name)?, Some(name.to_owned()))
                .await;
        }
        Err(NativeError::Unavailable)
    }

    async fn cleanup_selected(
        &self,
        journal: &Journal,
        op: &Operation,
        name: &str,
        id: &str,
    ) -> Result<()> {
        let info = self
            .runtime
            .get_info(id)
            .await
            .map_err(|_| NativeError::Unavailable)?;
        if let Some(info) = &info {
            if info.id.as_str() != id || info.name.as_deref() != Some(name) {
                return Err(NativeError::Conflict);
            }
            // A copied name alone does not authorize removal of an arbitrary
            // native root: immutable image, resources, mounts and config match.
            self.inspect_known(&op.scoped_session_id, id, name, op.restore_spec.as_ref())
                .await?;
            journal
                .update(&op.operation_id, |current| {
                    current.cleanup_box_ids.insert(id.to_owned());
                    current.cleanup_names.insert(id.to_owned(), name.to_owned());
                    Ok(())
                })
                .map_err(journal_error)?;
            if info.status == BoxStatus::Running {
                let process =
                    read_shim_process(&self.home, id, info.pid.ok_or(NativeError::Conflict)?)?;
                journal
                    .update(&op.operation_id, |current| {
                        current
                            .cleanup_processes
                            .insert(id.to_owned(), process.clone());
                        Ok(())
                    })
                    .map_err(journal_error)?;
                if read_shim_process(&self.home, id, process.pid)? != process {
                    return Err(NativeError::Conflict);
                }
                self.revoke_orphan_boot_for_cleanup(journal, &op.operation_id, id, &process)
                    .await?;
                let native = self
                    .runtime
                    .get(id)
                    .await
                    .map_err(|_| NativeError::Unavailable)?
                    .ok_or(NativeError::Missing)?;
                native.stop().await.map_err(|_| NativeError::Unavailable)?;
            }
        }
        let retirement = self
            .retire_native_lease(journal, &op.operation_id, id)
            .await?;
        let (_, current) = cleanup_operation(journal, &op.operation_id)?;
        let retired_process = retirement
            .as_ref()
            .map(|receipt| &receipt.retirement.process);
        if let Some(process) = current.cleanup_processes.get(id) {
            if retired_process != Some(process) {
                require_exited(process).await?;
            }
        } else if id == current.box_id {
            if let Some(process) = &current.started_process {
                if retired_process != Some(process) {
                    require_exited(process).await?;
                }
            } else if current.start_attempted && retirement.is_none() {
                // Startup may have created a writer before its response was lost.
                return Err(NativeError::Conflict);
            } else if let Some(process) = &current.source_process {
                if retired_process != Some(process) {
                    require_exited(process).await?;
                }
            }
        }
        if info.is_some() {
            let stopped = self
                .runtime
                .get_info(id)
                .await
                .map_err(|_| NativeError::Unavailable)?
                .ok_or(NativeError::Conflict)?;
            if stopped.status != BoxStatus::Stopped || stopped.pid.is_some() {
                return Err(NativeError::Conflict);
            }
            // Never ask SDK force removal to signal an unchecked/recycled PID.
            self.runtime
                .remove(id, false)
                .await
                .map_err(|_| NativeError::Unavailable)?;
        }
        if self
            .runtime
            .get_info(id)
            .await
            .map_err(|_| NativeError::Unavailable)?
            .is_some()
        {
            return Err(NativeError::Conflict);
        }
        // SDK logs filesystem cleanup failures but returns success. Complete and
        // sync the exact selected native directory before acknowledging removal.
        let home = self.home.clone();
        let id = id.to_owned();
        tokio::task::spawn_blocking(move || remove_native_home(&home, &id))
            .await
            .map_err(|_| NativeError::Unavailable)??;
        self.release_native_lease(retirement).await
    }

    pub(super) fn inspect_name_fence(
        &self,
        op: &Operation,
        name: &str,
        info: &BoxInfo,
    ) -> Result<bool> {
        let id = info.id.as_str();
        let config = read_config_mode(&self.home, id, false)?;
        if config.name.as_deref() != Some(name) || info.name.as_deref() != Some(name) {
            return Err(NativeError::Conflict);
        }
        let expected = fence_options(op, name)?;
        if serde_json::to_value(&config.options).map_err(|_| NativeError::Unsupported)?
            != serde_json::to_value(expected).map_err(|_| NativeError::Unsupported)?
        {
            return Ok(false);
        }
        if !matches!(info.status, BoxStatus::Configured | BoxStatus::Stopped)
            || info.pid.is_some()
            || serde_json::to_value(config.engine_kind).map_err(|_| NativeError::Unsupported)?
                != serde_json::Value::String("Libkrun".into())
        {
            return Err(NativeError::Conflict);
        }
        for path in [
            config.box_home.join("disks/disk.qcow2"),
            config.box_home.join("shim.pid"),
        ] {
            match fs::symlink_metadata(path) {
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
                _ => return Err(NativeError::Conflict),
            }
        }
        Ok(true)
    }
}

pub(super) fn cleanup_operation(
    journal: &Journal,
    operation: &str,
) -> Result<(JournalState, Operation)> {
    let state = journal.snapshot().map_err(journal_error)?;
    let op = state
        .operations
        .get(operation)
        .ok_or(NativeError::Conflict)?
        .clone();
    if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation) {
        return Err(NativeError::Conflict);
    }
    Ok((state, op))
}
pub(super) fn source_name(state: &JournalState, op: &Operation) -> Result<String> {
    let mut origins = state.operations.values().filter(|prior| {
        prior.kind == Kind::Target
            && prior.phase == Phase::Active
            && prior.verified
            && prior.archive.is_some()
            && prior.scoped_session_id == op.scoped_session_id
            && prior.generation == op.generation
            && prior.box_id == op.box_id
            && prior.restore_spec == op.restore_spec
    });
    let origin = origins.next();
    if origins.next().is_some() {
        return Err(NativeError::Conflict);
    }
    Ok(origin
        .map(|prior| {
            target_name(
                &prior.scoped_session_id,
                &prior.operation_id,
                prior.generation,
            )
        })
        .unwrap_or_else(|| terminal_name(&op.scoped_session_id)))
}
pub(super) fn fence_options(op: &Operation, name: &str) -> Result<BoxOptions> {
    // /dev/null is a character device, so no path below it can be an OCI bundle.
    // Even an accidental SDK start cannot create a runnable replacement root.
    if !fs::symlink_metadata("/dev/null")
        .map_err(|_| NativeError::Unavailable)?
        .file_type()
        .is_char_device()
    {
        return Err(NativeError::Unsupported);
    }
    let identity =
        serde_json::to_vec(&(&op.scoped_session_id, &op.operation_id, op.generation, name))
            .map_err(|_| NativeError::Conflict)?;
    let mut options = BoxOptions {
        cpus: Some(1),
        memory_mib: Some(32),
        rootfs: RootfsSpec::RootfsPath(format!(
            "/dev/null/onlyboxes-name-fence-v1-{:x}",
            Sha256::digest(identity)
        )),
        network: boxlite::NetworkSpec::Disabled,
        auto_delete: Some(0),
        auto_stop: Some(0),
        auto_resume: Some(false),
        entrypoint: Some(vec!["/bin/false".into()]),
        ..Default::default()
    };
    options.advanced.security.resource_limits.max_processes = Some(1);
    options.sanitize().map_err(|_| NativeError::Unsupported)?;
    Ok(options)
}
pub(super) async fn require_exited(process: &HostProcess) -> Result<()> {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    while !process.writers_exited().map_err(journal_error)? {
        if tokio::time::Instant::now() >= deadline {
            return Err(NativeError::Unavailable);
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    Ok(())
}
pub(super) fn remove_native_home(home: &Path, id: &str) -> Result<()> {
    if !valid_id(id) {
        return Err(NativeError::Conflict);
    }
    let boxes = home.join("boxes");
    let parent = fs::symlink_metadata(&boxes).map_err(|_| NativeError::Unavailable)?;
    // SAFETY: geteuid reads process identity.
    let uid = unsafe { libc::geteuid() };
    if !parent.is_dir() || parent.uid() != uid || parent.mode() & 0o022 != 0 {
        return Err(NativeError::Conflict);
    }
    let path = boxes.join(id);
    match fs::symlink_metadata(&path) {
        Ok(meta) if meta.is_dir() && meta.uid() == uid && meta.mode() & 0o022 == 0 => {
            fs::remove_dir_all(&path).map_err(|_| NativeError::Unavailable)?;
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
        _ => return Err(NativeError::Conflict),
    }
    fs::File::open(&boxes)
        .and_then(|file| file.sync_all())
        .map_err(|_| NativeError::Unavailable)?;
    match fs::symlink_metadata(&path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        _ => Err(NativeError::Conflict),
    }
}
pub(super) fn journal_error(error: Error) -> NativeError {
    match error {
        Error::Conflict => NativeError::Conflict,
        Error::Unavailable => NativeError::Unavailable,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{symlink, PermissionsExt};

    #[test]
    fn physical_cleanup_is_id_scoped_and_rejects_redirected_roots() {
        let home = std::env::temp_dir().join(format!("onlyboxes-cleanup-{}", uuid::Uuid::new_v4()));
        fs::create_dir(&home).unwrap();
        fs::set_permissions(&home, fs::Permissions::from_mode(0o700)).unwrap();
        fs::create_dir(home.join("boxes")).unwrap();
        fs::create_dir(home.join("outside")).unwrap();
        fs::write(home.join("outside/keep"), b"unrelated").unwrap();
        symlink(home.join("outside"), home.join("boxes/redirected")).unwrap();
        assert_eq!(
            remove_native_home(&home, "redirected"),
            Err(NativeError::Conflict)
        );
        assert_eq!(
            remove_native_home(&home, "../outside"),
            Err(NativeError::Conflict)
        );
        assert_eq!(fs::read(home.join("outside/keep")).unwrap(), b"unrelated");
        fs::create_dir(home.join("boxes/owned")).unwrap();
        fs::write(home.join("boxes/owned/disk"), b"residue").unwrap();
        symlink(home.join("outside"), home.join("boxes/owned/nested-link")).unwrap();
        remove_native_home(&home, "owned").unwrap();
        remove_native_home(&home, "owned").unwrap();
        assert!(!home.join("boxes/owned").exists());
        assert_eq!(fs::read(home.join("outside/keep")).unwrap(), b"unrelated");
        fs::remove_dir_all(home).unwrap();
    }
}
