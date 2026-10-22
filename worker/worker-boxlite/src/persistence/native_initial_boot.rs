//! The caller owns SDK create/start through completion and serializes the scope.
//! Errors retain the selected root and boot authority for explicit reconciliation.
use super::*;
use crate::persistence::{BootAuthorization, HostProcess, InitialAdoption, InitialBoot};
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
        {
            return Err(NativeError::Conflict);
        }
        // Verify the configured service before accepting SDK create work.
        guard.status("initial-service-check").await.map_err(error)?;
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
        let authorization = BootAuthorization {
            operation_id: format!("initial-{}", uuid::Uuid::new_v4()),
            box_id: id.into(),
            scoped_session_id: scope.into(),
            generation: 1,
            prior_revision: None,
            expires_unix_ms,
            origin: HostProcess::current().map_err(error)?,
            executable: self.boot_executable(id, true)?,
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
}
fn error(error: crate::persistence::Error) -> NativeError {
    match error {
        crate::persistence::Error::Conflict => NativeError::Conflict,
        crate::persistence::Error::Unavailable => NativeError::Unavailable,
    }
}
