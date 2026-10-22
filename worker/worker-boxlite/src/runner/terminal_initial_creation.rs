//! Managed creation holds capacity and disk on every uncertain outcome. The
//! backend must confirm the independent grant before publishing its first ID.
use super::*;
impl TerminalSessionManager {
    pub(crate) async fn create_claimed_session(
        &self,
        scope: &str,
        request_deadline: i64,
    ) -> Result<String, TerminalOperationError> {
        let guarded = {
            let mut state = self.sessions.lock().await;
            if self.closed.load(Ordering::SeqCst) || state.failed {
                return Err(unavailable());
            }
            match state.execution_guard.clone() {
                None => None,
                Some(guard) => {
                    let journal = state.journal.clone().ok_or_else(unavailable)?;
                    let expiry =
                        guard_deadline(state.get(scope).ok_or_else(conflict)?.lease_expires_at)?;
                    if request_deadline > 0 && request_deadline <= to_unix_millis(SystemTime::now())
                    {
                        return Err(TerminalOperationError::DeadlineExceeded);
                    }
                    state.pending_adoptions.insert(scope.into());
                    Some((journal, guard, expiry))
                }
            }
        };
        let Some((journal, guard, expiry)) = guarded else {
            return self
                .backend
                .create_session_box_for_session(scope, self.shutdown.clone(), request_deadline)
                .await;
        };
        let created = self
            .backend
            .create_guarded_session_box(scope, journal.clone(), guard.clone(), expiry)
            .await;
        let mut state = self.sessions.lock().await;
        let snapshot = match journal.snapshot() {
            Ok(snapshot) => snapshot,
            Err(_) => {
                state.fail();
                return Err(unavailable());
            }
        };
        if let Some(id) = snapshot.probes.get(scope) {
            state.initial_roots.insert(scope.into(), id.clone());
            state
                .protected_names
                .insert(terminal_session_resource_name(scope));
        }
        let id = created?;
        let adoption = snapshot.adoptions.get(scope).ok_or_else(unavailable)?;
        if !adoption.confirmed
            || adoption.guard != *guard.identity()
            || adoption.lease.box_id != id
            || adoption.lease.expires_unix_ms < expiry
            || !snapshot
                .initial_boots
                .get(scope)
                .is_some_and(|b| !b.managed && b.authorization.box_id == id)
        {
            return Err(unavailable());
        }
        let live = guard.check_running(&id).await.map_err(|_| unavailable())?;
        if live.scoped_session_id != scope
            || live.generation != 1
            || live.process != adoption.lease.process
            || live.expires_unix_ms < expiry
        {
            return Err(unavailable());
        }
        // Shutdown never turns an owned root into an ordinary cleanup target.
        if self.closed.load(Ordering::SeqCst) || state.failed {
            return Err(unavailable());
        }
        state.pending_adoptions.remove(scope);
        Ok(id)
    }
}
