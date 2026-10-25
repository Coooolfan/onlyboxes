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
    pub(super) initial_roots: HashMap<String, String>,
    pub(super) pending_adoptions: HashSet<String>,
    pub(super) journal: Option<Arc<Journal>>,
    pub(super) execution_guard: Option<worker_boxlite::persistence::LeaseGuardClient>,
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
pub(super) fn unavailable() -> TerminalOperationError {
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
        self.failed
            || self.pending_adoptions.contains(scope)
            || self.execution_guard.is_some()
                && (self.bootstrap.contains_key(scope)
                    || self.initial_roots.contains_key(scope)
                    || self.fences.contains_key(scope))
            || self.fences.get(scope).is_some_and(|f| f.held)
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
            .saturating_add(
                self.bootstrap
                    .iter()
                    .chain(self.initial_roots.iter())
                    .filter(|(scope, _)| {
                        self.execution_guard.is_some()
                            && !self.fences.contains_key(*scope)
                            && !retiring.contains_key(*scope)
                            && !self
                                .entries
                                .get(*scope)
                                .is_some_and(|s| s.capacity_reserved)
                    })
                    .count(),
            )
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
        if generation < 0 || self.pending_adoptions.contains(scope) {
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
                    || session.persistence_fault
                    || session.lease_expires_at <= SystemTime::now()
                {
                    return Err(conflict());
                }
            }
            None => {
                if self.initial_roots.contains_key(scope)
                    && (!self.connected
                        || self.entries.get(scope).is_none_or(|s| {
                            s.destroying
                                || s.persistence_fault
                                || s.lease_expires_at <= SystemTime::now()
                        }))
                {
                    return Err(conflict());
                }
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
        if (self.fences.contains_key(scope)
            || self.bootstrap.contains_key(scope)
            || self.initial_roots.contains_key(scope))
            && (connection.is_none() || connection != self.connection.as_deref())
        {
            return Err(conflict());
        }
        Ok(())
    }
    pub(super) async fn persist_lease(
        &mut self,
        scope: &str,
        generation: i64,
        target: SystemTime,
    ) -> Result<(), TerminalOperationError> {
        let Some(fence) = self.fences.get(scope) else {
            return self
                .confirm_execution_guard(scope, generation, Some(target))
                .await;
        };
        let already_covered = self
            .entries
            .get(scope)
            .is_some_and(|s| s.lease_expires_at >= target);
        let Some(journal) = self.journal.as_ref() else {
            self.fail();
            return Err(unavailable());
        };
        let id = fence.operation.clone();
        let box_id = fence.box_id.clone();
        let result = if already_covered {
            Ok(())
        } else {
            journal.update(&id, |op| {
                if !op.active()
                    || op.scoped_session_id != scope
                    || op.generation != generation
                    || op.box_id != box_id
                {
                    return Err(worker_boxlite::persistence::Error::Conflict);
                }
                op.lease_expires_unix_ms = op.lease_expires_unix_ms.max(to_unix_millis(target));
                Ok(())
            })
        };
        if result.is_err() {
            self.fail();
            return Err(unavailable());
        }
        self.confirm_execution_guard(scope, generation, Some(target))
            .await
    }
    pub(super) async fn confirm_execution_guard(
        &mut self,
        scope: &str,
        generation: i64,
        extend: Option<SystemTime>,
    ) -> Result<(), TerminalOperationError> {
        let managed = self.fences.contains_key(scope)
            || self.bootstrap.contains_key(scope)
            || self.initial_roots.contains_key(scope);
        let memory = self
            .entries
            .get(scope)
            .map(|session| session.lease_expires_at);
        let effective = match (memory, extend) {
            (Some(a), Some(b)) => Some(a.max(b)),
            (a, b) => a.or(b),
        };
        let result = self
            .check_execution_guard(scope, generation, extend)
            .await
            .and_then(|()| {
                if managed && effective.is_none_or(|deadline| deadline <= SystemTime::now()) {
                    Err(unavailable())
                } else {
                    Ok(())
                }
            });
        if result.is_err() {
            self.fail();
        }
        result
    }
    async fn check_execution_guard(
        &mut self,
        scope: &str,
        generation: i64,
        extend: Option<SystemTime>,
    ) -> Result<(), TerminalOperationError> {
        let managed = self.fences.contains_key(scope)
            || self.bootstrap.contains_key(scope)
            || self.initial_roots.contains_key(scope);
        if !managed {
            return Ok(());
        }
        let snapshot = self.read_journal()?;
        let generation = if generation == 0 && self.initial_roots.contains_key(scope) {
            1
        } else {
            generation
        };
        let op = snapshot
            .current
            .get(scope)
            .map(|id| &snapshot.operations[id]);
        let Some(guard) = self.execution_guard.clone() else {
            return if op.is_none_or(|op| op.boot_guard.is_none())
                && !snapshot.adoptions.contains_key(scope)
            {
                Ok(())
            } else {
                Err(unavailable())
            };
        };
        if guard.identity().worker_id != snapshot.worker_id {
            return Err(unavailable());
        }
        let (box_id, process) = if let Some(fence) = self.fences.get(scope) {
            let op = op.ok_or_else(unavailable)?;
            if fence.operation != op.operation_id
                || fence.generation != generation
                || fence.box_id != op.box_id
                || !op.active()
                || op
                    .boot_guard
                    .as_ref()
                    .is_none_or(|binding| &binding.guard != guard.identity())
            {
                return Err(unavailable());
            }
            (
                op.box_id.clone(),
                Some(op.started_process.clone().ok_or_else(unavailable)?),
            )
        } else {
            if generation != 1 {
                return Err(conflict());
            }
            let adoption = snapshot.adoptions.get(scope).ok_or_else(unavailable)?;
            if !adoption.confirmed || &adoption.guard != guard.identity() {
                return Err(unavailable());
            }
            (
                self.bootstrap
                    .get(scope)
                    .or_else(|| self.initial_roots.get(scope))
                    .cloned()
                    .ok_or_else(conflict)?,
                Some(adoption.lease.process.clone()),
            )
        };
        let memory = self.entries.get(scope).map(|entry| entry.lease_expires_at);
        let mut required = extend.or(memory).ok_or_else(unavailable)?;
        if let Some(memory) = memory {
            required = required.max(memory);
        }
        if extend.is_some() {
            if let Some(op) = op {
                required = required.max(
                    system_time_from_unix_millis(op.lease_expires_unix_ms)
                        .ok_or_else(unavailable)?,
                );
            }
        }
        check_guard_lease(
            &guard,
            scope,
            generation,
            &box_id,
            process.as_ref(),
            required,
            extend.is_some(),
        )
        .await
    }
    async fn verify_saved_guard(
        &self,
        op: &worker_boxlite::persistence::Operation,
        worker: &str,
        running: bool,
    ) -> Result<(), TerminalOperationError> {
        let Some(guard) = &self.execution_guard else {
            return if op.boot_guard.is_none() {
                Ok(())
            } else {
                Err(unavailable())
            };
        };
        if guard.identity().worker_id != worker
            || op
                .boot_guard
                .as_ref()
                .is_none_or(|binding| &binding.guard != guard.identity())
        {
            return Err(unavailable());
        }
        if running {
            let process = op.started_process.as_ref().ok_or_else(unavailable)?;
            let lease =
                system_time_from_unix_millis(op.lease_expires_unix_ms).ok_or_else(unavailable)?;
            check_guard_lease(
                guard,
                &op.scoped_session_id,
                op.generation,
                &op.box_id,
                Some(process),
                lease,
                false,
            )
            .await?;
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

// Round up so the millisecond companion grant covers the full in-memory time.
pub(super) fn guard_deadline(time: SystemTime) -> Result<i64, TerminalOperationError> {
    let duration = time
        .duration_since(SystemTime::UNIX_EPOCH)
        .map_err(|_| unavailable())?;
    let floor = i64::try_from(duration.as_millis()).map_err(|_| unavailable())?;
    floor
        .checked_add(i64::from(
            !duration.subsec_nanos().is_multiple_of(1_000_000),
        ))
        .ok_or_else(unavailable)
}
// A known completed boot may have stopped while its managed scope remains
// active in the durable journal. Retention permits registration and explicit
// higher-generation purge only; it never rearms that boot or opens its gates.
async fn stopped_managed_hold(
    op: &worker_boxlite::persistence::Operation,
    worker: &str,
    guard: &worker_boxlite::persistence::LeaseGuardClient,
) -> bool {
    let Some(binding) = &op.boot_guard else {
        return false;
    };
    let Some(authorization) = &binding.authorization else {
        return false;
    };
    let Some(process) = &op.started_process else {
        return false;
    };
    if !op.active()
        || !matches!(op.kind, Kind::Source | Kind::Target)
        || !op.start_attempted
        || guard.identity().worker_id != worker
        || &binding.guard != guard.identity()
        || authorization.operation_id != op.operation_id
        || authorization.scoped_session_id != op.scoped_session_id
        || authorization.generation != op.generation
        || authorization.box_id != op.box_id
        || op.boot_executable.as_ref() != Some(&authorization.executable)
        || !authorization.origin.has_exited().is_ok_and(|exited| exited)
        || !process.writers_exited().is_ok_and(|exited| exited)
    {
        return false;
    }
    // Both responses must come from the exact already-bound live companion.
    // Missing history, a new epoch or an old writer's mere absence is not proof.
    if !guard
        .boot_status(&op.box_id)
        .await
        .is_ok_and(|saved| saved.as_ref() == Some(authorization))
    {
        return false;
    }
    guard.status(&op.box_id).await.is_ok_and(|saved| {
        saved.is_some_and(|lease| {
            lease.scoped_session_id == op.scoped_session_id
                && lease.generation == op.generation
                && lease.process == *process
                && lease.expires_unix_ms >= op.lease_expires_unix_ms
                && lease.revision > authorization.prior_revision.unwrap_or(0)
        })
    })
}

// This proof permits only inventory/retention, never execution or deletion.
// Keep the saved service epoch and boot receipts unchanged. A pending root
// remains protected from orphan cleanup and consumes its existing capacity.
async fn stopped_initial_hold(
    snapshot: &worker_boxlite::persistence::JournalState,
    scope: &str,
    id: &str,
    guard: &worker_boxlite::persistence::LeaseGuardClient,
) -> bool {
    let Some(boot) = snapshot.initial_boots.get(scope) else {
        return false;
    };
    let Some(adoption) = snapshot.adoptions.get(scope) else {
        return false;
    };
    let current = guard.identity();
    if snapshot.current.contains_key(scope)
        || snapshot.probes.get(scope).map(String::as_str) != Some(id)
        || boot.managed
        || !adoption.confirmed
        || boot.guard != adoption.guard
        || boot.authorization.box_id != id
        || adoption.lease.box_id != id
        || current.worker_id != snapshot.worker_id
        || current.worker_id != adoption.guard.worker_id
        || current.store_id != adoption.guard.store_id
        || !boot
            .authorization
            .origin
            .has_exited()
            .is_ok_and(|exited| exited)
        || (current != &adoption.guard
            && !adoption
                .guard
                .process
                .has_exited()
                .is_ok_and(|exited| exited))
        || !adoption
            .lease
            .process
            .writers_exited()
            .is_ok_and(|exited| exited)
    {
        return false;
    }
    // Query the already bound live service. A file on disk, missing record,
    // unreachable service or elapsed wall clock is not a revocation receipt.
    guard.status(id).await.is_ok_and(|lease| {
        lease.is_some_and(|lease| {
            lease.revoked
                && lease.scoped_session_id == scope
                && lease.generation == 1
                && lease.process == adoption.lease.process
                && lease.revision >= adoption.lease.revision
                && lease.expires_unix_ms >= adoption.lease.expires_unix_ms
        })
    })
}

async fn check_guard_lease(
    guard: &worker_boxlite::persistence::LeaseGuardClient,
    scope: &str,
    generation: i64,
    box_id: &str,
    process: Option<&worker_boxlite::persistence::HostProcess>,
    required: SystemTime,
    extend: bool,
) -> Result<(), TerminalOperationError> {
    let required = guard_deadline(required)?;
    let current = guard
        .check_running(box_id)
        .await
        .map_err(|_| unavailable())?;
    if current.scoped_session_id != scope
        || current.generation != generation
        || process.is_some_and(|process| process != &current.process)
    {
        return Err(unavailable());
    }
    if current.expires_unix_ms < required {
        if !extend {
            return Err(unavailable());
        }
        guard
            .renew(
                current.revision,
                worker_boxlite::persistence::ExecutionLease {
                    revision: current.revision.checked_add(1).ok_or_else(unavailable)?,
                    expires_unix_ms: required,
                    ..current
                },
            )
            .await
            .map_err(|_| unavailable())?;
    }
    if required <= to_unix_millis(SystemTime::now()) {
        return Err(unavailable());
    }
    Ok(())
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
    pub(crate) async fn healthy_guarded_shutdown(
        &self,
        identity: &worker_boxlite::persistence::LeaseGuardIdentity,
    ) -> bool {
        let state = self.sessions.lock().await;
        !state.failed
            && state
                .execution_guard
                .as_ref()
                .is_some_and(|guard| guard.identity() == identity)
            && state
                .journal
                .as_ref()
                .is_some_and(|journal| journal.snapshot().is_ok())
    }

    pub(crate) async fn has_snapshot_journal(&self) -> bool {
        self.sessions.lock().await.journal.is_some()
    }

    pub(crate) async fn install_execution_guard(
        &self,
        guard: Option<worker_boxlite::persistence::LeaseGuardClient>,
    ) -> Result<(), TerminalOperationError> {
        let mut state = self.sessions.lock().await;
        if self.closed.load(Ordering::SeqCst) || state.failed {
            return Err(unavailable());
        }
        if state.execution_guard.as_ref().is_some_and(|old| {
            guard
                .as_ref()
                .is_none_or(|new| old.identity() != new.identity())
        }) {
            state.fail();
            return Err(unavailable());
        }
        state.execution_guard = guard;
        Ok(())
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
        let mut pending = HashSet::new();
        for (scope, id) in &snapshot.current {
            let op = &snapshot.operations[id];
            if !op.terminal()
                && (op.active() || op.phase == Phase::Ready || op.boot_guard.is_some())
                && state
                    .verify_saved_guard(
                        op,
                        &snapshot.worker_id,
                        op.active() || op.phase == Phase::Ready,
                    )
                    .await
                    .is_err()
            {
                let held = match &state.execution_guard {
                    Some(guard) => stopped_managed_hold(op, &snapshot.worker_id, guard).await,
                    None => false,
                };
                if !held {
                    state.fail();
                    return Err(unavailable());
                }
                pending.insert(scope.clone());
                tracing::warn!(
                    stage = "stopped_managed_held",
                    "BoxLite managed root retained with execution closed"
                );
            }
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
                    active: op.active() && !pending.contains(scope),
                    held: op.held() || pending.contains(scope),
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
            if !snapshot.current.contains_key(scope) {
                if let Some(adoption) = snapshot.adoptions.get(scope) {
                    let Some(guard) = state.execution_guard.clone() else {
                        state.fail();
                        return Err(unavailable());
                    };
                    let same_companion = guard.identity() == &adoption.guard;
                    let running = if same_companion && adoption.confirmed {
                        guard.check_running(id).await.is_ok_and(|live| {
                            live.scoped_session_id == *scope
                                && live.generation == 1
                                && live.process == adoption.lease.process
                                && live.expires_unix_ms >= adoption.lease.expires_unix_ms
                        })
                    } else {
                        false
                    };
                    if !same_companion || adoption.confirmed && !running {
                        if !stopped_initial_hold(&snapshot, scope, id, &guard).await {
                            tracing::error!(
                                stage = if same_companion {
                                    "initial_writer_lease"
                                } else {
                                    "initial_companion_identity"
                                },
                                "BoxLite persistence recovery check failed"
                            );
                            state.fail();
                            return Err(unavailable());
                        }
                        pending.insert(scope.clone());
                        tracing::warn!(
                            stage = "stopped_initial_held",
                            "BoxLite initial root retained with execution closed"
                        );
                    } else if !adoption.confirmed {
                        pending.insert(scope.clone());
                    }
                } else if let Some(boot) = snapshot.initial_boots.get(scope) {
                    if state
                        .execution_guard
                        .as_ref()
                        .is_none_or(|g| g.identity() != &boot.guard)
                    {
                        state.fail();
                        return Err(unavailable());
                    }
                    pending.insert(scope.clone());
                } else if state.execution_guard.is_some() {
                    state.fail();
                    return Err(unavailable());
                }
            }
            if state.entries.get(scope).is_some_and(|s| s.box_id != *id) {
                state.fail();
                return Err(conflict());
            }
            names.insert(terminal_session_resource_name(scope));
        }
        state.initial_roots = snapshot
            .probes
            .iter()
            .filter(|(scope, _)| {
                snapshot
                    .initial_boots
                    .get(*scope)
                    .is_some_and(|b| !b.managed)
            })
            .map(|(scope, id)| (scope.clone(), id.clone()))
            .collect();
        state.bootstrap = snapshot
            .probes
            .into_iter()
            .filter(|(scope, _)| !state.initial_roots.contains_key(scope))
            .collect();
        state.pending_adoptions = pending;
        state.fences = fences;
        state.protected_names = names;
        state.journal = Some(journal);
        state.connected = false;
        // A poisoned manager stays closed until reconstructed with a healthy journal.
        Ok(())
    }

    // Recover only the recorded running ID for an inspect retry. The entry
    // stays closed until adoption confirmation; this never starts a new VM.
    pub(crate) async fn restore_initial_adoption(
        &self,
        adoption: &worker_boxlite::persistence::InitialAdoption,
    ) -> Result<(), TerminalOperationError> {
        let scope = &adoption.lease.scoped_session_id;
        let id = &adoption.lease.box_id;
        let mut state = self.sessions.lock().await;
        if self.closed.load(Ordering::SeqCst) || state.failed {
            return Err(unavailable());
        }
        let snapshot = state.read_journal()?;
        if snapshot.current.contains_key(scope)
            || snapshot.adoptions.get(scope) != Some(adoption)
            || state
                .bootstrap
                .get(scope)
                .or_else(|| state.initial_roots.get(scope))
                != Some(id)
            || state
                .execution_guard
                .as_ref()
                .is_none_or(|guard| guard.identity() != &adoption.guard)
        {
            return Err(conflict());
        }
        if let Some(entry) = state.entries.get(scope) {
            return if entry.box_id == *id && !entry.destroying {
                Ok(())
            } else {
                Err(conflict())
            };
        }
        let lease = if adoption.confirmed {
            let live = state
                .execution_guard
                .as_ref()
                .unwrap()
                .check_running(id)
                .await
                .map_err(|_| unavailable())?;
            if live.process != adoption.lease.process
                || live.scoped_session_id != *scope
                || live.generation != 1
            {
                return Err(conflict());
            }
            live.expires_unix_ms
        } else {
            adoption.lease.expires_unix_ms
        };
        if self
            .backend
            .recover_pinned_session_box(id)
            .await?
            .as_deref()
            != Some(id)
        {
            return Err(conflict());
        }
        let (ready_tx, _) = watch::channel(SessionReadyState::Ready(id.clone()));
        state.entries.insert(
            scope.clone(),
            TerminalSession {
                session_id: scope.clone(),
                box_id: id.clone(),
                lease_expires_at: system_time_from_unix_millis(lease).ok_or_else(conflict)?,
                inflight: 0,
                destroying: false,
                capacity_reserved: true,
                ready_tx,
                proxy_cancel: CancellationToken::new(),
                proxy_inflight: Arc::new(AtomicU32::new(0)),
                persistence_fault: false,
            },
        );
        state.pending_adoptions.insert(scope.clone());
        self.active_session_reservations
            .fetch_add(1, Ordering::SeqCst);
        Ok(())
    }

    pub(crate) async fn fail_persistence(&self) {
        self.sessions.lock().await.fail();
        self.persistence_changed.notify_waiters();
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
            if state.fences.contains_key(scope)
                || state.bootstrap.contains_key(scope)
                || state.initial_roots.contains_key(scope)
            {
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
            if state.fences.contains_key(scope)
                || state.bootstrap.contains_key(scope)
                || state.initial_roots.contains_key(scope)
            {
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
        if state
            .verify_saved_guard(op, &snapshot.worker_id, true)
            .await
            .is_err()
        {
            state.fail();
            return Err(unavailable());
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
        if state
            .verify_saved_guard(op, &snapshot.worker_id, true)
            .await
            .is_err()
        {
            state.fail();
            return Err(unavailable());
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
        let mut state = self.sessions.lock().await;
        if self.closed.load(Ordering::SeqCst)
            || state.check_access(scope.trim(), generation).is_err()
        {
            return Err(ProxyTargetError::SessionNotFound);
        }
        if state
            .confirm_execution_guard(scope.trim(), generation, None)
            .await
            .is_err()
        {
            return Err(ProxyTargetError::SessionNotFound);
        }
        let session = state
            .entries
            .get(scope.trim())
            .ok_or(ProxyTargetError::SessionNotFound)?;
        if session.destroying
            || session.box_id.is_empty()
            || session.lease_expires_at <= SystemTime::now().max(now)
        {
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
        let mut state = self.sessions.lock().await;
        if self.closed.load(Ordering::SeqCst)
            || state.check_access(scope, generation).is_err()
            || !state
                .entries
                .get(scope)
                .is_some_and(|s| !s.destroying && s.box_id == box_id && s.lease_expires_at > now)
        {
            return false;
        }
        state
            .confirm_execution_guard(scope, generation, None)
            .await
            .is_ok()
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
    if state.pending_adoptions.contains(scope) {
        return Err(conflict());
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

#[path = "terminal_initial_creation.rs"]
mod initial_creation;
