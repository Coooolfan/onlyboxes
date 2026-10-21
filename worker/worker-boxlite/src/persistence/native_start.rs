//! Controlled boots retain the selected disk and never open public admission.
use super::stop::read_shim_process;
use super::*;
use crate::persistence::{Error, Operation};

impl NativeRuntime {
    /// Caller serializes this scope, keeps admission closed and owns this task
    /// through SDK completion, even after request cancellation. The absolute
    /// lease comes from Console authority; this method never invents a renewal.
    pub async fn start_target(
        &self,
        journal: &Journal,
        operation: &str,
        lease_expires_unix_ms: i64,
        readiness_timeout: Duration,
    ) -> Result<NativeInstance> {
        self.start_controlled(
            journal,
            operation,
            Kind::Target,
            lease_expires_unix_ms,
            readiness_timeout,
        )
        .await
    }

    /// Rollback preserves the original source ID, generation and disk. A
    /// committed snapshot cannot be rolled back. RolledBack remains gated until
    /// the manager receives explicit activation authority from Console.
    pub async fn rollback_source(
        &self,
        journal: &Journal,
        operation: &str,
        lease_expires_unix_ms: i64,
        readiness_timeout: Duration,
    ) -> Result<NativeInstance> {
        self.start_controlled(
            journal,
            operation,
            Kind::Source,
            lease_expires_unix_ms,
            readiness_timeout,
        )
        .await
    }

    async fn start_controlled(
        &self,
        journal: &Journal,
        operation: &str,
        kind: Kind,
        lease: i64,
        readiness_timeout: Duration,
    ) -> Result<NativeInstance> {
        if readiness_timeout.is_zero() || readiness_timeout > Duration::from_secs(600) {
            return Err(NativeError::Conflict);
        }
        let op = current_operation(journal, operation, kind)?;
        validate_lease(&op, lease, now_ms()?)?;
        let inspected = self.inspect_start(journal, operation, kind).await?;
        let action = boot_action(&op, inspected.status)?;
        // A source stopped by this operation must have independent exit proof.
        // Never trust an SDK Stopped row after a shutdown acknowledgement loss.
        if kind == Kind::Source && action == BootAction::Start {
            let old = op.source_process.as_ref().ok_or(NativeError::Conflict)?;
            if !op.source_stop_confirmed || !old.writers_exited().map_err(journal_error)? {
                return Err(NativeError::Conflict);
            }
        }
        let running = if action == BootAction::Reuse {
            let process = read_shim_process(
                &self.home,
                &op.box_id,
                inspected.pid.ok_or(NativeError::Conflict)?,
            )?;
            if let Some(pin) = &op.started_process {
                if pin != &process {
                    return Err(NativeError::Conflict);
                }
            } else if !op.start_attempted {
                // Only a source that never stopped may reuse an uncontrolled
                // running process. A recorded stop attempt must match it.
                if kind != Kind::Source
                    || op.source_stop_confirmed
                    || op
                        .source_process
                        .as_ref()
                        .is_some_and(|pin| pin != &process)
                {
                    return Err(NativeError::Conflict);
                }
            }
            Some(process)
        } else {
            None
        };
        // Persist phase + lease + boot intent BEFORE the first possible writer.
        journal
            .update(operation, |current| {
                if !same_operation(&op, current) {
                    return Err(Error::Conflict);
                }
                if kind == Kind::Source && matches!(current.phase, Phase::Preparing | Phase::Frozen)
                {
                    current.phase = Phase::RollingBack;
                }
                current.lease_expires_unix_ms = lease;
                current.start_attempted = true;
                if let Some(process) = &running {
                    current.started_process = Some(process.clone());
                }
                Ok(())
            })
            .map_err(journal_error)?;
        let native = self
            .runtime
            .get(&op.box_id)
            .await
            .map_err(|_| NativeError::Unavailable)?
            .ok_or(NativeError::Missing)?;
        if action == BootAction::Start {
            // Do not timeout/drop SDK start: it can continue creating writers.
            // An uncertain result leaves the durable intent and exact ID held.
            if lease <= now_ms()? {
                return Err(NativeError::Conflict);
            }
            native.start().await.map_err(|_| NativeError::Unavailable)?;
        }
        let started = self.inspect_start(journal, operation, kind).await?;
        if started.status != BoxStatus::Running {
            return Err(NativeError::Conflict);
        }
        let process = read_shim_process(
            &self.home,
            &op.box_id,
            started.pid.ok_or(NativeError::Conflict)?,
        )?;
        journal
            .update(operation, |current| {
                if !same_operation(&op, current)
                    || current.lease_expires_unix_ms <= now_ms().map_err(|_| Error::Unavailable)?
                {
                    return Err(Error::Conflict);
                }
                current.started_process = Some(process.clone());
                Ok(())
            })
            .map_err(journal_error)?;
        let remaining = lease
            .checked_sub(now_ms()?)
            .filter(|ms| *ms > 0)
            .ok_or(NativeError::Conflict)?;
        let timeout = readiness_timeout.min(Duration::from_millis(remaining as u64));
        // Probe the image contract. No user request or public proxy is admitted.
        let ready = async {
            let execution = native
                .exec(boxlite::BoxCommand::new("python").args([
                    "-c",
                    "import subprocess; subprocess.run(['/bin/sh','-c','exit 0'], check=True)",
                ]))
                .await
                .map_err(|_| NativeError::Unavailable)?;
            let result = execution
                .wait()
                .await
                .map_err(|_| NativeError::Unavailable)?;
            if result.exit_code != 0 || result.error_message.as_ref().is_some_and(|m| !m.is_empty())
            {
                return Err(NativeError::Unavailable);
            }
            Ok(())
        };
        tokio::time::timeout(timeout, ready)
            .await
            .map_err(|_| NativeError::Unavailable)??;
        let ready = self.inspect_start(journal, operation, kind).await?;
        if ready.status != BoxStatus::Running
            || read_shim_process(
                &self.home,
                &op.box_id,
                ready.pid.ok_or(NativeError::Conflict)?,
            )? != process
        {
            return Err(NativeError::Conflict);
        }
        journal
            .update(operation, |current| {
                if !same_operation(&op, current)
                    || current.started_process.as_ref() != Some(&process)
                    || current.lease_expires_unix_ms <= now_ms().map_err(|_| Error::Unavailable)?
                {
                    return Err(Error::Conflict);
                }
                current.phase = match (kind, current.phase) {
                    (Kind::Target, Phase::Importing) => Phase::Ready,
                    (Kind::Source, Phase::RollingBack) => Phase::RolledBack,
                    (_, phase) => phase,
                };
                Ok(())
            })
            .map_err(journal_error)?;
        Ok(ready)
    }

    async fn inspect_start(
        &self,
        journal: &Journal,
        operation: &str,
        kind: Kind,
    ) -> Result<NativeInstance> {
        match kind {
            Kind::Target => self.inspect_target(journal, operation).await,
            Kind::Source => self.inspect_source(journal, operation).await,
            _ => Err(NativeError::Conflict),
        }
    }
}

#[derive(Debug, PartialEq, Eq)]
enum BootAction {
    Start,
    Reuse,
}

fn boot_action(op: &Operation, status: BoxStatus) -> Result<BootAction> {
    match status {
        BoxStatus::Running => Ok(BootAction::Reuse),
        BoxStatus::Stopped if !op.start_attempted && !op.active() && op.phase != Phase::Ready => {
            Ok(BootAction::Start)
        }
        // No reboot of an uncertain attempt, or of a target that was once ready.
        _ => Err(NativeError::Conflict),
    }
}
fn validate_lease(op: &Operation, lease: i64, now: i64) -> Result<()> {
    if lease <= now || lease < op.lease_expires_unix_ms {
        Err(NativeError::Conflict)
    } else {
        Ok(())
    }
}
fn current_operation(journal: &Journal, operation: &str, kind: Kind) -> Result<Operation> {
    let state = journal.snapshot().map_err(journal_error)?;
    let op = state
        .operations
        .get(operation)
        .ok_or(NativeError::Conflict)?;
    if state.current.get(&op.scoped_session_id).map(String::as_str) != Some(operation)
        || op.kind != kind
        || !valid_start_phase(kind, op.phase)
        || op.box_id.is_empty()
    {
        return Err(NativeError::Conflict);
    }
    Ok(op.clone())
}
fn same_operation(prior: &Operation, current: &Operation) -> bool {
    prior.operation_id == current.operation_id
        && prior.generation == current.generation
        && prior.box_id == current.box_id
        && prior.kind == current.kind
        && valid_start_phase(current.kind, current.phase)
}
fn valid_start_phase(kind: Kind, phase: Phase) -> bool {
    matches!(
        (kind, phase),
        (
            Kind::Source,
            Phase::Preparing | Phase::Frozen | Phase::RollingBack | Phase::RolledBack
        ) | (
            Kind::Target,
            Phase::Importing | Phase::Ready | Phase::Active
        )
    )
}
fn now_ms() -> Result<i64> {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()
        .and_then(|d| i64::try_from(d.as_millis()).ok())
        .ok_or(NativeError::Unavailable)
}
fn journal_error(error: Error) -> NativeError {
    match error {
        Error::Conflict => NativeError::Conflict,
        Error::Unavailable => NativeError::Unavailable,
    }
}
