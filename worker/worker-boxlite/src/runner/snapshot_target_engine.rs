//! Target work shares source admission, durable accounting and transfer slots.
use super::*;
use worker_boxlite::persistence::{artifact_name, guest_platform, RUNTIME_VERSION};

impl NativeControlEngine {
    pub(super) async fn inspect_other(
        &self,
        _action: &ActionContext,
        op: &Operation,
    ) -> Outcome<ResultPayload> {
        if op.kind == Kind::Cancel {
            return Ok(state_result("canceled"));
        }
        if op.kind == Kind::Delete {
            return Ok(target_result(
                op,
                if op.phase == Phase::Deleted {
                    "canceled"
                } else {
                    "busy"
                },
            ));
        }
        if op.kind != Kind::Target {
            return Err(ControlError::Unavailable);
        }
        match op.phase {
            Phase::Prepared | Phase::Importing => Ok(target_result(op, "prepared")),
            Phase::Ready | Phase::Active => {
                self.verify_running_target(op).await?;
                let active = op.phase == Phase::Active && self.active_binding(op).await;
                Ok(target_result(op, if active { "active" } else { "ready" }))
            }
            Phase::Canceling => Ok(target_result(op, "busy")),
            Phase::Canceled => Ok(target_result(op, "canceled")),
            _ => Err(ControlError::Conflict),
        }
    }

    pub(super) async fn prepare_restore(
        &self,
        action: &ActionContext,
        known: Option<Operation>,
    ) -> Outcome<ResultPayload> {
        let spec = action
            .payload
            .restore_spec
            .as_ref()
            .ok_or(ControlError::Conflict)?;
        spec.validate().map_err(|_| ControlError::Conflict)?;
        if spec.runtime_version != RUNTIME_VERSION
            || spec.platform != guest_platform().map_err(native_error)?
        {
            return Err(ControlError::Unavailable);
        }
        let archive = action
            .payload
            .archive
            .as_ref()
            .ok_or(ControlError::Conflict)?;
        archive.validate().map_err(|_| ControlError::Conflict)?;
        let requested = limits(action)?;
        if archive.size_bytes > requested.max_bytes
            || archive.expanded_bytes > requested.max_expanded_bytes
        {
            return Err(ControlError::Quota);
        }
        let ttl = self.target_ttl(action.payload.lease_ttl_sec)?;
        let op = if let Some(op) = known {
            if op.kind != Kind::Target
                || op.restore_spec.as_ref() != Some(spec)
                || op.archive.as_ref() != Some(archive)
                || op.lease_ttl_sec != ttl
                || !matches!(
                    op.phase,
                    Phase::Prepared | Phase::Importing | Phase::Ready | Phase::Active
                )
            {
                return Err(ControlError::Conflict);
            }
            op
        } else {
            let _admission = self.admission.lock().await;
            let state = self.journal.snapshot().map_err(journal_error)?;
            if has_unfinalized_probe(&state, &action.frame.scoped_session_id)
                || state
                    .current
                    .get(&action.frame.scoped_session_id)
                    .is_some_and(|id| {
                        let prior = &state.operations[id];
                        !prior.terminal()
                            || prior.kind == Kind::Delete
                            || prior.generation >= action.frame.generation
                    })
            {
                return Err(ControlError::Conflict);
            }
            let stage = target_stage(archive)?;
            self.reserve_with_cache(action, stage).await?;
            if let Err(error) = self.authorize(action).await {
                self.release(action)?;
                return Err(error);
            }
            if let Err(error) = self
                .manager
                .reserve_snapshot_target(
                    &action.frame.scoped_session_id,
                    &action.frame.operation_id,
                    action.frame.generation,
                )
                .await
            {
                self.release(action)?;
                return Err(terminal_error(error));
            }
            // Finish durable ownership after reserving capacity, even if the
            // delivery expires. An uncertain claim keeps its gate/reservation.
            let op: Operation = serde_json::from_value(serde_json::json!({
                "operation_id":action.frame.operation_id,
                "scoped_session_id":action.frame.scoped_session_id,
                "generation":action.frame.generation, "kind":"target", "phase":"prepared",
                "restore_spec":spec, "archive":archive,
                "artifact":artifact_name(&action.frame.scoped_session_id, &action.frame.operation_id, action.frame.generation),
                "lease_ttl_sec":ttl, "staging_bytes":stage,
            })).map_err(|_| ControlError::Conflict)?;
            self.journal.claim(op.clone()).map_err(journal_error)?;
            self.release(action)?;
            self.manager
                .sync_snapshot_operation(&op.operation_id)
                .await
                .map_err(terminal_error)?;
            op
        };
        self.inspect_other(action, &op).await
    }

    fn target_ttl(&self, seconds: i64) -> Outcome<i32> {
        if seconds < 0 {
            return Err(ControlError::Conflict);
        }
        let requested = if seconds == 0 {
            None
        } else {
            Some(i32::try_from(seconds).map_err(|_| ControlError::Conflict)?)
        };
        let duration = self
            .manager
            .resolve_lease_duration(requested)
            .map_err(terminal_error)?;
        i32::try_from(duration.as_secs()).map_err(|_| ControlError::Conflict)
    }

    pub(super) async fn restore(
        &self,
        action: &ActionContext,
        known: Option<Operation>,
    ) -> Outcome<ResultPayload> {
        let op = target_operation(known)?;
        let archive = op.archive.as_ref().ok_or(ControlError::Conflict)?;
        if action.payload.archive.as_ref() != Some(archive)
            || !matches!(
                op.phase,
                Phase::Prepared | Phase::Importing | Phase::Ready | Phase::Active
            )
        {
            return Err(ControlError::Conflict);
        }
        if matches!(op.phase, Phase::Ready | Phase::Active) {
            return self.inspect_other(action, &op).await;
        }
        if op.box_id.is_empty() && !self.copy_cache(action, &op).await? {
            let path = self
                .journal
                .artifact_path(&op.scoped_session_id, &op.operation_id, op.generation)
                .map_err(journal_error)?;
            let downloaded = self
                .transfer
                .download(
                    action
                        .payload
                        .download
                        .as_ref()
                        .ok_or(ControlError::Conflict)?,
                    archive,
                    path,
                    action.cancel.clone(),
                )
                .await
                .map_err(transfer_error)?;
            // Close the cache lock before native normalization. Its result has
            // already passed full SHA; native import separately verifies format,
            // spec and scope, and pins the selected disk before it can boot.
            drop(downloaded);
        }
        self.authorize(action).await?;
        let limits = target_limits(archive)?;
        self.native
            .import_target(self.journal.clone(), &op.operation_id, limits)
            .await
            .map_err(native_error)?;
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.authorize(action).await?;
        let current = self.journal.snapshot().map_err(journal_error)?;
        let current = &current.operations[&op.operation_id];
        // Replaying an accepted start retains its absolute lease. A retry is
        // never fresh authority to renew an already running inactive target.
        let lease = if current.start_attempted {
            current.lease_expires_unix_ms
        } else {
            now_ms()?
                .checked_add(i64::from(op.lease_ttl_sec) * 1000)
                .ok_or(ControlError::Conflict)?
        };
        self.native
            .start_target(
                &self.journal,
                &op.operation_id,
                lease,
                self.config.readiness_timeout,
            )
            .await
            .map_err(native_error)?;
        self.retain_cache(&op.operation_id)?;
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.authorize(action).await?;
        self.manager
            .bind_snapshot_instance(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        let state = self.journal.snapshot().map_err(journal_error)?;
        self.inspect_other(action, &state.operations[&op.operation_id])
            .await
    }

    pub(super) async fn activate(
        &self,
        action: &ActionContext,
        known: Option<Operation>,
    ) -> Outcome<ResultPayload> {
        let op = target_operation(known)?;
        if !matches!(op.phase, Phase::Ready | Phase::Active) {
            return Err(ControlError::Conflict);
        }
        self.verify_running_target(&op).await?;
        self.authorize(action).await?;
        if op.phase == Phase::Ready {
            self.journal
                .update(&op.operation_id, |current| {
                    current.phase = Phase::Active;
                    Ok(())
                })
                .map_err(journal_error)?;
        }
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.authorize(action).await?;
        self.manager
            .bind_snapshot_instance(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.trim_cache(self.config.cache_max_bytes).await?;
        self.authorize(action).await?;
        self.manager
            .activate_snapshot_instance(
                &action.connection,
                &op.scoped_session_id,
                &op.operation_id,
                op.generation,
            )
            .await
            .map_err(terminal_error)?;
        let state = self.journal.snapshot().map_err(journal_error)?;
        Ok(target_result(&state.operations[&op.operation_id], "active"))
    }

    async fn verify_running_target(&self, op: &Operation) -> Outcome<()> {
        if op.lease_expires_unix_ms <= now_ms()? {
            return Err(ControlError::Conflict);
        }
        let native = self
            .native
            .inspect_target(&self.journal, &op.operation_id)
            .await
            .map_err(native_error)?;
        let process = op.started_process.as_ref().ok_or(ControlError::Conflict)?;
        if native.status != boxlite::BoxStatus::Running
            || native.pid != Some(process.pid)
            || process.has_exited().map_err(journal_error)?
        {
            return Err(ControlError::Conflict);
        }
        self.native
            .verify_execution_guard(&self.journal, &op.operation_id)
            .await
            .map_err(native_error)?;
        Ok(())
    }

    pub(super) async fn cancel_target(
        &self,
        action: &ActionContext,
        known: Option<Operation>,
    ) -> Outcome<ResultPayload> {
        if let Some(op) = known {
            if op.kind == Kind::Cancel {
                return Ok(state_result("canceled"));
            }
            if op.kind != Kind::Target || (op.phase == Phase::Active && !action.payload.delete) {
                return Err(ControlError::Conflict);
            }
            self.authorize(action).await?;
            // Per-scope dispatcher has joined import/start before accepting
            // cancel. Explicit revocation also drains previously admitted work.
            if op.phase != Phase::Canceled && !op.box_id.is_empty() {
                self.hold_for_rollback(action, &op.box_id).await?;
                tokio::select! {
                    _ = action.cancel.cancelled() => return Err(ControlError::Busy),
                    drained = tokio::time::timeout(self.config.drain_timeout, self.drain_existing(action, &op.box_id, true)) => drained.map_err(|_| ControlError::Busy)??,
                }
            }
            self.manager
                .sync_snapshot_operation(&op.operation_id)
                .await
                .map_err(terminal_error)?;
            self.transfer.wait_io_idle().await.map_err(transfer_error)?;
            self.authorize(action).await?;
            if action.payload.delete {
                self.native
                    .revoke_target(&self.journal, &op.operation_id)
                    .await
            } else {
                self.native
                    .cancel_target(&self.journal, &op.operation_id)
                    .await
            }
            .map_err(native_error)?;
            self.retain_cache(&op.operation_id)?;
            self.manager
                .sync_snapshot_operation(&op.operation_id)
                .await
                .map_err(terminal_error)?;
            boxlite_runtime::forget_terminal_session_box(&op.box_id);
            self.manager
                .detach_snapshot_instance(&op.scoped_session_id, &op.operation_id, op.generation)
                .await
                .map_err(terminal_error)?;
            self.trim_cache(self.config.cache_max_bytes).await?;
            return Ok(target_result(&op, "canceled"));
        }
        // No native import can precede a durable target claim. Fence delayed
        // prepare deliveries without guessing a source rollback identity.
        let _admission = self.admission.lock().await;
        self.authorize(action).await?;
        {
            let sessions = self.manager.sessions.lock().await;
            let state = self.journal.snapshot().map_err(journal_error)?;
            if sessions.contains_key(&action.frame.scoped_session_id)
                || has_unfinalized_probe(&state, &action.frame.scoped_session_id)
                || state
                    .current
                    .get(&action.frame.scoped_session_id)
                    .is_some_and(|id| {
                        let prior = &state.operations[id];
                        !prior.terminal()
                            || prior.kind == Kind::Delete
                            || prior.generation >= action.frame.generation
                    })
            {
                return Err(ControlError::Conflict);
            }
            self.journal
                .cancel_unknown(
                    &action.frame.operation_id,
                    &action.frame.scoped_session_id,
                    action.frame.generation,
                )
                .map_err(journal_error)?;
        }
        let state = self.journal.snapshot().map_err(journal_error)?;
        if state.current.get(&action.frame.scoped_session_id) == Some(&action.frame.operation_id) {
            self.release(action)?;
            self.manager
                .sync_snapshot_operation(&action.frame.operation_id)
                .await
                .map_err(terminal_error)?;
            self.manager
                .detach_snapshot_instance(
                    &action.frame.scoped_session_id,
                    &action.frame.operation_id,
                    action.frame.generation,
                )
                .await
                .map_err(terminal_error)?;
        }
        Ok(state_result("canceled"))
    }
    pub(super) async fn purge_session(
        &self,
        action: &ActionContext,
        known: Option<Operation>,
    ) -> Outcome<ResultPayload> {
        let spec = action
            .payload
            .restore_spec
            .as_ref()
            .ok_or(ControlError::Conflict)?;
        spec.validate().map_err(|_| ControlError::Conflict)?;
        if action.frame.generation <= 1
            || spec.runtime_version != RUNTIME_VERSION
            || spec.platform != guest_platform().map_err(native_error)?
        {
            return Err(ControlError::Conflict);
        }
        let op = if let Some(op) = known {
            if op.kind != Kind::Delete || op.restore_spec.as_ref() != Some(spec) {
                return Err(ControlError::Conflict);
            }
            op
        } else {
            let _admission = self.admission.lock().await;
            self.authorize(action).await?;
            let mut sessions = self.manager.sessions.lock().await;
            if sessions.failed
                || !sessions.connected
                || sessions.connection.as_deref() != Some(&action.connection)
            {
                return Err(ControlError::Unavailable);
            }
            let state = self.journal.snapshot().map_err(journal_error)?;
            let prior = state
                .current
                .get(&action.frame.scoped_session_id)
                .map(|id| &state.operations[id]);
            if prior.is_some_and(|op| {
                op.kind == Kind::Delete
                    || op.generation >= action.frame.generation
                    || (op.kind != Kind::Cancel && op.restore_spec.as_ref() != Some(spec))
            }) {
                return Err(ControlError::Conflict);
            }
            let id = prior
                .filter(|op| !op.box_id.is_empty())
                .map(|op| op.box_id.clone())
                .or_else(|| state.probes.get(&action.frame.scoped_session_id).cloned())
                .unwrap_or_default();
            if sessions
                .get(&action.frame.scoped_session_id)
                .is_some_and(|session| session.box_id != id || session.destroying)
            {
                return Err(ControlError::Conflict);
            }
            let op: Operation = serde_json::from_value(serde_json::json!({
                "operation_id":action.frame.operation_id, "scoped_session_id":action.frame.scoped_session_id,
                "generation":action.frame.generation, "kind":"delete", "phase":"deleting",
                "restore_spec":spec, "box_id":id,
            })).map_err(|_| ControlError::Conflict)?;
            self.journal.claim(op.clone()).map_err(journal_error)?;
            sessions.set_fence(
                op.scoped_session_id.clone(),
                Fence {
                    operation: op.operation_id.clone(),
                    generation: op.generation,
                    box_id: op.box_id.clone(),
                    active: false,
                    held: true,
                    capacity: true,
                    terminal: false,
                    permanent: true,
                },
            );
            self.manager.persistence_changed.notify_waiters();
            op
        };
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        if op.phase != Phase::Deleted {
            tokio::select! {
                _ = action.cancel.cancelled() => return Err(ControlError::Busy),
                drained = tokio::time::timeout(self.config.drain_timeout, self.drain_existing(action, &op.box_id, true)) => drained.map_err(|_| ControlError::Busy)??,
            }
        }
        let state = self.journal.snapshot().map_err(journal_error)?;
        {
            let mut pins = self.pins.lock().map_err(|_| ControlError::Unavailable)?;
            for prior in state
                .operations
                .values()
                .filter(|prior| prior.scoped_session_id == op.scoped_session_id)
            {
                pins.remove(&prior.operation_id);
            }
        }
        self.transfer.wait_io_idle().await.map_err(transfer_error)?;
        self.authorize(action).await?;
        self.native
            .purge_session(self.journal.clone(), &op.operation_id)
            .await
            .map_err(native_error)?;
        self.release(action)?;
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        let cleaned = self.journal.snapshot().map_err(journal_error)?;
        for prior in cleaned
            .operations
            .values()
            .filter(|prior| prior.scoped_session_id == op.scoped_session_id)
        {
            boxlite_runtime::forget_terminal_session_box(&prior.box_id);
            for id in &prior.cleanup_box_ids {
                boxlite_runtime::forget_terminal_session_box(id);
            }
        }
        self.manager
            .detach_snapshot_instance(&op.scoped_session_id, &op.operation_id, op.generation)
            .await
            .map_err(terminal_error)?;
        Ok(target_result(&op, "canceled"))
    }
}

fn has_unfinalized_probe(state: &JournalState, scope: &str) -> bool {
    state.probes.get(scope).is_some_and(|id| {
        !state.operations.values().any(|op| {
            op.scoped_session_id == scope
                && op.kind == Kind::Source
                && op.phase == Phase::Finalized
                && op.box_id == *id
        })
    })
}

fn target_operation(known: Option<Operation>) -> Outcome<Operation> {
    known
        .filter(|op| op.kind == Kind::Target)
        .ok_or(ControlError::Conflict)
}
fn target_result(op: &Operation, state: &str) -> ResultPayload {
    ResultPayload {
        state: state.into(),
        restore_spec: op.restore_spec.clone(),
        archive: op.archive.clone(),
        instance_id: op.box_id.clone(),
        lease_expires_unix_ms: op.lease_expires_unix_ms,
        ..Default::default()
    }
}
fn target_limits(archive: &worker_boxlite::persistence::Archive) -> Outcome<ArchiveLimits> {
    // Canonicalizing SDK import metadata can change the compressed header size.
    let max_bytes = archive
        .size_bytes
        .checked_add(64 << 10)
        .filter(|n| *n <= MAX_BYTES)
        .ok_or(ControlError::Quota)?;
    Ok(ArchiveLimits {
        max_bytes,
        max_expanded_bytes: archive.expanded_bytes,
    })
}
fn target_stage(archive: &worker_boxlite::persistence::Archive) -> Outcome<i64> {
    let limits = target_limits(archive)?;
    let bytes = limits
        .max_bytes
        .checked_mul(3)
        .and_then(|n| {
            archive
                .expanded_bytes
                .checked_mul(2)
                .and_then(|disk| n.checked_add(disk))
        })
        .ok_or(ControlError::Quota)?;
    bytes
        .checked_add(bytes / 5)
        .filter(|n| *n <= MAX_BYTES)
        .ok_or(ControlError::Quota)
}
