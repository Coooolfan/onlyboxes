use super::*;
use crate::persistence::archive::{canonicalize_named_export, describe_archive};
use crate::persistence::{verify_archive, Archive, ArchiveError, ArchiveLimits, Journal};
use std::fs::{File, OpenOptions};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};

impl NativeRuntime {
    /// Caller reserves scratch space and serializes/drains native operations.
    /// Keep the task alive until SDK native I/O finishes even after cancellation.
    pub async fn export_source(
        &self,
        journal: Arc<Journal>,
        operation: &str,
        limits: ArchiveLimits,
    ) -> Result<Archive> {
        limits.validate().map_err(archive_error)?;
        let j = journal.clone();
        let state = tokio::task::spawn_blocking(move || j.snapshot())
            .await
            .map_err(|_| NativeError::Unavailable)?
            .map_err(|error| match error {
                crate::persistence::Error::Conflict => NativeError::Conflict,
                crate::persistence::Error::Unavailable => NativeError::Unavailable,
            })?;
        let op = state
            .operations
            .get(operation)
            .ok_or(NativeError::Conflict)?
            .clone();
        if op.kind != Kind::Source
            || state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
            || !matches!(op.phase, Phase::Preparing | Phase::Frozen)
        {
            return Err(NativeError::Conflict);
        }
        let spec = op
            .restore_spec
            .as_ref()
            .ok_or(NativeError::Conflict)?
            .clone();
        let path = journal
            .artifact_path(&op.scoped_session_id, operation, op.generation)
            .map_err(|_| NativeError::Unavailable)?;
        if op.phase == Phase::Frozen {
            if !op.source_stop_confirmed
                || !op
                    .source_process
                    .as_ref()
                    .ok_or(NativeError::Conflict)?
                    .writers_exited()
                    .map_err(|_| NativeError::Unavailable)?
            {
                return Err(NativeError::Conflict);
            }
            let archive = op.archive.ok_or(NativeError::Conflict)?;
            let scope = op.scoped_session_id;
            return blocking(move || {
                let mut file = open_archive(&path)?;
                verify_archive(&mut file, &spec, &scope, &archive, limits)
                    .map_err(archive_error)?;
                Ok(archive)
            })
            .await;
        }
        let required = limits
            .max_bytes
            .checked_add(limits.max_expanded_bytes)
            .ok_or(NativeError::Budget)?;
        if op.staging_bytes < required {
            return Err(NativeError::Budget);
        }
        let stopped_source = self.stop_source(&journal, operation).await?;
        if path.try_exists().map_err(|_| NativeError::Unavailable)? {
            let scope = op.scoped_session_id.clone();
            let id = operation.to_owned();
            let archive = blocking(move || {
                let mut file = open_archive(&path)?;
                describe_archive(&mut file, &spec, &scope, &id, limits).map_err(archive_error)
            })
            .await?;
            return publish(journal, operation.to_owned(), archive).await;
        }
        let parent = path.parent().ok_or(NativeError::Conflict)?;
        let workspace = Workspace::new(
            parent,
            path.file_stem()
                .and_then(|p| p.to_str())
                .ok_or(NativeError::Conflict)?,
        )?;
        let raw = workspace.0.join("raw.boxlite");
        let native = self
            .runtime
            .get(&op.box_id)
            .await
            .map_err(|_| NativeError::Unavailable)?
            .ok_or(NativeError::Missing)?;
        native
            .export(boxlite::ExportOptions::default(), &raw)
            .await
            .map_err(|error| {
                tracing::warn!(%error, "BoxLite native archive export failed");
                NativeError::Unavailable
            })?;
        let output = workspace.0.join("canonical.boxlite");
        let output_path = output.clone();
        let home = self.home.clone();
        let captured = op.clone();
        let id = operation.to_owned();
        let target_spec = spec.clone();
        let archive = blocking(move || {
            let config = read_config(&home, &captured.box_id)?;
            let mut input = open_archive(&raw)?;
            let mut file = OpenOptions::new()
                .read(true)
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(&output_path)
                .map_err(|_| NativeError::Unavailable)?;
            canonicalize_named_export(
                &mut input,
                &mut file,
                &target_spec,
                &captured.scoped_session_id,
                &config.options,
                &stopped_source.box_name,
                &id,
                limits,
            )
            .map_err(archive_error)
        })
        .await?;
        let stopped = self.inspect_source(&journal, operation).await?;
        if stopped.status != BoxStatus::Stopped || Some(&stopped.spec) != op.restore_spec.as_ref() {
            return Err(NativeError::Conflict);
        }
        let parent = parent.to_owned();
        let scope = op.scoped_session_id;
        let id = operation.to_owned();
        let archive = blocking(move || match fs::hard_link(&output, &path) {
            Ok(()) => {
                File::open(parent)
                    .and_then(|dir| dir.sync_all())
                    .map_err(|_| NativeError::Unavailable)?;
                Ok(archive)
            }
            Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {
                let mut file = open_archive(&path)?;
                describe_archive(&mut file, &spec, &scope, &id, limits).map_err(archive_error)
            }
            Err(_) => Err(NativeError::Unavailable),
        })
        .await?;
        publish(journal, operation.to_owned(), archive).await
    }
}

pub(super) async fn blocking<T: Send + 'static>(
    work: impl FnOnce() -> Result<T> + Send + 'static,
) -> Result<T> {
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|_| NativeError::Unavailable)?
}
async fn publish(journal: Arc<Journal>, operation: String, archive: Archive) -> Result<Archive> {
    blocking(move || {
        journal
            .update(&operation, |op| {
                if !matches!(op.phase, Phase::Preparing | Phase::Frozen) {
                    return Err(crate::persistence::Error::Conflict);
                }
                op.phase = Phase::Frozen;
                op.archive = Some(archive.clone());
                op.verified = true;
                op.artifact = crate::persistence::artifact_name(
                    &op.scoped_session_id,
                    &op.operation_id,
                    op.generation,
                );
                Ok(())
            })
            .map_err(|error| match error {
                crate::persistence::Error::Conflict => NativeError::Conflict,
                crate::persistence::Error::Unavailable => NativeError::Unavailable,
            })?;
        Ok(archive)
    })
    .await
}
pub(super) fn archive_error(error: ArchiveError) -> NativeError {
    match error {
        ArchiveError::Budget => NativeError::Budget,
        ArchiveError::Invalid => NativeError::ArchiveInvalid,
        ArchiveError::Io => NativeError::Unavailable,
    }
}
fn open_archive(path: &Path) -> Result<File> {
    OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .map_err(|_| NativeError::Unavailable)
}
pub(super) struct Workspace(pub(super) PathBuf);
impl Workspace {
    pub(super) fn new(parent: &Path, identity: &str) -> Result<Self> {
        let path = parent.join(format!("work-{identity}-{}", uuid::Uuid::new_v4()));
        fs::DirBuilder::new()
            .mode(0o700)
            .create(&path)
            .map_err(|_| NativeError::Unavailable)?;
        Ok(Self(path))
    }
}
impl Drop for Workspace {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
