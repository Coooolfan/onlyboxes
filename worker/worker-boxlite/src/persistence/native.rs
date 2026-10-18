//! Inspection reads the immutable configuration of the chosen native ID. It
//! never derives an existing box's configuration from current Worker defaults.

use std::collections::BTreeMap;
use std::fs;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use boxlite::litebox::BoxConfig;
use boxlite::{BoxInfo, BoxOptions, BoxStatus, BoxliteRuntime, RootfsSpec};
use rusqlite::{Connection, OpenFlags, OptionalExtension};
use sha2::{Digest, Sha256};

use super::types::{valid_id, valid_scope};
use super::{Journal, Kind, Phase, RestoreSpec, ARCHIVE_FORMAT};

pub const RUNTIME_VERSION: &str = "0.10.7";
const CONFIG_LIMIT: usize = 64 << 10;

#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum NativeError {
    #[error("BoxLite persistence backend is unavailable")]
    Unavailable,
    #[error("BoxLite instance specification is unsupported")]
    Unsupported,
    #[error("BoxLite instance identity conflicts with the lifecycle operation")]
    Conflict,
    #[error("BoxLite instance is missing")]
    Missing,
}
type Result<T> = std::result::Result<T, NativeError>;

pub struct NativeInstance {
    pub box_id: String,
    pub spec: RestoreSpec,
    pub status: BoxStatus,
    pub pid: Option<u32>,
}

pub struct NativeRuntime {
    runtime: Arc<BoxliteRuntime>,
    home: PathBuf,
}

impl NativeRuntime {
    /// The SDK runtime must use this private, dedicated home. No directory or
    /// permissions are changed by inspection or this constructor.
    pub fn new(runtime: Arc<BoxliteRuntime>, home: impl AsRef<Path>) -> Result<Self> {
        let home = home.as_ref();
        if !home.is_absolute() {
            return Err(NativeError::Unsupported);
        }
        let meta = fs::symlink_metadata(home).map_err(|_| NativeError::Unavailable)?;
        // SAFETY: geteuid reads process identity and takes no pointer.
        if !meta.is_dir() || meta.mode() & 0o077 != 0 || meta.uid() != unsafe { libc::geteuid() } {
            return Err(NativeError::Unsupported);
        }
        let home = fs::canonicalize(home).map_err(|_| NativeError::Unavailable)?;
        Ok(Self { runtime, home })
    }

    pub async fn inspect(&self, scope: &str, box_id: &str) -> Result<NativeInstance> {
        if !valid_scope(scope) || !valid_id(box_id) {
            return Err(NativeError::Conflict);
        }
        let native = self
            .runtime
            .get(box_id)
            .await
            .map_err(|_| NativeError::Unavailable)?
            .ok_or(NativeError::Missing)?;
        let info = native.info().await.map_err(|_| NativeError::Unavailable)?;
        if info.id.as_str() != box_id || info.name.as_deref() != Some(terminal_name(scope).as_str())
        {
            return Err(NativeError::Conflict);
        }
        if !matches!(info.status, BoxStatus::Running | BoxStatus::Stopped)
            || (info.status == BoxStatus::Running && info.pid.is_none())
            || (info.status == BoxStatus::Stopped && info.pid.is_some())
        {
            return Err(NativeError::Unsupported);
        }
        let home = self.home.clone();
        let id = box_id.to_owned();
        let config = tokio::task::spawn_blocking(move || read_config(&home, &id))
            .await
            .map_err(|_| NativeError::Unavailable)??;
        let spec = spec_from_config(&config, &info)?;
        Ok(NativeInstance {
            box_id: box_id.to_owned(),
            spec,
            status: info.status,
            pid: info.pid,
        })
    }

    /// Caller must close command/preview admission and drain in-flight work.
    /// The durable source claim precedes stopping; an uncertain stop retains
    /// that claim and the same rootfs for reconciliation. No instance is removed.
    pub async fn stop_source(&self, journal: &Journal, operation: &str) -> Result<NativeInstance> {
        let state = journal.snapshot().map_err(|_| NativeError::Unavailable)?;
        let op = state
            .operations
            .get(operation)
            .ok_or(NativeError::Conflict)?;
        if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
            || op.kind != Kind::Source
            || !matches!(op.phase, Phase::Preparing | Phase::Frozen)
        {
            return Err(NativeError::Conflict);
        }
        let inspected = self.inspect(&op.scoped_session_id, &op.box_id).await?;
        if Some(&inspected.spec) != op.restore_spec.as_ref() {
            return Err(NativeError::Conflict);
        }
        let native = self
            .runtime
            .get(&op.box_id)
            .await
            .map_err(|_| NativeError::Unavailable)?
            .ok_or(NativeError::Missing)?;
        if inspected.status == BoxStatus::Running {
            native.stop().await.map_err(|_| NativeError::Unavailable)?;
        }
        let stopped = self.inspect(&op.scoped_session_id, &op.box_id).await?;
        let current = journal.snapshot().map_err(|_| NativeError::Unavailable)?;
        if stopped.status != BoxStatus::Stopped
            || stopped.pid.is_some()
            || current
                .current
                .get(&op.scoped_session_id)
                .map(String::as_str)
                != Some(operation)
            || Some(&stopped.spec) != op.restore_spec.as_ref()
            || !current.operations.get(operation).is_some_and(|latest| {
                latest.kind == Kind::Source
                    && latest.generation == op.generation
                    && matches!(latest.phase, Phase::Preparing | Phase::Frozen)
            })
        {
            return Err(NativeError::Conflict);
        }
        Ok(stopped)
    }
}

fn read_config(home: &Path, id: &str) -> Result<BoxConfig> {
    if !valid_id(id) {
        return Err(NativeError::Conflict);
    }
    // SAFETY: geteuid reads process identity and takes no pointer.
    let uid = unsafe { libc::geteuid() };
    for dir in [
        home.join("db"),
        home.join("boxes"),
        home.join("boxes").join(id),
    ] {
        let meta = fs::symlink_metadata(dir).map_err(|_| NativeError::Unavailable)?;
        if !meta.is_dir() || meta.uid() != uid || meta.mode() & 0o022 != 0 {
            return Err(NativeError::Unsupported);
        }
    }
    let path = home.join("db/boxlite.db");
    let meta = fs::symlink_metadata(&path).map_err(|_| NativeError::Unavailable)?;
    if !meta.is_file() || meta.uid() != uid || meta.nlink() != 1 || meta.mode() & 0o022 != 0 {
        return Err(NativeError::Unsupported);
    }
    // BoxLite 0.10.7 exposes BoxConfig as a public type but no configuration
    // getter on LiteBox. Read only its pinned schema; never run SDK migrations
    // or write the SDK database through this connection.
    let connection = Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_ONLY
            | OpenFlags::SQLITE_OPEN_NO_MUTEX
            | OpenFlags::SQLITE_OPEN_NOFOLLOW,
    )
    .map_err(|_| NativeError::Unavailable)?;
    connection
        .busy_timeout(Duration::from_secs(1))
        .map_err(|_| NativeError::Unavailable)?;
    let transaction = connection
        .unchecked_transaction()
        .map_err(|_| NativeError::Unavailable)?;
    let version: i64 = transaction
        .query_row("SELECT version FROM schema_version WHERE id=1", [], |row| {
            row.get(0)
        })
        .map_err(|_| NativeError::Unavailable)?;
    if version != 10 {
        return Err(NativeError::Unsupported);
    }
    let data: Option<String> = transaction
        .query_row(
            "SELECT json FROM box_config WHERE id=?1 AND length(CAST(json AS BLOB))<=?2",
            rusqlite::params![id, CONFIG_LIMIT as i64],
            |row| row.get(0),
        )
        .optional()
        .map_err(|_| NativeError::Unavailable)?;
    let data = data.ok_or(NativeError::Unsupported)?;
    let value: serde_json::Value =
        serde_json::from_str(&data).map_err(|_| NativeError::Unsupported)?;
    let config: BoxConfig =
        serde_json::from_value(value.clone()).map_err(|_| NativeError::Unsupported)?;
    if config.id.as_str() != id
        || fs::canonicalize(&config.box_home).map_err(|_| NativeError::Conflict)?
            != home.join("boxes").join(id)
    {
        return Err(NativeError::Conflict);
    }
    if serde_json::to_value(&config.options).map_err(|_| NativeError::Unsupported)?
        != value["options"]
    {
        return Err(NativeError::Unsupported);
    }
    transaction.commit().map_err(|_| NativeError::Unavailable)?;
    Ok(config)
}

fn spec_from_config(config: &BoxConfig, info: &BoxInfo) -> Result<RestoreSpec> {
    if config.id != info.id || config.name != info.name {
        return Err(NativeError::Conflict);
    }
    if serde_json::to_value(config.engine_kind).map_err(|_| NativeError::Unsupported)?
        != serde_json::Value::String("Libkrun".into())
    {
        return Err(NativeError::Unsupported);
    }
    let RootfsSpec::Image(original) = &config.options.rootfs else {
        return Err(NativeError::Unsupported);
    };
    if info.image != *original
        || config.options.cpus != Some(info.cpus)
        || config.options.memory_mib != Some(info.memory_mib)
    {
        return Err(NativeError::Conflict);
    }
    let resolved = info
        .resolved_image
        .as_ref()
        .ok_or(NativeError::Unsupported)?;
    let image = pin_image(original, &resolved.manifest_digest)?;
    let options = &config.options;
    let env: BTreeMap<_, _> = options.env.iter().cloned().collect();
    if env.len() != options.env.len() {
        return Err(NativeError::Unsupported);
    }
    let max_processes = options
        .advanced
        .security
        .resource_limits
        .max_processes
        .and_then(|n| i32::try_from(n).ok())
        .ok_or(NativeError::Unsupported)?;
    let spec = RestoreSpec {
        runtime: "boxlite".into(),
        runtime_version: RUNTIME_VERSION.into(),
        platform: guest_platform()?.into(),
        format: ARCHIVE_FORMAT.into(),
        image,
        memory_mib: info.memory_mib as i64,
        cpus: info.cpus as f64,
        max_processes,
        user: options.user.clone().unwrap_or_default(),
        working_dir: options.working_dir.clone().unwrap_or_default(),
        env,
        entrypoint: options.entrypoint.clone().unwrap_or_default(),
        command: options.cmd.clone().unwrap_or_default(),
    };
    spec.validate().map_err(|_| NativeError::Unsupported)?;
    let mut expected = options_for_spec(&spec)?;
    expected.rootfs = RootfsSpec::Image(original.clone());
    let mut actual = options.clone();
    actual.env.sort();
    if serde_json::to_value(actual).map_err(|_| NativeError::Unsupported)?
        != serde_json::to_value(expected).map_err(|_| NativeError::Unsupported)?
    {
        return Err(NativeError::Unsupported);
    }
    Ok(spec)
}

pub(crate) fn options_for_spec(spec: &RestoreSpec) -> Result<BoxOptions> {
    spec.validate().map_err(|_| NativeError::Unsupported)?;
    if spec.runtime_version != RUNTIME_VERSION
        || spec.platform != guest_platform()?
        || spec.entrypoint.is_empty()
        || spec.command.is_empty()
    {
        return Err(NativeError::Unsupported);
    }
    let mut options = BoxOptions {
        cpus: Some(spec.cpus as u8),
        memory_mib: Some(spec.memory_mib as u32),
        rootfs: RootfsSpec::Image(spec.image.clone()),
        auto_delete: Some(0),
        detach: true,
        user: (!spec.user.is_empty()).then(|| spec.user.clone()),
        working_dir: (!spec.working_dir.is_empty()).then(|| spec.working_dir.clone()),
        env: spec
            .env
            .iter()
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect(),
        entrypoint: Some(spec.entrypoint.clone()),
        cmd: Some(spec.command.clone()),
        ..Default::default()
    };
    options.advanced.security.resource_limits.max_processes = Some(spec.max_processes as u64);
    options.sanitize().map_err(|_| NativeError::Unsupported)?;
    Ok(options)
}

fn pin_image(original: &str, digest: &str) -> Result<String> {
    let Some(hash) = digest.strip_prefix("sha256:") else {
        return Err(NativeError::Unsupported);
    };
    if hash.len() != 64
        || !hash
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(NativeError::Unsupported);
    }
    let reference = original.split('@').next().ok_or(NativeError::Unsupported)?;
    let repository = match reference.rfind(':') {
        Some(colon) if reference.rfind('/').is_none_or(|slash| colon > slash) => {
            &reference[..colon]
        }
        _ => reference,
    };
    if repository.is_empty() {
        return Err(NativeError::Unsupported);
    }
    Ok(format!("{repository}@{digest}"))
}

pub fn terminal_name(scope: &str) -> String {
    format!(
        "onlyboxes-terminal-v1-{:x}",
        Sha256::digest(scope.as_bytes())
    )
}

pub fn guest_platform() -> Result<&'static str> {
    match std::env::consts::ARCH {
        "aarch64" => Ok("linux/arm64"),
        "x86_64" => Ok("linux/amd64"),
        _ => Err(NativeError::Unsupported),
    }
}

#[cfg(test)]
#[path = "native_tests.rs"]
mod tests;
