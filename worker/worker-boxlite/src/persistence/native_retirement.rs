//! The cleanup receipt is journaled before requesting a durable exit proof.
//! Absence is never substituted for a release acknowledgement.
use super::cleanup::cleanup_operation;
use super::*;
use crate::persistence::{GuardedRetirement, LeaseRetirement};

impl NativeRuntime {
    /// Turn positive orphan-writer evidence into a deny-only ledger entry.
    /// Called before SDK stop so the guard can inspect the live executable; a
    /// retry may instead use an observation retained by that exact guard epoch.
    pub(super) async fn revoke_orphan_boot_for_cleanup(
        &self,
        journal: &Journal,
        operation: &str,
        id: &str,
        process: &crate::persistence::HostProcess,
    ) -> Result<()> {
        let Some(guard) = &self.execution_guard else {
            return Ok(());
        };
        let (state, op) = cleanup_operation(journal, operation)?;
        if guard.identity().worker_id != state.worker_id {
            return Err(NativeError::Conflict);
        }
        if guard
            .status(id)
            .await
            .map_err(guard_error)?
            .is_some_and(|lease| {
                lease.process == *process
                    && lease.scoped_session_id == op.scoped_session_id
                    && lease.generation <= op.generation
                    && (op.kind == Kind::Delete || lease.generation == op.generation)
            })
        {
            return Ok(());
        }
        let Some(authorization) = guard.boot_status(id).await.map_err(guard_error)? else {
            return Ok(());
        };
        let recognized = state.operations.values().any(|old| {
            old.scoped_session_id == op.scoped_session_id
                && old.box_id == id
                && old.generation == authorization.generation
                && old.generation <= op.generation
                && old.boot_guard.as_ref().is_some_and(|binding| {
                    same_store(&binding.guard, guard.identity())
                        && binding.authorization.as_ref() == Some(&authorization)
                })
        });
        if !recognized {
            return Err(NativeError::Conflict);
        }
        guard
            .revoke_boot_writer(authorization, process.clone())
            .await
            .map_err(guard_error)?;
        Ok(())
    }

    pub(super) async fn retire_native_lease(
        &self,
        journal: &Journal,
        operation: &str,
        id: &str,
    ) -> Result<Option<GuardedRetirement>> {
        let (state, op) = cleanup_operation(journal, operation)?;
        let saved = op.lease_retirements.get(id).cloned().or_else(|| {
            state
                .operations
                .values()
                .filter(|old| {
                    op.kind == Kind::Delete
                        && old.scoped_session_id == op.scoped_session_id
                        && old.generation < op.generation
                })
                .find_map(|old| old.lease_retirements.get(id).cloned())
        });
        let protected = state.operations.values().any(|old| {
            old.scoped_session_id == op.scoped_session_id
                && (old.box_id == id && old.boot_guard.is_some()
                    || old.lease_retirements.contains_key(id))
        });
        let had_writer = state.operations.values().any(|old| {
            old.scoped_session_id == op.scoped_session_id
                && (old.box_id == id
                    && (matches!(old.kind, Kind::Source | Kind::Adopt) || old.start_attempted)
                    || old.cleanup_processes.contains_key(id))
        });
        let Some(guard) = &self.execution_guard else {
            return if !protected && saved.is_none() {
                Ok(None)
            } else {
                Err(NativeError::Conflict)
            };
        };
        if guard.identity().worker_id != state.worker_id {
            return Err(NativeError::Conflict);
        }
        if let Some(process) = op.cleanup_processes.get(id).or_else(|| {
            (op.box_id == id)
                .then_some(op.started_process.as_ref())
                .flatten()
        }) {
            self.revoke_orphan_boot_for_cleanup(journal, operation, id, process)
                .await?;
        }
        let receipt = if let Some(saved) = saved {
            if &saved.guard != guard.identity()
                && (!same_store(&saved.guard, guard.identity())
                    || !guard
                        .retirement_status(id)
                        .await
                        .map_err(guard_error)?
                        .is_some_and(|status| status.retirement == saved.retirement))
            {
                return Err(NativeError::Conflict);
            }
            saved
        } else if let Some(lease) = guard.status(id).await.map_err(guard_error)? {
            if lease.scoped_session_id != op.scoped_session_id
                || lease.generation > op.generation
                || op.kind != Kind::Delete && lease.generation != op.generation
            {
                return Err(NativeError::Conflict);
            }
            GuardedRetirement {
                guard: guard.identity().clone(),
                retirement: LeaseRetirement {
                    operation_id: operation.into(),
                    cleanup_generation: op.generation,
                    box_id: id.into(),
                    scoped_session_id: op.scoped_session_id.clone(),
                    generation: lease.generation,
                    revision: lease.revision,
                    process: lease.process,
                },
            }
        } else {
            if protected
                || had_writer
                || guard.boot_status(id).await.map_err(guard_error)?.is_some()
                || guard
                    .retirement_status(id)
                    .await
                    .map_err(guard_error)?
                    .is_some()
            {
                return Err(NativeError::Conflict);
            }
            return Ok(None);
        };
        journal
            .update(operation, |current| {
                current.cleanup_box_ids.insert(id.into());
                if current
                    .lease_retirements
                    .get(id)
                    .is_some_and(|prior| prior != &receipt)
                {
                    return Err(crate::persistence::Error::Conflict);
                }
                current.lease_retirements.insert(id.into(), receipt.clone());
                Ok(())
            })
            .map_err(guard_error)?;
        guard
            .prepare_retirement(receipt.retirement.clone())
            .await
            .map_err(guard_error)?;
        Ok(Some(receipt))
    }
    pub(super) async fn release_native_lease(
        &self,
        receipt: Option<GuardedRetirement>,
    ) -> Result<()> {
        if let Some(receipt) = receipt {
            let guard = self.execution_guard.as_ref().ok_or(NativeError::Conflict)?;
            if !same_store(&receipt.guard, guard.identity()) {
                return Err(NativeError::Conflict);
            }
            guard
                .finish_retirement(receipt.retirement)
                .await
                .map_err(guard_error)?;
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

// A recorded full-subtree stop remains true when a later authorized boot uses
// the same cgroup inode. Re-querying its current population cannot describe an
// older VM epoch. The old PID lifecycle must still be over on this host.
pub(super) fn stopped_epoch(
    state: &crate::persistence::JournalState,
    scope: &str,
    id: &str,
    process: &crate::persistence::HostProcess,
) -> bool {
    state.operations.values().any(|old| {
        old.scoped_session_id == scope
            && old.box_id == id
            && old.source_stop_confirmed
            && old.source_process.as_ref() == Some(process)
    }) && process.has_exited() == Ok(true)
}

fn same_store(
    a: &crate::persistence::LeaseGuardIdentity,
    b: &crate::persistence::LeaseGuardIdentity,
) -> bool {
    a.worker_id == b.worker_id && a.store_id == b.store_id && a.process.host_id == b.process.host_id
}
