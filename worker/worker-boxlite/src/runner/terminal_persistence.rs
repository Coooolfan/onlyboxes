//! Lifecycle admission shares the session lock with creation, leases and cleanup.
use super::*;
use crate::proto::registryv1::{
    sandbox_persistence_recovery_candidate, SandboxPersistenceRecoveryCandidate,
};
use std::ops::{Deref, DerefMut};
use worker_boxlite::persistence::{Journal, Kind, Phase};

#[derive(Clone)]
pub(super) struct Fence {
    operation: String,
    generation: i64,
    pub(super) box_id: String,
    active: bool,
    held: bool,
    capacity: bool,
    terminal: bool,
    permanent: bool,
}
#[derive(Default)]
pub(super) struct SessionState {
    entries: HashMap<String, TerminalSession>,
    pub(super) fences: HashMap<String, Fence>,
    pub(super) bootstrap: HashMap<String, String>,
    pub(super) journal: Option<Arc<Journal>>,
    pub(super) protected_names: HashSet<String>,
    connection: Option<String>,
    connected: bool,
    pub(super) failed: bool,
}
impl Deref for SessionState {
    type Target = HashMap<String, TerminalSession>;
    fn deref(&self) -> &Self::Target {
        &self.entries
    }
}
impl DerefMut for SessionState {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.entries
    }
}
pub(super) fn conflict() -> TerminalOperationError {
    TerminalExecError {
        code: "session_state_conflict".into(),
        message: "sandbox lifecycle or generation does not allow execution".into(),
    }
    .into()
}
fn unavailable() -> TerminalOperationError {
    TerminalExecError {
        code: "persistence_unavailable".into(),
        message: "sandbox lifecycle journal is unavailable".into(),
    }
    .into()
}
fn identity(scope: &str, operation: &str, generation: i64) -> Result<(), TerminalOperationError> {
    let parts: Vec<_> = scope.splitn(3, ':').collect();
    if scope.len() > 1024
        || parts.len() != 3
        || parts[0] != "obx"
        || parts[1].is_empty()
        || parts[2].is_empty()
        || scope.trim() != scope
        || scope.bytes().any(|b| matches!(b, 0 | b'\r' | b'\n'))
        || operation.is_empty()
        || operation.len() > 128
        || !operation
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        || generation <= 0
    {
        return Err(conflict());
    }
    Ok(())
}
impl SessionState {
    pub(super) fn fail(&mut self) {
        self.failed = true;
        for session in self.entries.values() {
            session.proxy_cancel.cancel();
        }
    }
    pub(super) fn preserve(&self, scope: &str) -> bool {
        self.failed || self.fences.get(scope).is_some_and(|f| f.held)
    }
    pub(super) fn additional_reservations(&self, retiring: &HashMap<String, u32>) -> u32 {
        self.fences
            .iter()
            .filter(|(scope, f)| {
                f.capacity
                    && !retiring.contains_key(*scope)
                    && !self
                        .entries
                        .get(*scope)
                        .is_some_and(|s| s.capacity_reserved)
            })
            .count()
            .min(u32::MAX as usize) as u32
    }
    pub(super) fn check_access(
        &self,
        scope: &str,
        generation: i64,
    ) -> Result<(), TerminalOperationError> {
        if self.failed {
            return Err(unavailable());
        }
        if generation < 0 {
            return Err(conflict());
        }
        match self.fences.get(scope) {
            Some(fence) => {
                if !self.connected || !fence.active || fence.generation != generation {
                    return Err(conflict());
                }
                let session = self.entries.get(scope).ok_or_else(conflict)?;
                if session.box_id != fence.box_id
                    || session.destroying
                    || session.persistence_fault
                    || session.lease_expires_at <= SystemTime::now()
                {
                    return Err(conflict());
                }
            }
            None if generation != 0 => {
                let session = self.entries.get(scope).ok_or_else(conflict)?;
                if generation != 1
                    || !self.connected
                    || self.bootstrap.get(scope) != Some(&session.box_id)
                    || session.destroying
                    || session.box_id.is_empty()
                    || session.lease_expires_at <= SystemTime::now()
                {
                    return Err(conflict());
                }
            }
            None => {
                if self.bootstrap.contains_key(scope) {
                    return Err(conflict());
                }
            }
        }
        Ok(())
    }
    pub(super) fn check_command_access(
        &self,
        scope: &str,
        generation: i64,
        connection: Option<&str>,
    ) -> Result<(), TerminalOperationError> {
        self.check_access(scope, generation)?;
        if (self.fences.contains_key(scope) || self.bootstrap.contains_key(scope))
            && (connection.is_none() || connection != self.connection.as_deref())
        {
            return Err(conflict());
        }
        Ok(())
    }
    pub(super) fn persist_lease(
        &mut self,
        scope: &str,
        generation: i64,
        target: SystemTime,
    ) -> Result<(), TerminalOperationError> {
        let Some(fence) = self.fences.get(scope) else {
            return Ok(());
        };
        if self
            .entries
            .get(scope)
            .is_some_and(|s| s.lease_expires_at >= target)
        {
            return Ok(());
        }
        let Some(journal) = self.journal.as_ref() else {
            self.fail();
            return Err(unavailable());
        };
        let id = fence.operation.clone();
        let box_id = fence.box_id.clone();
        let result = journal.update(&id, |op| {
            if !op.active()
                || op.scoped_session_id != scope
                || op.generation != generation
                || op.box_id != box_id
            {
                return Err(worker_boxlite::persistence::Error::Conflict);
            }
            op.lease_expires_unix_ms = op.lease_expires_unix_ms.max(to_unix_millis(target));
            Ok(())
        });
        if result.is_err() {
            self.fail();
            return Err(unavailable());
        }
        Ok(())
    }
    fn read_journal(
        &mut self,
    ) -> Result<worker_boxlite::persistence::JournalState, TerminalOperationError> {
        let result = self.journal.as_ref().ok_or_else(unavailable)?.snapshot();
        match result {
            Ok(snapshot) => Ok(snapshot),
            Err(_) => {
                self.fail();
                Err(unavailable())
            }
        }
    }
    pub(super) fn journal_lease(
        &mut self,
        scope: &str,
    ) -> Result<SystemTime, TerminalOperationError> {
        let snapshot = self.read_journal()?;
        let Some(id) = snapshot.current.get(scope) else {
            return self
                .entries
                .get(scope)
                .map(|s| s.lease_expires_at)
                .ok_or_else(conflict);
        };
        system_time_from_unix_millis(snapshot.operations[id].lease_expires_unix_ms)
            .ok_or_else(conflict)
    }
    fn set_fence(&mut self, scope: String, fence: Fence) {
        if let Some(session) = self.entries.get_mut(&scope) {
            if !fence.active {
                session.proxy_cancel.cancel();
            }
        }
        self.fences.insert(scope, fence);
    }
}

pub(crate) struct ProxyPermit {
    pub(crate) box_id: String,
    pub(crate) cancel: CancellationToken,
    count: Arc<AtomicU32>,
    changed: Arc<Notify>,
}
impl Drop for ProxyPermit {
    fn drop(&mut self) {
        let previous = self.count.fetch_sub(1, Ordering::SeqCst);
        debug_assert!(previous > 0, "proxy reservation underflow");
        self.changed.notify_waiters();
    }
}

// Entry points used by the lifecycle coordinator; all native side effects stay
// outside the lock and follow the durable operation claim.
#[allow(dead_code)]
impl TerminalSessionManager {
    pub(crate) async fn disconnect_current_persistence(&self) {
        let connection = self.sessions.lock().await.connection.clone();
        if let Some(connection) = connection {
            self.disconnect_persistence(&connection).await;
        }
    }
    pub(crate) async fn has_snapshot_journal(&self) -> bool {
        self.sessions.lock().await.journal.is_some()
    }

    pub(crate) async fn install_snapshot_journal(
        &self,
        journal: Arc<Journal>,
    ) -> Result<(), TerminalOperationError> {
        let snapshot = match journal.snapshot() {
            Ok(state) => state,
            Err(_) => {
                self.sessions.lock().await.fail();
                return Err(unavailable());
            }
        };
        let mut state = self.sessions.lock().await;
        if self.closed.load(Ordering::SeqCst) {
            return Err(unavailable());
        }
        for (scope, id) in &snapshot.current {
            let op = &snapshot.operations[id];
            if state.entries.get(scope).is_some_and(|s| {
                !s.box_id.is_empty() && !op.box_id.is_empty() && s.box_id != op.box_id
            }) {
                state.fail();
                return Err(conflict());
            }
        }
        let mut fences = HashMap::new();
        let mut names = HashSet::new();
        for (scope, id) in &snapshot.current {
            let op = &snapshot.operations[id];
            names.insert(terminal_session_resource_name(scope));
            for origin in snapshot
                .operations
                .values()
                .filter(|o| o.scoped_session_id == *scope && o.kind == Kind::Target)
            {
                names.insert(worker_boxlite::persistence::target_name(
                    scope,
                    &origin.operation_id,
                    origin.generation,
                ));
            }
            fences.insert(
                scope.clone(),
                Fence {
                    operation: op.operation_id.clone(),
                    generation: op.generation,
                    box_id: op.box_id.clone(),
                    active: op.active(),
                    held: op.held(),
                    capacity: !op.terminal(),
                    terminal: op.terminal(),
                    permanent: op.kind == Kind::Delete,
                },
            );
            if let Some(session) = state.entries.get_mut(scope) {
                session.proxy_cancel.cancel();
                if op.active() {
                    session.lease_expires_at =
                        system_time_from_unix_millis(op.lease_expires_unix_ms)
                            .ok_or_else(conflict)?;
                }
            }
        }
        for (scope, id) in &snapshot.probes {
            if state.entries.get(scope).is_some_and(|s| s.box_id != *id) {
                state.fail();
                return Err(conflict());
            }
            names.insert(terminal_session_resource_name(scope));
        }
        state.bootstrap = snapshot.probes.into_iter().collect();
        state.fences = fences;
        state.protected_names = names;
        state.journal = Some(journal);
        state.connected = false;
        // A poisoned manager stays closed until reconstructed with a healthy journal.
        Ok(())
    }

    pub(crate) async fn protect_persistence_recovery(
        &self,
        connection: &str,
        ordinary: &[TerminalSessionRecoveryCandidate],
        held: &[SandboxPersistenceRecoveryCandidate],
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        state.connected = false;
        for (scope, session) in &state.entries {
            if state.fences.contains_key(scope) || state.bootstrap.contains_key(scope) {
                session.proxy_cancel.cancel();
            }
        }
        let valid = (!connection.trim().is_empty()
            && connection.trim() == connection
            && connection.len() <= 256)
            && ordinary.iter().all(|c| c.generation >= 0);
        if !valid || state.failed || self.closed.load(Ordering::SeqCst) {
            state.fail();
            return Err(unavailable());
        }
        if state.journal.is_none()
            && (!held.is_empty() || ordinary.iter().any(|c| c.generation != 0))
        {
            state.fail();
            return Err(unavailable());
        }
        let mut seen = HashSet::new();
        for hint in held {
            if identity(&hint.scoped_session_id, &hint.operation_id, hint.generation).is_err()
                || !matches!(
                    sandbox_persistence_recovery_candidate::Role::try_from(hint.role),
                    Ok(sandbox_persistence_recovery_candidate::Role::Source
                        | sandbox_persistence_recovery_candidate::Role::Target)
                )
                || !seen.insert(hint.scoped_session_id.clone())
                || state.fences.get(&hint.scoped_session_id).is_some_and(|f| {
                    f.generation > hint.generation
                        || (f.permanent
                            && (f.operation != hint.operation_id
                                || f.generation != hint.generation))
                })
            {
                state.fail();
                return Err(conflict());
            }
        }
        state.connection = Some(connection.into());
        for hint in held {
            state
                .protected_names
                .insert(terminal_session_resource_name(&hint.scoped_session_id));
            state
                .protected_names
                .insert(worker_boxlite::persistence::target_name(
                    &hint.scoped_session_id,
                    &hint.operation_id,
                    hint.generation,
                ));
            if state.fences.get(&hint.scoped_session_id).is_some_and(|f| {
                f.operation == hint.operation_id
                    && f.generation == hint.generation
                    && (f.active || f.terminal || f.permanent)
            }) {
                continue;
            }
            let box_id = state
                .fences
                .get(&hint.scoped_session_id)
                .map(|f| f.box_id.clone())
                .unwrap_or_default();
            state.set_fence(
                hint.scoped_session_id.clone(),
                Fence {
                    operation: hint.operation_id.clone(),
                    generation: hint.generation,
                    box_id,
                    active: false,
                    held: true,
                    capacity: true,
                    terminal: false,
                    permanent: false,
                },
            );
        }
        Ok(())
    }

    pub(crate) async fn finish_persistence_recovery(
        &self,
        connection: &str,
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        if state.failed
            || state.connection.as_deref() != Some(connection)
            || self.closed.load(Ordering::SeqCst)
        {
            return Err(unavailable());
        }
        state.connected = true;
        let active: HashSet<_> = state
            .fences
            .iter()
            .filter(|(_, f)| f.active)
            .map(|(s, _)| s.clone())
            .collect();
        let known: HashSet<_> = state.fences.keys().cloned().collect();
        for (scope, session) in &mut state.entries {
            if (!known.contains(scope) || active.contains(scope))
                && session.proxy_cancel.is_cancelled()
            {
                session.proxy_cancel = CancellationToken::new();
            }
        }
        Ok(())
    }

    pub(crate) async fn disconnect_persistence(&self, connection: &str) {
        let mut state = self.sessions.lock().await;
        if state.connection.as_deref() != Some(connection) {
            return;
        }
        state.connected = false;
        for (scope, session) in &state.entries {
            if state.fences.contains_key(scope) {
                session.proxy_cancel.cancel();
            }
        }
        self.persistence_changed.notify_waiters();
    }

    pub(crate) async fn hold_snapshot_source(
        &self,
        scope: &str,
        operation: &str,
        generation: i64,
    ) -> Result<String, TerminalOperationError> {
        identity(scope, operation, generation)?;
        let mut state = self.sessions.lock().await;
        if state.failed || !state.connected || self.closed.load(Ordering::SeqCst) {
            return Err(unavailable());
        }
        if state.fences.get(scope).is_some_and(|f| {
            f.generation != generation
                || (!f.active && f.operation != operation)
                || f.terminal
                || f.permanent
        }) {
            return Err(conflict());
        }
        let session = state.entries.get(scope).ok_or_else(conflict)?;
        if session.destroying
            || session.box_id.is_empty()
            || (session.lease_expires_at <= SystemTime::now()
                && !state
                    .fences
                    .get(scope)
                    .is_some_and(|f| f.held && f.operation == operation))
        {
            return Err(conflict());
        }
        let box_id = session.box_id.clone();
        state.set_fence(
            scope.into(),
            Fence {
                operation: operation.into(),
                generation,
                box_id: box_id.clone(),
                active: false,
                held: true,
                capacity: true,
                terminal: false,
                permanent: false,
            },
        );
        self.persistence_changed.notify_waiters();
        Ok(box_id)
    }

    pub(crate) async fn drain_snapshot_source(
        &self,
        scope: &str,
        operation: &str,
        generation: i64,
    ) -> Result<(), TerminalOperationError> {
        loop {
            let changed = self.persistence_changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            {
                let state = self.sessions.lock().await;
                let fence = state.fences.get(scope).ok_or_else(conflict)?;
                if state.failed
                    || self.closed.load(Ordering::SeqCst)
                    || fence.operation != operation
                    || fence.generation != generation
                    || !fence.held
                {
                    return Err(conflict());
                }
                let session = state.entries.get(scope).ok_or_else(conflict)?;
                if session.persistence_fault {
                    return Err(TerminalOperationError::ExecutionFailed(
                        "accepted command failed while draining snapshot source".into(),
                    ));
                }
                if session.inflight == 0 && session.proxy_inflight.load(Ordering::SeqCst) == 0 {
                    return Ok(());
                }
            }
            changed.await;
        }
    }

    pub(crate) async fn reserve_snapshot_target(
        &self,
        scope: &str,
        operation: &str,
        generation: i64,
    ) -> Result<(), TerminalOperationError> {
        identity(scope, operation, generation)?;
        let mut state = self.sessions.lock().await;
        if state.failed || !state.connected || self.closed.load(Ordering::SeqCst) {
            return Err(unavailable());
        }
        if let Some(f) = state.fences.get(scope) {
            if f.permanent {
                return Err(conflict());
            }
            if f.operation == operation && f.generation == generation && !f.active && !f.terminal {
                return Ok(());
            }
            if f.permanent || !f.terminal || f.generation >= generation {
                return Err(conflict());
            }
        }
        if state.entries.contains_key(scope) {
            return Err(conflict());
        }
        if self.max_active_sessions > 0
            && self.reservation_count(&state) >= self.max_active_sessions
        {
            return Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_SESSION_CAPACITY_EXCEEDED.into(),
                message: TERMINAL_EXEC_CAPACITY_MESSAGE.into(),
            }
            .into());
        }
        state.set_fence(
            scope.into(),
            Fence {
                operation: operation.into(),
                generation,
                box_id: String::new(),
                active: false,
                held: true,
                capacity: true,
                terminal: false,
                permanent: false,
            },
        );
        Ok(())
    }

    pub(crate) async fn bind_snapshot_instance(
        &self,
        operation: &str,
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        if state.failed || self.closed.load(Ordering::SeqCst) {
            return Err(unavailable());
        }
        let snapshot = state.read_journal()?;
        let op = snapshot.operations.get(operation).ok_or_else(conflict)?;
        if snapshot
            .current
            .get(&op.scoped_session_id)
            .map(String::as_str)
            != Some(operation)
            || !matches!(op.phase, Phase::Ready | Phase::Active | Phase::RolledBack)
            || op.box_id.is_empty()
            || op.lease_expires_unix_ms <= to_unix_millis(SystemTime::now())
        {
            return Err(conflict());
        }
        let fence = state
            .fences
            .get(&op.scoped_session_id)
            .ok_or_else(conflict)?;
        if fence.operation != operation
            || fence.generation != op.generation
            || (!fence.box_id.is_empty() && fence.box_id != op.box_id)
        {
            return Err(conflict());
        }
        // SDK stop invalidates old LiteBox handles, including the ordinary
        // terminal cache. Refresh only the selected, already-running ID before
        // binding/rollback can be activated. This path never starts a box.
        if self
            .backend
            .recover_pinned_session_box(&op.box_id)
            .await?
            .as_deref()
            != Some(op.box_id.as_str())
        {
            return Err(conflict());
        }
        if let Some(session) = state.entries.get(&op.scoped_session_id) {
            if session.box_id != op.box_id || session.destroying {
                return Err(conflict());
            }
            return Ok(());
        }
        let (ready_tx, _) = watch::channel(SessionReadyState::Ready(op.box_id.clone()));
        state.entries.insert(
            op.scoped_session_id.clone(),
            TerminalSession {
                session_id: op.scoped_session_id.clone(),
                box_id: op.box_id.clone(),
                lease_expires_at: system_time_from_unix_millis(op.lease_expires_unix_ms)
                    .ok_or_else(conflict)?,
                inflight: 0,
                destroying: false,
                capacity_reserved: true,
                ready_tx,
                proxy_cancel: CancellationToken::new(),
                proxy_inflight: Arc::new(AtomicU32::new(0)),
                persistence_fault: false,
            },
        );
        state
            .fences
            .get_mut(&op.scoped_session_id)
            .ok_or_else(conflict)?
            .box_id = op.box_id.clone();
        self.active_session_reservations
            .fetch_add(1, Ordering::SeqCst);
        Ok(())
    }

    pub(crate) async fn sync_snapshot_operation(
        &self,
        operation: &str,
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        let snapshot = state.read_journal()?;
        let op = snapshot.operations.get(operation).ok_or_else(conflict)?;
        if snapshot
            .current
            .get(&op.scoped_session_id)
            .map(String::as_str)
            != Some(operation)
        {
            return Err(conflict());
        }
        if state.fences.get(&op.scoped_session_id).is_some_and(|f| {
            f.generation > op.generation || (f.permanent && op.kind != Kind::Delete)
        }) {
            state.fail();
            return Err(conflict());
        }
        let opened = state
            .fences
            .get(&op.scoped_session_id)
            .is_some_and(|f| f.active && f.operation == operation && f.generation == op.generation);
        let active = opened && op.active();
        state.set_fence(
            op.scoped_session_id.clone(),
            Fence {
                operation: operation.into(),
                generation: op.generation,
                box_id: op.box_id.clone(),
                active,
                held: !op.terminal() && !active,
                capacity: !op.terminal(),
                terminal: op.terminal(),
                permanent: op.kind == Kind::Delete,
            },
        );
        state
            .protected_names
            .insert(terminal_session_resource_name(&op.scoped_session_id));
        for origin in snapshot
            .operations
            .values()
            .filter(|o| o.scoped_session_id == op.scoped_session_id && o.kind == Kind::Target)
        {
            state
                .protected_names
                .insert(worker_boxlite::persistence::target_name(
                    &op.scoped_session_id,
                    &origin.operation_id,
                    origin.generation,
                ));
        }
        Ok(())
    }

    pub(crate) async fn activate_snapshot_instance(
        &self,
        connection: &str,
        scope: &str,
        operation: &str,
        generation: i64,
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        if state.failed || !state.connected || state.connection.as_deref() != Some(connection) {
            return Err(unavailable());
        }
        let snapshot = state.read_journal()?;
        let op = snapshot.operations.get(operation).ok_or_else(conflict)?;
        if snapshot.current.get(scope).map(String::as_str) != Some(operation)
            || op.scoped_session_id != scope
            || !op.active()
            || op.generation != generation
            || op.lease_expires_unix_ms <= to_unix_millis(SystemTime::now())
        {
            return Err(conflict());
        }
        let fence = state.fences.get(scope).ok_or_else(conflict)?;
        if fence.operation != operation
            || fence.generation != generation
            || fence.box_id != op.box_id
        {
            return Err(conflict());
        }
        let already_active = fence.active;
        let session = state.entries.get_mut(scope).ok_or_else(conflict)?;
        if already_active
            && session.box_id == op.box_id
            && !session.destroying
            && !session.persistence_fault
            && !session.proxy_cancel.is_cancelled()
        {
            return Ok(());
        }
        if session.box_id != op.box_id
            || session.destroying
            || session.inflight != 0
            || session.proxy_inflight.load(Ordering::SeqCst) != 0
        {
            return Err(conflict());
        }
        session.lease_expires_at =
            system_time_from_unix_millis(op.lease_expires_unix_ms).ok_or_else(conflict)?;
        session.persistence_fault = false;
        if session.proxy_cancel.is_cancelled() {
            session.proxy_cancel = CancellationToken::new();
        }
        let fence = state.fences.get_mut(scope).ok_or_else(conflict)?;
        fence.active = true;
        fence.held = false;
        Ok(())
    }

    pub(crate) async fn detach_snapshot_instance(
        &self,
        scope: &str,
        operation: &str,
        generation: i64,
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        let snapshot = state.read_journal()?;
        let op = snapshot.operations.get(operation).ok_or_else(conflict)?;
        if snapshot.current.get(scope).map(String::as_str) != Some(operation)
            || op.scoped_session_id != scope
            || op.generation != generation
            || !op.terminal()
        {
            return Err(conflict());
        }
        let fence = state.fences.get(scope).ok_or_else(conflict)?;
        if state.failed
            || fence.active
            || fence.operation != operation
            || fence.generation != generation
        {
            return Err(conflict());
        }
        if state
            .entries
            .get(scope)
            .is_some_and(|s| s.inflight != 0 || s.proxy_inflight.load(Ordering::SeqCst) != 0)
        {
            return Err(conflict());
        }
        if let Some(session) = state.entries.remove(scope) {
            session.proxy_cancel.cancel();
            if session.capacity_reserved {
                self.active_session_reservations
                    .fetch_sub(1, Ordering::SeqCst);
            }
        }
        if let Some(f) = state.fences.get_mut(scope) {
            f.capacity = false;
            f.terminal = true;
            f.held = false;
            f.permanent = op.kind == Kind::Delete;
        }
        Ok(())
    }

    pub(crate) async fn claim_proxy(
        &self,
        scope: &str,
        port: u16,
        generation: i64,
        now: SystemTime,
    ) -> Result<ProxyPermit, ProxyTargetError> {
        let state = self.sessions.lock().await;
        if self.closed.load(Ordering::SeqCst)
            || state.check_access(scope.trim(), generation).is_err()
        {
            return Err(ProxyTargetError::SessionNotFound);
        }
        let session = state
            .entries
            .get(scope.trim())
            .ok_or(ProxyTargetError::SessionNotFound)?;
        if session.destroying || session.box_id.is_empty() || session.lease_expires_at <= now {
            return Err(ProxyTargetError::SessionNotFound);
        }
        let enabled_ports = self.backend.proxy_ports();
        if !enabled_ports.contains(&port) {
            return Err(ProxyTargetError::PortNotEnabled {
                requested_port: port,
                enabled_ports,
            });
        }
        session.proxy_inflight.fetch_add(1, Ordering::SeqCst);
        Ok(ProxyPermit {
            box_id: session.box_id.clone(),
            cancel: session.proxy_cancel.clone(),
            count: session.proxy_inflight.clone(),
            changed: self.persistence_changed.clone(),
        })
    }

    pub(crate) async fn proxy_binding_active(
        &self,
        scope: &str,
        box_id: &str,
        generation: i64,
        now: SystemTime,
    ) -> bool {
        let state = self.sessions.lock().await;
        !self.closed.load(Ordering::SeqCst)
            && state.check_access(scope, generation).is_ok()
            && state
                .entries
                .get(scope)
                .is_some_and(|s| !s.destroying && s.box_id == box_id && s.lease_expires_at > now)
    }
}

pub(super) fn state_recovery_allowed(
    state: &SessionState,
    scope: &str,
    generation: i64,
) -> Result<(), TerminalOperationError> {
    if state.failed {
        return Err(unavailable());
    }
    match state.fences.get(scope) {
        Some(f) if f.active && f.generation == generation && !f.box_id.is_empty() => Ok(()),
        Some(_) => Err(conflict()),
        None if generation == 0 && !state.bootstrap.contains_key(scope) => Ok(()),
        None if generation == 1 && state.bootstrap.contains_key(scope) => Ok(()),
        None => Err(conflict()),
    }
}

#[cfg(test)]
#[path = "terminal_persistence_tests.rs"]
mod tests;

#[allow(dead_code)]
#[path = "snapshot_native_engine.rs"]
pub(crate) mod native_control;
