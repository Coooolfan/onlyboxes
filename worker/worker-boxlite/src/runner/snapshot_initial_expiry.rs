//! A never-managed first boot keeps ordinary execution-TTL cleanup semantics.
//! Guard revocation, complete writer exit and a durable deny fence precede IO.
use super::*;
use worker_boxlite::persistence::HostProcess;

impl NativeControlEngine {
    pub(super) async fn expire_initials(&self, cancel: &CancellationToken) -> Outcome<()> {
        let snapshot = self.journal.snapshot().map_err(journal_error)?;
        let now = now_ms()?;
        let mut scopes = std::collections::BTreeSet::new();
        for op in snapshot
            .operations
            .values()
            .filter(|op| op.initial_expiry.is_some() && op.phase == Phase::Deleting)
        {
            scopes.insert(op.scoped_session_id.clone());
        }
        for (scope, boot) in &snapshot.initial_boots {
            if boot.managed
                || snapshot.current.contains_key(scope)
                || !snapshot.adoptions.get(scope).is_some_and(|adoption| {
                    adoption.confirmed && adoption.lease.expires_unix_ms <= now
                })
            {
                continue;
            }
            scopes.insert(scope.clone());
        }
        let mut scopes: Vec<_> = scopes.into_iter().collect();
        if scopes.is_empty() {
            return Ok(());
        }
        // Round-robin bounded attempts: an invalid root or an old companion
        // epoch cannot starve independently eligible scopes indefinitely.
        let offset = self.initial_expiry_cursor.fetch_add(4, Ordering::SeqCst) % scopes.len();
        scopes.rotate_left(offset);
        let mut pending = false;
        for scope in scopes.into_iter().take(4) {
            if cancel.is_cancelled() {
                break;
            }
            let current = snapshot
                .current
                .get(&scope)
                .and_then(|id| snapshot.operations.get(id));
            let work = if let Some(op) = current {
                self.finish_initial_expiry(op).await
            } else {
                match self.claim_initial_expiry(&scope).await {
                    Ok(Some(op)) => self.finish_initial_expiry(&op).await,
                    Ok(None) => Ok(()),
                    Err(error) => Err(error),
                }
            };
            pending |= work.is_err();
        }
        if pending {
            Err(ControlError::Unavailable)
        } else {
            Ok(())
        }
    }

    async fn claim_initial_expiry(&self, scope: &str) -> Outcome<Option<Operation>> {
        // Same lock as creation, renewal, first snapshot adoption and proxy
        // admission. Do not select cancellation against accepted SDK work.
        let mut sessions = self.manager.sessions.lock().await;
        if self.manager.closed.load(Ordering::SeqCst)
            || sessions.failed
            || self.failed.load(Ordering::SeqCst)
        {
            return Err(ControlError::Unavailable);
        }
        let now = now_ms()?;
        let state = self.journal.snapshot().map_err(journal_error)?;
        let Some(boot) = state.initial_boots.get(scope).filter(|boot| !boot.managed) else {
            return Ok(None);
        };
        let Some(adoption) = state
            .adoptions
            .get(scope)
            .filter(|adoption| adoption.confirmed)
        else {
            return Ok(None);
        };
        if state.current.contains_key(scope)
            || state.probes.get(scope) != Some(&boot.authorization.box_id)
            || adoption.lease.expires_unix_ms > now
            || sessions.get(scope).is_some_and(|session| {
                session.box_id != adoption.lease.box_id
                    || session.inflight != 0
                    || session.proxy_inflight.load(Ordering::SeqCst) != 0
                    || to_unix_millis(session.lease_expires_at) > now
            })
        {
            return Ok(None);
        }
        let Some(guard) = self.native.execution_guard() else {
            return Err(ControlError::Unavailable);
        };
        // A different guardian epoch cannot acquire automatic deletion authority.
        if guard.identity() != &boot.guard || guard.identity() != &adoption.guard {
            return Ok(None);
        }
        let origin = &boot.authorization.origin;
        if *origin != HostProcess::current().map_err(journal_error)?
            && !origin.has_exited().map_err(journal_error)?
        {
            return Ok(None);
        }
        if guard
            .boot_status(&adoption.lease.box_id)
            .await
            .map_err(journal_error)?
            != Some(boot.authorization.clone())
        {
            return Ok(None);
        }
        let Some(lease) = guard
            .status(&adoption.lease.box_id)
            .await
            .map_err(journal_error)?
        else {
            return Ok(None);
        };
        if !lease.revoked
            || lease.generation != 1
            || lease.scoped_session_id != scope
            || lease.box_id != adoption.lease.box_id
            || lease.process != adoption.lease.process
            || lease.expires_unix_ms > now
            || lease.expires_unix_ms < adoption.lease.expires_unix_ms
            || lease.revision < adoption.lease.revision
            || !lease.process.has_exited().map_err(journal_error)?
            || !lease.process.writers_exited().map_err(journal_error)?
        {
            return Ok(None);
        }
        // Read and bind the exact immutable native configuration before claiming
        // deletion. Unsupported/missing configuration retains the root.
        let native = self
            .native
            .inspect(scope, &lease.box_id)
            .await
            .map_err(native_error)?;
        if native.status != boxlite::BoxStatus::Stopped || native.pid.is_some() {
            return Ok(None);
        }
        let op: Operation = serde_json::from_value(serde_json::json!({
            "operation_id":format!("initial-expiry-{}", uuid::Uuid::new_v4()),
            "scoped_session_id":scope, "generation":2, "kind":"delete", "phase":"deleting",
            "restore_spec":native.spec, "box_id":lease.box_id, "initial_expiry":lease,
        }))
        .map_err(|_| ControlError::Conflict)?;
        self.journal.claim(op.clone()).map_err(journal_error)?;
        sessions.set_fence(
            scope.into(),
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
        sessions.initial_roots.remove(scope);
        sessions.bootstrap.remove(scope);
        sessions.pending_adoptions.remove(scope);
        self.manager.persistence_changed.notify_waiters();
        Ok(Some(op))
    }

    async fn finish_initial_expiry(&self, op: &Operation) -> Outcome<()> {
        if self.manager.closed.load(Ordering::SeqCst)
            || self.failed.load(Ordering::SeqCst)
            || self.manager.sessions.lock().await.failed
        {
            return Err(ControlError::Unavailable);
        }
        let state = self.journal.snapshot().map_err(journal_error)?;
        let boot = state
            .initial_boots
            .get(&op.scoped_session_id)
            .ok_or(ControlError::Conflict)?;
        if self
            .native
            .execution_guard()
            .is_none_or(|guard| guard.identity() != &boot.guard)
        {
            return Err(ControlError::Unavailable);
        }
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.native
            .purge_session(self.journal.clone(), &op.operation_id)
            .await
            .map_err(native_error)?;
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.manager
            .detach_snapshot_instance(&op.scoped_session_id, &op.operation_id, op.generation)
            .await
            .map_err(terminal_error)?;
        tracing::info!(operation_id = %op.operation_id, "expired unmanaged initial BoxLite root reclaimed");
        Ok(())
    }
}
