//! Adoption is read-only with respect to the original VM. It never starts or
//! substitutes a missing instance, and retry never refreshes the first grant.
use super::*;
use crate::persistence::{ExecutionLease, InitialAdoption};
impl NativeRuntime {
    pub async fn prepare_initial_adoption(
        &self,
        journal: &Journal,
        scope: &str,
        id: &str,
        lease: i64,
    ) -> Result<InitialAdoption> {
        let guard = self.execution_guard.as_ref().ok_or(NativeError::Conflict)?;
        let state = journal.snapshot().map_err(error)?;
        if state.worker_id != guard.identity().worker_id || state.current.contains_key(scope) {
            return Err(NativeError::Conflict);
        }
        let process = self.inspect_writer(scope, id).await?;
        journal
            .begin_initial_adoption(InitialAdoption {
                guard: guard.identity().clone(),
                lease: ExecutionLease {
                    box_id: id.into(),
                    scoped_session_id: scope.into(),
                    generation: 1,
                    revision: 1,
                    expires_unix_ms: lease,
                    process,
                    revoked: false,
                },
                confirmed: false,
            })
            .map_err(error)
    }
    pub async fn confirm_initial_adoption(
        &self,
        journal: &Journal,
        adoption: &InitialAdoption,
    ) -> Result<()> {
        let guard = self.execution_guard.as_ref().ok_or(NativeError::Conflict)?;
        if guard.identity() != &adoption.guard {
            return Err(NativeError::Conflict);
        }
        let scope = &adoption.lease.scoped_session_id;
        let id = &adoption.lease.box_id;
        if self.inspect_writer(scope, id).await? != adoption.lease.process {
            return Err(NativeError::Conflict);
        }
        if !adoption.confirmed {
            guard.adopt(adoption.lease.clone()).await.map_err(error)?;
        }
        let check = |lease: &ExecutionLease| {
            lease.box_id == *id
                && lease.scoped_session_id == *scope
                && lease.generation == 1
                && lease.process == adoption.lease.process
                && lease.expires_unix_ms >= adoption.lease.expires_unix_ms
                && !lease.revoked
        };
        if !check(&guard.check_running(id).await.map_err(error)?) {
            return Err(NativeError::Conflict);
        }
        journal.confirm_initial_adoption(adoption).map_err(error)?;
        if self.inspect_writer(scope, id).await? != adoption.lease.process
            || !check(&guard.check_running(id).await.map_err(error)?)
        {
            return Err(NativeError::Conflict);
        }
        Ok(())
    }
}
fn error(error: crate::persistence::Error) -> NativeError {
    match error {
        crate::persistence::Error::Conflict => NativeError::Conflict,
        crate::persistence::Error::Unavailable => NativeError::Unavailable,
    }
}
