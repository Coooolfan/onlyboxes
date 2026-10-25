//! Production startup owns the dedicated home before SDK recovery and retains
//! the external companion's exact service epoch for the lifetime of this Worker.
use super::{snapshot_control, terminal_session_manager, RunnerError};
use crate::config::Config;
use crate::proto::registryv1::{SandboxPersistenceCapabilities, SandboxPersistenceUsage};
use std::fs;
use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, OnceLock};
use std::time::Duration;
use terminal_session_manager::{
    CacheMaintenance, NativeControlConfig, NativeControlEngine, TerminalSessionManager,
};
use worker_boxlite::persistence::{
    guest_platform, ExecutionLeaseLedger, HostProcess, LeaseGuardClient, NativeRuntime,
    RuntimeHomeGuard, ARCHIVE_FORMAT, RUNTIME_VERSION,
};

static SHARED: OnceLock<Arc<Startup>> = OnceLock::new();
pub(crate) struct Startup {
    config: crate::config::PersistenceConfig,
    home: String,
    worker: String,
    owner: Arc<RuntimeHomeGuard>,
    client: LeaseGuardClient,
    manager: Arc<TerminalSessionManager>,
    engine: Arc<NativeControlEngine>,
    native: Arc<NativeRuntime>,
    dispatcher: Arc<snapshot_control::ControlDispatcher>,
    cache_maintenance: CacheMaintenance,
}
fn unavailable() -> RunnerError {
    RunnerError::Message("BoxLite persistence ownership or companion is unavailable".into())
}
// Only fixed call-site labels reach startup errors; SDK/IO error text can
// contain paths, configuration or transport credentials.
fn startup_unavailable(stage: &'static str) -> RunnerError {
    RunnerError::Message(format!("BoxLite persistence startup failed at {stage}"))
}
fn canonical_candidate(path: &Path) -> Result<PathBuf, RunnerError> {
    resolve_candidate(path, true)
}
fn resolve_candidate(path: &Path, leaf: bool) -> Result<PathBuf, RunnerError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) => {
            if leaf && metadata.file_type().is_symlink() {
                return Err(unavailable());
            }
            let resolved = fs::canonicalize(path).map_err(|_| unavailable())?;
            if !resolved.is_dir() {
                return Err(unavailable());
            }
            Ok(resolved)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            let parent = path.parent().ok_or_else(unavailable)?;
            Ok(resolve_candidate(parent, false)?.join(path.file_name().ok_or_else(unavailable)?))
        }
        Err(_) => Err(unavailable()),
    }
}
fn directories(config: &Config) -> Result<Vec<PathBuf>, RunnerError> {
    let paths = [
        &config.boxlite_home,
        &config.persistence.journal_dir,
        &config.persistence.lease_state_dir,
        &config.persistence.lease_socket_dir,
        &config.persistence.python_home,
    ]
    .into_iter()
    .map(|path| canonical_candidate(Path::new(path)))
    .collect::<Result<Vec<_>, _>>()?;
    for (index, path) in paths.iter().enumerate() {
        for other in paths.iter().skip(index + 1) {
            if path.starts_with(other) || other.starts_with(path) {
                return Err(RunnerError::Message("persistent BoxLite home, journal, lease state, control and pythonExec home must be separate non-nested directories".into()));
            }
        }
    }
    Ok(paths)
}
pub(crate) async fn initialize(
    cfg: &Config,
    manager: Arc<TerminalSessionManager>,
) -> Result<Option<Arc<Startup>>, RunnerError> {
    cfg.persistence
        .validate(&cfg.boxlite_home)
        .map_err(RunnerError::Message)?;
    if !cfg.persistence.enabled {
        crate::boxlite_runtime::validate_unmanaged_home(cfg)
            .map_err(|error| RunnerError::Message(error.to_string()))?;
        return Ok(None);
    }
    let paths = directories(cfg).map_err(|_| startup_unavailable("directories"))?;
    ExecutionLeaseLedger::verify_external_store(
        &paths[2],
        &cfg.worker_id,
        &cfg.persistence.lease_store_id,
    )
    .map_err(|_| startup_unavailable("lease_store"))?;
    let socket = paths[3].join("control.sock");
    let identity = LeaseGuardClient::discover(
        &socket,
        &cfg.worker_id,
        Some(&cfg.persistence.lease_store_id),
    )
    .await
    .map_err(|_| startup_unavailable("companion_discovery"))?;
    let current = HostProcess::current().map_err(|_| startup_unavailable("host_identity"))?;
    if identity.process.host_id != current.host_id
        || identity.process.boot_id != current.boot_id
        || identity.process.process_namespace != current.process_namespace
    {
        return Err(startup_unavailable("companion_identity"));
    }
    let client = LeaseGuardClient::bind(socket, identity);
    let owner = Arc::new(
        RuntimeHomeGuard::open(&paths[0], &paths[1], &cfg.worker_id)
            .map_err(|_| startup_unavailable("home_ownership"))?,
    );
    owner
        .bind_execution_store(&paths[2], &cfg.persistence.lease_store_id)
        .map_err(|_| startup_unavailable("execution_store_binding"))?;
    // Stateless pythonExec instances never enter the protected terminal home.
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&paths[4])
        .map_err(|_| startup_unavailable("python_home_create"))?;
    let metadata =
        fs::symlink_metadata(&paths[4]).map_err(|_| startup_unavailable("python_home_metadata"))?;
    use std::os::unix::fs::MetadataExt;
    if !metadata.is_dir()
        || metadata.permissions().mode() & 0o077 != 0
        || metadata.uid() != unsafe { libc::geteuid() }
    {
        return Err(startup_unavailable("python_home_permissions"));
    }
    let journal = owner.journal();
    let runtime = Arc::new(
        crate::boxlite_runtime::initialize_guarded(cfg, owner.clone())
            .map_err(|_| startup_unavailable("sdk_initialization"))?
            .clone(),
    );
    let native = Arc::new(
        NativeRuntime::new(runtime, &paths[0])
            .map_err(|_| startup_unavailable("native_runtime"))?
            .with_execution_guard(client.clone()),
    );
    let p = &cfg.persistence;
    let engine = NativeControlEngine::new(
        manager.clone(),
        journal,
        native.clone(),
        NativeControlConfig {
            max_inflight: p.max_inflight,
            tmp_quota_bytes: p.tmp_quota_bytes,
            drain_timeout: Duration::from_secs(p.drain_timeout_sec),
            readiness_timeout: Duration::from_secs(p.readiness_timeout_sec),
            cache_ttl: Duration::from_secs(p.cache_ttl_sec),
            cache_max_bytes: p.cache_max_bytes,
            transfer_concurrency: p.transfer_concurrency,
            transfer_idle: Duration::from_secs(p.transfer_idle_sec),
        },
    )
    .await
    .map_err(|_| startup_unavailable("control_engine"))?;
    let dispatcher = snapshot_control::ControlDispatcher::new(engine.clone(), p.max_inflight)
        .map_err(|_| startup_unavailable("dispatcher"))?;
    let cache_maintenance = engine.start_cache_maintenance();
    let startup = Arc::new(Startup {
        config: p.clone(),
        home: cfg.boxlite_home.clone(),
        worker: cfg.worker_id.clone(),
        owner,
        client,
        manager,
        engine,
        native,
        dispatcher: dispatcher.clone(),
        cache_maintenance,
    });
    if snapshot_control::install_dispatcher(dispatcher).is_err()
        || SHARED.set(startup.clone()).is_err()
    {
        let _ = startup
            .engine
            .stop_cache_maintenance(&startup.cache_maintenance)
            .await;
        return Err(startup_unavailable("singleton_install"));
    }
    tracing::info!(
        runtime_version = RUNTIME_VERSION,
        max_inflight = p.max_inflight,
        "BoxLite persistence initialized"
    );
    Ok(Some(startup))
}
impl Startup {
    fn matches(&self, cfg: &Config) -> bool {
        self.config == cfg.persistence
            && self.home == cfg.boxlite_home
            && self.worker == cfg.worker_id
    }
    fn request_writer_stops(&self) {
        if self
            .native
            .request_guarded_writer_stops(&self.owner.journal())
            .is_err()
        {
            tracing::warn!("persistent writer stop requests retain an unresolved outcome");
        }
    }
    pub(crate) async fn monitor(&self) -> RunnerError {
        let mut ticks = tokio::time::interval(Duration::from_secs(1));
        ticks.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            ticks.tick().await;
            let healthy = self
                .owner
                .verify_execution_store(
                    Path::new(&self.config.lease_state_dir),
                    &self.config.lease_store_id,
                )
                .is_ok()
                && self.client.status("worker-health-check").await.is_ok();
            if !healthy {
                self.dispatcher.fail_closed();
                self.manager.fail_persistence().await;
                self.request_writer_stops();
                return unavailable();
            }
        }
    }
}
pub(crate) fn capabilities(
    cfg: &Config,
) -> Result<Option<SandboxPersistenceCapabilities>, RunnerError> {
    cfg.persistence
        .validate(&cfg.boxlite_home)
        .map_err(RunnerError::Message)?;
    if !cfg.persistence.enabled {
        return Ok(None);
    }
    let context = SHARED
        .get()
        .filter(|context| context.matches(cfg))
        .ok_or_else(unavailable)?;
    Ok(Some(SandboxPersistenceCapabilities {
        enabled: true,
        runtime: "boxlite".into(),
        runtime_version: RUNTIME_VERSION.into(),
        guest_platform: guest_platform().map_err(|_| unavailable())?.into(),
        read_formats: vec![ARCHIVE_FORMAT.into()],
        write_formats: vec![ARCHIVE_FORMAT.into()],
        max_inflight: context.config.max_inflight as i32,
        available_staging_bytes: context
            .engine
            .available_staging_bytes()
            .map_err(|_| unavailable())?,
    }))
}
pub(crate) fn usage(cfg: &Config) -> Result<Option<SandboxPersistenceUsage>, RunnerError> {
    if !cfg.persistence.enabled {
        return Ok(None);
    }
    let context = SHARED
        .get()
        .filter(|context| context.matches(cfg))
        .ok_or_else(unavailable)?;
    Ok(Some(SandboxPersistenceUsage {
        inflight: context.dispatcher.inflight().map_err(|_| unavailable())?,
        available_staging_bytes: context
            .engine
            .available_staging_bytes()
            .map_err(|_| unavailable())?,
    }))
}

pub(crate) async fn finish_shutdown(orderly_and_drained: bool) {
    if let Some(context) = SHARED.get() {
        let preserve = orderly_and_drained
            && context
                .manager
                .healthy_guarded_shutdown(context.client.identity())
                .await
            && context
                .owner
                .verify_execution_store(
                    Path::new(&context.config.lease_state_dir),
                    &context.config.lease_store_id,
                )
                .is_ok()
            && context.client.status("worker-shutdown-check").await.is_ok();
        if preserve {
            // All accepted work is joined. The same independent service still
            // owns every boot/lease; it enforces expiry across this restart.
            // SDK shutdown separately skips detached terminal instances.
            tracing::info!("persistent BoxLite roots retained under independent execution leases");
        } else {
            context.request_writer_stops();
        }
    }
}

pub(crate) async fn stop_cache_maintenance() {
    if let Some(context) = SHARED.get() {
        if context
            .engine
            .stop_cache_maintenance(&context.cache_maintenance)
            .await
            .is_err()
        {
            tracing::warn!("snapshot cache maintenance could not confirm shutdown");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn directory_aliases_and_nested_roots_are_rejected_before_sdk_startup() {
        let root =
            std::env::temp_dir().join(format!("onlyboxes-startup-paths-{}", uuid::Uuid::new_v4()));
        fs::DirBuilder::new().mode(0o700).create(&root).unwrap();
        let mut cfg = Config::load();
        cfg.boxlite_home = root.join("home").to_str().unwrap().into();
        cfg.persistence.journal_dir = root.join("journal").to_str().unwrap().into();
        cfg.persistence.lease_state_dir = root.join("leases").to_str().unwrap().into();
        cfg.persistence.lease_socket_dir = root.join("control").to_str().unwrap().into();
        cfg.persistence.python_home = root.join("python").to_str().unwrap().into();
        assert!(directories(&cfg).is_ok());
        cfg.persistence.python_home = root.join("home/python").to_str().unwrap().into();
        assert!(directories(&cfg).is_err());
        let alias = root.join("alias");
        std::os::unix::fs::symlink(&root, &alias).unwrap();
        cfg.persistence.python_home = alias.join("home").to_str().unwrap().into();
        assert!(directories(&cfg).is_err());
        fs::remove_dir_all(root).unwrap();
    }
}
