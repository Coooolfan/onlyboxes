//! The caller owns SDK create/start through completion and serializes the scope.
//! Errors retain the selected root and boot authority for explicit reconciliation.
use super::*;
use crate::persistence::{
    BootAuthorization, HostProcess, InitialAdoption, InitialBoot, InitialCreation,
};
impl NativeRuntime {
    pub async fn create_initial_session(
        &self,
        journal: &Journal,
        scope: &str,
        options: BoxOptions,
        expires_unix_ms: i64,
    ) -> Result<NativeInstance> {
        let guard = self.execution_guard.as_ref().ok_or(NativeError::Conflict)?;
        let state = journal.snapshot().map_err(error)?;
        if !valid_scope(scope)
            || state.worker_id != guard.identity().worker_id
            || expires_unix_ms <= super::start::now_ms()?
            || state.current.contains_key(scope)
            || state.probes.contains_key(scope)
            || state.adoptions.contains_key(scope)
            || state.initial_boots.contains_key(scope)
            || state.initial_creations.contains_key(scope)
        {
            return Err(NativeError::Conflict);
        }
        // Verify the configured service before accepting SDK create work.
        guard.status("initial-service-check").await.map_err(error)?;
        // The local SDK derives effective limits (including RLIMIT_FSIZE)
        // before persisting config. Bind that effective configuration, then
        // require SDK create's own normalization to produce the same bytes.
        let options = tokio::task::spawn_blocking(move || {
            let mut options = options;
            options.sanitize().map_err(|_| NativeError::Unsupported)?;
            Ok::<_, NativeError>(options)
        })
        .await
        .map_err(|_| NativeError::Unavailable)??;
        let creation = InitialCreation {
            operation_id: format!("initial-{}", uuid::Uuid::new_v4()),
            scoped_session_id: scope.into(),
            guard: guard.identity().clone(),
            origin: HostProcess::current().map_err(error)?,
            expires_unix_ms,
            options_sha256: InitialCreation::options_digest(&options).map_err(error)?,
            box_id: String::new(),
            abandoned: false,
            cleanup_complete: false,
        };
        journal
            .begin_initial_creation(creation.clone())
            .map_err(error)?;
        let native = self
            .runtime
            .create(options, Some(terminal_name(scope)))
            .await
            .map_err(|_| NativeError::Unavailable)?;
        let id = native.id().as_str();
        let configured = native.info().await.map_err(|_| NativeError::Unavailable)?;
        if configured.status != BoxStatus::Configured
            || configured.pid.is_some()
            || configured.name.as_deref() != Some(&terminal_name(scope))
        {
            return Err(NativeError::Conflict);
        }
        // Configured SDK rows precede creation of their physical box directory.
        let persisted = read_config_mode(&self.home, id, false)?;
        if InitialCreation::options_digest(&persisted.options).map_err(error)?
            != creation.options_sha256
        {
            return Err(NativeError::Conflict);
        }
        journal
            .select_initial_creation(&creation, id)
            .map_err(error)?;
        let executable = self.prepare_initial_creation_executable(journal, scope)?;
        let authorization = BootAuthorization {
            operation_id: creation.operation_id,
            box_id: id.into(),
            scoped_session_id: scope.into(),
            generation: 1,
            prior_revision: None,
            expires_unix_ms,
            origin: creation.origin,
            executable,
        };
        journal
            .begin_initial_boot(InitialBoot {
                guard: guard.identity().clone(),
                authorization: authorization.clone(),
                managed: false,
            })
            .map_err(error)?;
        guard
            .prepare_boot(authorization.clone())
            .await
            .map_err(error)?;
        // Never drop an accepted SDK start on request timeout or cancellation.
        native.start().await.map_err(|_| NativeError::Unavailable)?;
        let process = self.inspect_writer(scope, id).await?;
        let lease = guard
            .bind_boot(authorization, process)
            .await
            .map_err(error)?;
        let adoption = journal
            .begin_initial_adoption(InitialAdoption {
                guard: guard.identity().clone(),
                lease,
                confirmed: false,
            })
            .map_err(error)?;
        self.confirm_initial_adoption(journal, &adoption).await?;
        self.inspect(scope, id).await
    }

    /// Prepare only a current, durably selected initial create. This returns
    /// executable evidence, never a boot grant; begin_initial_boot must still
    /// persist before any independent authorization or SDK start.
    pub fn prepare_initial_creation_executable(
        &self,
        journal: &Journal,
        scope: &str,
    ) -> Result<crate::persistence::BootExecutable> {
        let state = journal.snapshot().map_err(error)?;
        let intent = state
            .initial_creations
            .get(scope)
            .ok_or(NativeError::Conflict)?;
        if intent.abandoned
            || intent.box_id.is_empty()
            || state.initial_boots.contains_key(scope)
            || self
                .execution_guard
                .as_ref()
                .is_none_or(|guard| guard.identity() != &intent.guard)
            || intent.origin != HostProcess::current().map_err(error)?
        {
            return Err(NativeError::Conflict);
        }
        let id = intent.box_id.as_str();
        let config = read_config_mode(&self.home, id, false)?;
        if InitialCreation::options_digest(&config.options).map_err(error)? != intent.options_sha256
        {
            return Err(NativeError::Conflict);
        }
        crate::persistence::boot_executable::validate_unstarted_directory(&self.home, id)
            .map_err(error)?;
        // SDK create persists only configuration. Materialize its still-empty
        // box directory so the shim can be pinned before SDK start creates it.
        for directory in [self.home.join("boxes"), self.home.join("boxes").join(id)] {
            use std::os::unix::fs::DirBuilderExt;
            match fs::DirBuilder::new().mode(0o700).create(&directory) {
                Ok(()) => {
                    fs::File::open(directory.parent().ok_or(NativeError::Conflict)?)
                        .and_then(|f| f.sync_all())
                        .map_err(|_| NativeError::Unavailable)?;
                }
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => (),
                Err(_) => return Err(NativeError::Unavailable),
            }
        }
        self.boot_executable(id, true)
    }
}
fn error(error: crate::persistence::Error) -> NativeError {
    match error {
        crate::persistence::Error::Conflict => NativeError::Conflict,
        crate::persistence::Error::Unavailable => NativeError::Unavailable,
    }
}
