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
    #[error("BoxLite snapshot archive is invalid")]
    ArchiveInvalid,
    #[error("BoxLite snapshot archive exceeds its budget")]
    Budget,
}
type Result<T> = std::result::Result<T, NativeError>;

pub struct NativeInstance {
    pub box_id: String,
    pub box_name: String,
    pub spec: RestoreSpec,
    pub status: BoxStatus,
    pub pid: Option<u32>,
}

pub struct NativeRuntime {
    runtime: Arc<BoxliteRuntime>,
    execution_guard: Option<super::LeaseGuardClient>,
    home: PathBuf,
}

impl NativeRuntime {
    /// Bind the independent companion for every controlled boot and replay.
    /// The paired journal fixes the Worker and each accepted service epoch.
    pub fn with_execution_guard(mut self, guard: super::LeaseGuardClient) -> Self {
        self.execution_guard = Some(guard);
        self
    }

    pub fn execution_guard(&self) -> Option<&super::LeaseGuardClient> {
        self.execution_guard.as_ref()
    }

    fn boot_executable(&self, box_id: &str, create: bool) -> Result<super::BootExecutable> {
        let config = read_config(&self.home, box_id)?;
        if !config.options.advanced.security.jailer_enabled
            || config.options.advanced.security.sandbox_profile.is_some()
        {
            return Err(NativeError::Unsupported);
        }
        let source =
            boxlite::util::find_binary("boxlite-shim").map_err(|_| NativeError::Unavailable)?;
        super::BootExecutable::prepare(&self.home, box_id, &source, create).map_err(|error| {
            match error {
                super::Error::Conflict => NativeError::Conflict,
                super::Error::Unavailable => NativeError::Unavailable,
            }
        })
    }

    /// Read-only executable evidence for an original running session. No
    /// missing executable is created around an already-running process.
    pub async fn original_boot_executable(
        &self,
        scope: &str,
        box_id: &str,
    ) -> Result<super::BootExecutable> {
        let instance = self.inspect(scope, box_id).await?;
        if instance.status != BoxStatus::Running {
            return Err(NativeError::Conflict);
        }
        self.boot_executable(box_id, false)
    }
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
        Ok(Self {
            runtime,
            home,
            execution_guard: None,
        })
    }

    /// Available capacity in the dedicated SDK home, before accepting staging.
    pub fn available_staging_bytes(&self) -> Result<u64> {
        crate::persistence::space::available(&self.home).map_err(|_| NativeError::Unavailable)
    }

    pub async fn inspect(&self, scope: &str, box_id: &str) -> Result<NativeInstance> {
        self.inspect_known(scope, box_id, &terminal_name(scope), None)
            .await
    }

    /// Pin a running original session's writer identity without starting,
    /// stopping or deleting it. The caller must durably bind this evidence
    /// before using it for independent lease enforcement.
    pub async fn inspect_writer(&self, scope: &str, box_id: &str) -> Result<super::HostProcess> {
        let first = self.inspect(scope, box_id).await?;
        if first.status != BoxStatus::Running {
            return Err(NativeError::Conflict);
        }
        let pin =
            stop::read_shim_process(&self.home, box_id, first.pid.ok_or(NativeError::Conflict)?)?;
        let current = self.inspect(scope, box_id).await?;
        if current.status != BoxStatus::Running
            || current.pid != first.pid
            || current.spec != first.spec
            || stop::read_shim_process(&self.home, box_id, pin.pid)? != pin
        {
            return Err(NativeError::Conflict);
        }
        Ok(pin)
    }

    pub async fn inspect_target(
        &self,
        journal: &Journal,
        operation: &str,
    ) -> Result<NativeInstance> {
        let state = journal.snapshot().map_err(|_| NativeError::Unavailable)?;
        let op = state
            .operations
            .get(operation)
            .ok_or(NativeError::Conflict)?;
        if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
            || op.kind != Kind::Target
            || !op.verified
            || op.box_id.is_empty()
            || !matches!(op.phase, Phase::Importing | Phase::Ready | Phase::Active)
        {
            return Err(NativeError::Conflict);
        }
        let spec = op.restore_spec.as_ref().ok_or(NativeError::Conflict)?;
        self.inspect_known(
            &op.scoped_session_id,
            &op.box_id,
            &target_name(&op.scoped_session_id, &op.operation_id, op.generation),
            Some(spec),
        )
        .await
    }

    pub async fn inspect_source(
        &self,
        journal: &Journal,
        operation: &str,
    ) -> Result<NativeInstance> {
        let state = journal.snapshot().map_err(|_| NativeError::Unavailable)?;
        let op = state
            .operations
            .get(operation)
            .ok_or(NativeError::Conflict)?;
        if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
            || op.kind != Kind::Source
            || !matches!(
                op.phase,
                Phase::Preparing
                    | Phase::Frozen
                    | Phase::Committed
                    | Phase::Finalized
                    | Phase::RollingBack
                    | Phase::RolledBack
            )
        {
            return Err(NativeError::Conflict);
        }
        let spec = op.restore_spec.as_ref().ok_or(NativeError::Conflict)?;
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
        let imported = origins.next();
        if origins.next().is_some() {
            return Err(NativeError::Conflict);
        }
        let name = imported
            .map(|prior| {
                target_name(
                    &prior.scoped_session_id,
                    &prior.operation_id,
                    prior.generation,
                )
            })
            .unwrap_or_else(|| terminal_name(&op.scoped_session_id));
        let inspected = self
            .inspect_known(
                &op.scoped_session_id,
                &op.box_id,
                &name,
                imported.map(|_| spec),
            )
            .await?;
        if &inspected.spec != spec {
            return Err(NativeError::Conflict);
        }
        Ok(inspected)
    }

    async fn inspect_known(
        &self,
        scope: &str,
        box_id: &str,
        name: &str,
        expected: Option<&RestoreSpec>,
    ) -> Result<NativeInstance> {
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
        if info.id.as_str() != box_id || info.name.as_deref() != Some(name) {
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
        let spec = match expected {
            Some(pin) => spec_from_config_with_pin(&config, &info, Some(pin))?,
            None => spec_from_config(&config, &info)?,
        };
        if expected.is_some_and(|expected| expected != &spec) {
            return Err(NativeError::Conflict);
        }
        Ok(NativeInstance {
            box_id: box_id.to_owned(),
            box_name: name.to_owned(),
            spec,
            status: info.status,
            pid: info.pid,
        })
    }
}

fn read_config(home: &Path, id: &str) -> Result<BoxConfig> {
    read_config_mode(home, id, true)
}
fn read_config_mode(home: &Path, id: &str, require_home: bool) -> Result<BoxConfig> {
    if !valid_id(id) {
        return Err(NativeError::Conflict);
    }
    // SAFETY: geteuid reads process identity and takes no pointer.
    let uid = unsafe { libc::geteuid() };
    let mut directories = vec![home.join("db"), home.join("boxes")];
    let box_home = home.join("boxes").join(id);
    match fs::symlink_metadata(&box_home) {
        Ok(_) => directories.push(box_home),
        Err(e) if !require_home && e.kind() == std::io::ErrorKind::NotFound => (),
        Err(_) => return Err(NativeError::Unavailable),
    }
    for dir in directories {
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
    let data: Option<(Option<String>, String)> = transaction
        .query_row(
            "SELECT name,json FROM box_config WHERE id=?1 AND length(CAST(json AS BLOB))<=?2",
            rusqlite::params![id, CONFIG_LIMIT as i64],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .optional()
        .map_err(|_| NativeError::Unavailable)?;
    let (stored_name, data) = data.ok_or(NativeError::Unsupported)?;
    let value: serde_json::Value =
        serde_json::from_str(&data).map_err(|_| NativeError::Unsupported)?;
    let config: BoxConfig =
        serde_json::from_value(value.clone()).map_err(|_| NativeError::Unsupported)?;
    if config.id.as_str() != id
        || config.name != stored_name
        || config.box_home.file_name().and_then(|name| name.to_str()) != Some(id)
        || fs::canonicalize(config.box_home.parent().ok_or(NativeError::Conflict)?)
            .map_err(|_| NativeError::Conflict)?
            != home.join("boxes")
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
    spec_from_config_with_pin(config, info, None)
}

fn spec_from_config_with_pin(
    config: &BoxConfig,
    info: &BoxInfo,
    pin: Option<&RestoreSpec>,
) -> Result<RestoreSpec> {
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
    let image = if let Some(resolved) = info.resolved_image.as_ref() {
        pin_image(original, &resolved.manifest_digest)?
    } else {
        let pin = pin.ok_or(NativeError::Unsupported)?;
        pin.validate().map_err(|_| NativeError::Unsupported)?;
        if original != &pin.image {
            return Err(NativeError::Conflict);
        }
        pin.image.clone()
    };
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

/// Operation and generation are immutable journal identities. A native import
/// can be discovered after a lost SDK acknowledgement without using the legacy
/// stable session name, which may still refer to an older generation.
pub fn target_name(scope: &str, operation: &str, generation: i64) -> String {
    let tuple =
        serde_json::to_vec(&(scope, operation, generation)).expect("string tuple serializes");
    format!("onlyboxes-restore-v1-{:x}", Sha256::digest(tuple))
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

#[path = "native_export.rs"]
mod export;

#[path = "native_import.rs"]
mod import;

#[path = "native_stop.rs"]
pub(super) mod stop;

#[path = "native_boot_guard.rs"]
mod boot_guard;
#[path = "native_start.rs"]
mod start;

#[path = "native_cleanup.rs"]
mod cleanup;

#[path = "native_delete.rs"]
mod delete;

#[path = "native_preflight.rs"]
mod preflight;
pub(super) use preflight::preflight_home;

#[path = "native_recovery.rs"]
mod recovery;
pub(super) use recovery::reconcile_home;
