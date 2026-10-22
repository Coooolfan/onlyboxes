//! The lifecycle journal retains the exact companion epoch before SDK start.
//! No guarded operation can be replayed through an unguarded runtime.
use super::*;
use crate::persistence::{BootAuthorization, BootExecutable, GuardedBoot, HostProcess, Operation};

impl NativeRuntime {
    /// Read-only companion check for readiness inspection and activation. The
    /// caller separately verifies the SDK instance and public execution gates.
    pub async fn verify_execution_guard(&self, journal: &Journal, operation: &str) -> Result<()> {
        let state = journal.snapshot().map_err(guard_error)?;
        let op = state
            .operations
            .get(operation)
            .ok_or(NativeError::Conflict)?;
        if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation) {
            return Err(NativeError::Conflict);
        }
        let Some(guard) = &self.execution_guard else {
            return if op.boot_guard.is_none() {
                Ok(())
            } else {
                Err(NativeError::Conflict)
            };
        };
        let binding = op.boot_guard.as_ref().ok_or(NativeError::Conflict)?;
        if binding.guard != *guard.identity() || state.worker_id != guard.identity().worker_id {
            return Err(NativeError::Conflict);
        }
        let process = op.started_process.as_ref().ok_or(NativeError::Conflict)?;
        self.guard_boot_writer(op, Some(binding), process, op.lease_expires_unix_ms, false)
            .await
    }

    pub(super) async fn boot_guard_binding(
        &self,
        journal: &Journal,
        op: &Operation,
        executable: Option<&BootExecutable>,
        lease: i64,
    ) -> Result<Option<GuardedBoot>> {
        let Some(guard) = &self.execution_guard else {
            return if op.boot_guard.is_none() {
                Ok(None)
            } else {
                Err(NativeError::Conflict)
            };
        };
        let state = journal.snapshot().map_err(guard_error)?;
        if guard.identity().worker_id != state.worker_id {
            return Err(NativeError::Conflict);
        }
        if let Some(binding) = &op.boot_guard {
            if &binding.guard != guard.identity() {
                return Err(NativeError::Conflict);
            }
            return Ok(Some(binding.clone()));
        }
        let Some(executable) = executable else {
            return Ok(Some(GuardedBoot {
                guard: guard.identity().clone(),
                authorization: None,
            }));
        };
        // Read status before recording an attempt: a failed read cannot confer
        // authority to manufacture a new predecessor revision.
        let prior = guard.status(&op.box_id).await.map_err(guard_error)?;
        if prior.as_ref().is_some_and(|lease| {
            lease.scoped_session_id != op.scoped_session_id || lease.generation != op.generation
        }) {
            return Err(NativeError::Conflict);
        }
        Ok(Some(GuardedBoot {
            guard: guard.identity().clone(),
            authorization: Some(BootAuthorization {
                operation_id: op.operation_id.clone(),
                box_id: op.box_id.clone(),
                scoped_session_id: op.scoped_session_id.clone(),
                generation: op.generation,
                prior_revision: prior.map(|lease| lease.revision),
                expires_unix_ms: lease,
                origin: HostProcess::current().map_err(guard_error)?,
                executable: executable.clone(),
            }),
        }))
    }

    pub(super) async fn guard_boot_writer(
        &self,
        op: &Operation,
        binding: Option<&GuardedBoot>,
        process: &HostProcess,
        lease: i64,
        may_bind: bool,
    ) -> Result<()> {
        let Some(guard) = &self.execution_guard else {
            return if binding.is_none() {
                Ok(())
            } else {
                Err(NativeError::Conflict)
            };
        };
        let mut current = if may_bind {
            guard.status(&op.box_id).await.map_err(guard_error)?
        } else {
            Some(guard.check_running(&op.box_id).await.map_err(guard_error)?)
        };
        let matches = |running: &crate::persistence::ExecutionLease| {
            running.box_id == op.box_id
                && running.scoped_session_id == op.scoped_session_id
                && running.generation == op.generation
                && &running.process == process
                && !running.revoked
        };
        if !current.as_ref().is_some_and(matches) && may_bind {
            let binding = binding.ok_or(NativeError::Conflict)?;
            if &binding.guard != guard.identity() {
                return Err(NativeError::Conflict);
            }
            current = Some(
                guard
                    .bind_boot(
                        binding.authorization.clone().ok_or(NativeError::Conflict)?,
                        process.clone(),
                    )
                    .await
                    .map_err(guard_error)?,
            );
        }
        let mut current = current.filter(matches).ok_or(NativeError::Conflict)?;
        if current.expires_unix_ms < lease && may_bind {
            current = guard
                .renew(
                    current.revision,
                    crate::persistence::ExecutionLease {
                        revision: current
                            .revision
                            .checked_add(1)
                            .ok_or(NativeError::Conflict)?,
                        expires_unix_ms: lease,
                        ..current.clone()
                    },
                )
                .await
                .map_err(guard_error)?;
        }
        if !matches(&current) || current.expires_unix_ms < lease {
            return Err(NativeError::Conflict);
        }
        Ok(())
    }
}

fn guard_error(error: crate::persistence::Error) -> NativeError {
    match error {
        crate::persistence::Error::Conflict => NativeError::Conflict,
        crate::persistence::Error::Unavailable => NativeError::Unavailable,
    }
}
