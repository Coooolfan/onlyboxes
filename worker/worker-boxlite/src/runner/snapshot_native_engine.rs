//! Native lifecycle actions. Admission/task ownership is supplied by the
//! control dispatcher; this engine never registers a production capability.
use super::*;
use crate::proto::registryv1::sandbox_persistence_control::Action;
use crate::runner::snapshot_control::{
    ActionContext, ControlError, LifecycleEngine, ResultPayload,
};
use std::fs::{File, OpenOptions};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use worker_boxlite::persistence::{
    ArchiveLimits, Error as JournalError, JournalState, NativeError, NativeInstance, NativeRuntime,
    Operation, RestoreSpec, Transfer, TransferError, VerifiedSource,
};

#[path = "snapshot_cache_engine.rs"]
mod cache;
#[path = "snapshot_target_engine.rs"]
mod target;

type Outcome<T> = Result<T, ControlError>;
const MAX_BYTES: i64 = 1 << 60;
#[derive(Clone)]
pub(crate) struct NativeControlConfig {
    pub max_inflight: usize,
    pub tmp_quota_bytes: i64,
    pub drain_timeout: Duration,
    pub readiness_timeout: Duration,
    pub cache_ttl: Duration,
    pub cache_max_bytes: i64,
    pub transfer_concurrency: usize,
    pub transfer_idle: Duration,
}
struct Reservation {
    operation: String,
    generation: i64,
    bytes: i64,
}
pub(crate) struct NativeControlEngine {
    manager: Arc<TerminalSessionManager>,
    journal: Arc<Journal>,
    native: Arc<NativeRuntime>,
    transfer: Transfer,
    config: NativeControlConfig,
    admission: Mutex<()>,
    reservations: std::sync::Mutex<HashMap<String, Reservation>>,
    pins: std::sync::Mutex<HashMap<String, Arc<VerifiedSource>>>,
    failed: AtomicBool,
}
impl NativeControlEngine {
    pub(crate) async fn new(
        manager: Arc<TerminalSessionManager>,
        journal: Arc<Journal>,
        native: Arc<NativeRuntime>,
        config: NativeControlConfig,
    ) -> Outcome<Arc<Self>> {
        if !(1..=64).contains(&config.max_inflight)
            || config.tmp_quota_bytes <= 0
            || config.tmp_quota_bytes > MAX_BYTES
            || config.drain_timeout.is_zero()
            || config.drain_timeout > Duration::from_secs(600)
            || config.readiness_timeout.is_zero()
            || config.readiness_timeout > Duration::from_secs(600)
            || config.cache_max_bytes < 0
            || config.cache_max_bytes > config.tmp_quota_bytes
            || config.cache_ttl.is_zero()
            || config.cache_ttl.as_millis() > i64::MAX as u128
        {
            return Err(ControlError::Conflict);
        }
        let transfer = Transfer::new(config.transfer_concurrency, config.transfer_idle)
            .map_err(transfer_error)?;
        manager
            .install_execution_guard(native.execution_guard().cloned())
            .await
            .map_err(terminal_error)?;
        manager
            .install_snapshot_journal(journal.clone())
            .await
            .map_err(terminal_error)?;
        Ok(Arc::new(Self {
            manager,
            journal,
            native,
            transfer,
            config,
            admission: Mutex::new(()),
            reservations: Default::default(),
            pins: Default::default(),
            failed: AtomicBool::new(false),
        }))
    }

    async fn run(&self, action: &ActionContext) -> Outcome<ResultPayload> {
        self.authorize(action).await?;
        let state = self.journal.snapshot().map_err(journal_error)?;
        let frame = &action.frame;
        let known = state.operations.get(&frame.operation_id).cloned();
        if known.as_ref().is_some_and(|op| {
            op.scoped_session_id != frame.scoped_session_id || op.generation != frame.generation
        }) || state
            .current
            .get(&frame.scoped_session_id)
            .is_some_and(|id| state.operations[id].generation > frame.generation)
        {
            return Err(ControlError::Conflict);
        }
        if known.as_ref().is_some_and(|op| {
            state.current.get(&op.scoped_session_id).map(String::as_str)
                != Some(&frame.operation_id)
        }) {
            return Err(ControlError::Conflict);
        }
        match Action::try_from(frame.action).map_err(|_| ControlError::Conflict)? {
            Action::Inspect if known.as_ref().is_some_and(|op| op.kind != Kind::Source) => {
                self.inspect_other(action, known.as_ref().unwrap()).await
            }
            Action::Inspect => self.inspect(action, &state, known.as_ref()).await,
            Action::PrepareRestore => self.prepare_restore(action, known).await,
            Action::Restore => self.restore(action, known).await,
            Action::Activate => self.activate(action, known).await,
            Action::PrepareSnapshot => self.prepare(action, known).await,
            Action::UploadSnapshot => self.upload(action, source(&known)?).await,
            Action::VerifySnapshot => self.verify(action, source(&known)?).await,
            Action::FinalizeSuspend => self.finalize(action, source(&known)?).await,
            Action::Cancel
                if action.payload.rollback
                    && !action.payload.purge_session
                    && !action.payload.delete =>
            {
                self.rollback(action, &state, known).await
            }
            Action::Cancel
                if action.payload.purge_session
                    && !action.payload.rollback
                    && !action.payload.delete =>
            {
                self.purge_session(action, known).await
            }
            Action::Cancel if !action.payload.rollback && !action.payload.purge_session => {
                self.cancel_target(action, known).await
            }
            _ => Err(ControlError::Unavailable),
        }
    }
    async fn authorize(&self, action: &ActionContext) -> Outcome<()> {
        if !action.authorized() {
            return Err(ControlError::Busy);
        }
        if self.failed.load(Ordering::SeqCst) || self.manager.closed.load(Ordering::SeqCst) {
            return Err(ControlError::Unavailable);
        }
        let state = self.manager.sessions.lock().await;
        if state.failed {
            return Err(ControlError::Unavailable);
        }
        if !state.connected || state.connection.as_deref() != Some(&action.connection) {
            return Err(ControlError::Conflict);
        }
        if state
            .pending_adoptions
            .contains(&action.frame.scoped_session_id)
            && action.frame.action != Action::Inspect as i32
            && !(action.frame.action == Action::Cancel as i32
                && action.payload.purge_session
                && action.frame.generation > 1)
        {
            return Err(ControlError::Busy);
        }
        Ok(())
    }
    async fn binding(&self, action: &ActionContext) -> Outcome<(String, i64)> {
        let state = self.manager.sessions.lock().await;
        let session = state
            .get(&action.frame.scoped_session_id)
            .ok_or(ControlError::Conflict)?;
        if session.destroying
            || session.box_id.is_empty()
            || state
                .fences
                .get(&action.frame.scoped_session_id)
                .is_some_and(|f| f.generation != action.frame.generation || f.permanent)
        {
            return Err(ControlError::Conflict);
        }
        Ok((
            session.box_id.clone(),
            to_unix_millis(session.lease_expires_at),
        ))
    }
    async fn described(
        &self,
        state: &JournalState,
        scope: &str,
        id: &str,
    ) -> Outcome<NativeInstance> {
        match state.current.get(scope).map(|id| &state.operations[id]) {
            Some(op) if op.box_id != id => Err(ControlError::Conflict),
            Some(op) if op.kind == Kind::Target => self
                .native
                .inspect_target(&self.journal, &op.operation_id)
                .await
                .map_err(native_error),
            Some(op) if op.kind == Kind::Source => self
                .native
                .inspect_source(&self.journal, &op.operation_id)
                .await
                .map_err(native_error),
            Some(op) if op.kind != Kind::Adopt => Err(ControlError::Conflict),
            _ => self.native.inspect(scope, id).await.map_err(native_error),
        }
    }
    async fn inspect(
        &self,
        action: &ActionContext,
        state: &JournalState,
        known: Option<&Operation>,
    ) -> Outcome<ResultPayload> {
        if let Some(op) = known {
            if op.kind != Kind::Source {
                return Err(ControlError::Unavailable);
            }
            if op.phase == Phase::Frozen {
                let archive = op.archive.as_ref().ok_or(ControlError::Conflict)?;
                self.native
                    .export_source(
                        self.journal.clone(),
                        &op.operation_id,
                        ArchiveLimits {
                            max_bytes: archive.size_bytes,
                            max_expanded_bytes: archive.expanded_bytes,
                        },
                    )
                    .await
                    .map_err(native_error)?;
            }
            let mut response = result(op);
            if op.phase == Phase::RolledBack {
                if self.active_binding(op).await {
                    self.verify_running_source(op).await?;
                } else {
                    response.state = "busy".into();
                }
            }
            return Ok(response);
        }
        if let Some(id) = state.current.get(&action.frame.scoped_session_id) {
            let current = &state.operations[id];
            if !current.active() || current.generation != action.frame.generation {
                return Ok(state_result("missing"));
            }
        } else if action.frame.generation != 1 {
            return Ok(state_result("missing"));
        }
        let (id, lease) = match self.binding(action).await {
            Ok(binding) => binding,
            Err(ControlError::Conflict) => {
                if let Some(adoption) =
                    state
                        .adoptions
                        .get(&action.frame.scoped_session_id)
                        .filter(|_| {
                            action.frame.generation == 1
                                && !state.current.contains_key(&action.frame.scoped_session_id)
                        })
                {
                    self.manager
                        .restore_initial_adoption(adoption)
                        .await
                        .map_err(terminal_error)?;
                    self.binding(action).await?
                } else {
                    return Ok(state_result("missing"));
                }
            }
            Err(e) => return Err(e),
        };
        if lease <= now_ms()? {
            return Err(ControlError::Conflict);
        }
        let inspected = self
            .described(state, &action.frame.scoped_session_id, &id)
            .await?;
        if inspected.status != boxlite::BoxStatus::Running {
            return Err(ControlError::Conflict);
        }
        self.authorize(action).await?;
        if !state.current.contains_key(&action.frame.scoped_session_id) {
            let mut sessions = self.manager.sessions.lock().await;
            let session = sessions
                .get(&action.frame.scoped_session_id)
                .ok_or(ControlError::Conflict)?;
            if session.box_id != id || session.destroying {
                return Err(ControlError::Conflict);
            }
            let scope = &action.frame.scoped_session_id;
            if let Some(guard) = self.native.execution_guard() {
                let deadline =
                    super::guard_deadline(session.lease_expires_at).map_err(terminal_error)?;
                sessions.bootstrap.insert(scope.clone(), id.clone());
                sessions.pending_adoptions.insert(scope.clone());
                sessions.get(scope).unwrap().proxy_cancel.cancel();
                let adoption = self
                    .native
                    .prepare_initial_adoption(&self.journal, scope, &id, deadline)
                    .await
                    .map_err(native_error)?;
                self.native
                    .confirm_initial_adoption(&self.journal, &adoption)
                    .await
                    .map_err(native_error)?;
                let live = guard.check_running(&id).await.map_err(journal_error)?;
                if live.expires_unix_ms < deadline {
                    return Err(ControlError::Conflict);
                }
                sessions.pending_adoptions.remove(scope);
                let session = sessions.get_mut(scope).ok_or(ControlError::Conflict)?;
                if session.proxy_cancel.is_cancelled() {
                    session.proxy_cancel = tokio_util::sync::CancellationToken::new();
                }
            } else {
                self.journal.probe(scope, &id).map_err(journal_error)?;
                sessions.bootstrap.insert(scope.clone(), id.clone());
            }
        }
        Ok(ResultPayload {
            state: "running".into(),
            restore_spec: Some(inspected.spec),
            instance_id: id,
            lease_expires_unix_ms: lease,
            ..Default::default()
        })
    }

    async fn prepare(
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
        let limits = limits(action)?;
        let stage = source_stage(limits)?;
        let op = if let Some(op) = known {
            if op.kind != Kind::Source
                || op.restore_spec.as_ref() != Some(spec)
                || !matches!(op.phase, Phase::Preparing | Phase::Frozen)
            {
                return Err(ControlError::Conflict);
            }
            if stage > op.staging_bytes {
                return Err(ControlError::Quota);
            }
            op
        } else {
            let _admission = self.admission.lock().await;
            let before = self.journal.snapshot().map_err(journal_error)?;
            match before
                .current
                .get(&action.frame.scoped_session_id)
                .map(|id| &before.operations[id])
            {
                Some(current)
                    if !current.active() || current.generation != action.frame.generation =>
                {
                    return Err(ControlError::Conflict)
                }
                None if action.frame.generation != 1 => return Err(ControlError::Conflict),
                _ => (),
            }
            self.reserve_with_cache(action, stage).await?;
            let state = self.journal.snapshot().map_err(journal_error)?;
            let (id, lease) = match self.binding(action).await {
                Ok(binding) => binding,
                Err(error) => {
                    self.release(action)?;
                    return Err(error);
                }
            };
            let described = match self
                .described(&state, &action.frame.scoped_session_id, &id)
                .await
            {
                Ok(info) if info.spec == *spec => info,
                Ok(_) => {
                    self.release(action)?;
                    return Err(ControlError::Conflict);
                }
                Err(error) => {
                    self.release(action)?;
                    return Err(error);
                }
            };
            if let Err(error) = self.authorize(action).await {
                self.release(action)?;
                return Err(error);
            }
            let held = match self
                .manager
                .hold_snapshot_source(
                    &action.frame.scoped_session_id,
                    &action.frame.operation_id,
                    action.frame.generation,
                )
                .await
            {
                Ok(id) => id,
                Err(error) => {
                    self.release(action)?;
                    return Err(terminal_error(error));
                }
            };
            if held != described.box_id {
                return Err(ControlError::Conflict);
            }
            // Once held, finish recording intent even if the delivery expires.
            let (current_id, current_lease) = self.binding(action).await?;
            if current_id != id {
                return Err(ControlError::Conflict);
            }
            let op = source_intent(action, spec.clone(), id, lease.max(current_lease), stage)?;
            self.journal.claim(op.clone()).map_err(journal_error)?;
            self.release(action)?;
            self.manager
                .sync_snapshot_operation(&op.operation_id)
                .await
                .map_err(terminal_error)?;
            op
        };
        self.authorize(action).await?;
        if op.phase == Phase::Preparing {
            let requested = action.payload.drain_timeout_sec;
            let drain = if requested > 0 {
                self.config
                    .drain_timeout
                    .min(Duration::from_secs(requested as u64))
            } else {
                self.config.drain_timeout
            };
            tokio::select! {
                _ = action.cancel.cancelled() => return Err(ControlError::Busy),
                outcome = tokio::time::timeout(drain, self.drain_existing(action, &op.box_id, false)) => outcome.map_err(|_| ControlError::Busy)??,
            }
        }
        self.authorize(action).await?;
        self.native
            .export_source(self.journal.clone(), &op.operation_id, limits)
            .await
            .map_err(native_error)?;
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.current_result(&op.operation_id)
    }

    async fn upload(&self, action: &ActionContext, op: Operation) -> Outcome<ResultPayload> {
        require_frozen(&op, action)?;
        let archive = op.archive.as_ref().ok_or(ControlError::Conflict)?;
        let cached = self
            .pins
            .lock()
            .map_err(|_| ControlError::Unavailable)?
            .get(&op.operation_id)
            .cloned();
        let source = if let Some(source) = cached {
            source
        } else {
            let file = self.archive_file(&op)?;
            let source = Arc::new(
                self.transfer
                    .source(file, archive.clone(), action.cancel.clone())
                    .await
                    .map_err(transfer_error)?,
            );
            self.pins
                .lock()
                .map_err(|_| ControlError::Unavailable)?
                .insert(op.operation_id.clone(), source.clone());
            source
        };
        let parts = self
            .transfer
            .upload(&source, &action.payload.parts, action.cancel.clone())
            .await
            .map_err(transfer_error)?;
        let mut response = result(&op);
        response.parts = parts;
        Ok(response)
    }
    async fn verify(&self, action: &ActionContext, op: Operation) -> Outcome<ResultPayload> {
        require_frozen(&op, action)?;
        let archive = op.archive.as_ref().ok_or(ControlError::Conflict)?;
        if action.payload.archive.as_ref() != Some(archive) {
            return Err(ControlError::Conflict);
        }
        self.transfer
            .verify_remote(
                action
                    .payload
                    .download
                    .as_ref()
                    .ok_or(ControlError::Conflict)?,
                archive,
                action.cancel.clone(),
            )
            .await
            .map_err(transfer_error)?;
        self.journal
            .update(&op.operation_id, |current| {
                current.remote_verified = true;
                Ok(())
            })
            .map_err(journal_error)?;
        let mut response = self.current_result(&op.operation_id)?;
        response.state = "verified".into();
        Ok(response)
    }
    async fn finalize(&self, action: &ActionContext, op: Operation) -> Outcome<ResultPayload> {
        if !action.payload.snapshot_committed
            || !op.remote_verified
            || !matches!(
                op.phase,
                Phase::Frozen | Phase::Committed | Phase::Finalized
            )
            || action
                .payload
                .archive
                .as_ref()
                .is_some_and(|archive| Some(archive) != op.archive.as_ref())
        {
            return Err(ControlError::Conflict);
        }
        if op.phase == Phase::Frozen {
            self.journal
                .update(&op.operation_id, |current| {
                    current.phase = Phase::Committed;
                    current.snapshot_committed = true;
                    Ok(())
                })
                .map_err(journal_error)?;
            self.manager
                .sync_snapshot_operation(&op.operation_id)
                .await
                .map_err(terminal_error)?;
        }
        self.pins
            .lock()
            .map_err(|_| ControlError::Unavailable)?
            .remove(&op.operation_id);
        self.transfer.wait_io_idle().await.map_err(transfer_error)?;
        self.authorize(action).await?;
        // This accepted cleanup is awaited through SDK acknowledgement loss.
        self.native
            .finalize_source(&self.journal, &op.operation_id)
            .await
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
        self.current_result(&op.operation_id)
    }

    async fn rollback(
        &self,
        action: &ActionContext,
        state: &JournalState,
        known: Option<Operation>,
    ) -> Outcome<ResultPayload> {
        let op = if let Some(op) = known {
            if op.kind != Kind::Source
                || op.snapshot_committed
                || !matches!(
                    op.phase,
                    Phase::Preparing | Phase::Frozen | Phase::RollingBack | Phase::RolledBack
                )
                || action
                    .payload
                    .restore_spec
                    .as_ref()
                    .is_some_and(|spec| Some(spec) != op.restore_spec.as_ref())
            {
                return Err(ControlError::Conflict);
            }
            op
        } else {
            let _admission = self.admission.lock().await;
            let spec = action
                .payload
                .restore_spec
                .as_ref()
                .ok_or(ControlError::Conflict)?;
            spec.validate().map_err(|_| ControlError::Conflict)?;
            let current = state
                .current
                .get(&action.frame.scoped_session_id)
                .map(|id| &state.operations[id]);
            if current.is_some_and(|op| !op.active() || op.generation != action.frame.generation) {
                return Err(ControlError::Conflict);
            }
            let id = current
                .map(|op| op.box_id.clone())
                .or_else(|| state.probes.get(&action.frame.scoped_session_id).cloned())
                .ok_or(ControlError::Conflict)?;
            let described = self
                .described(state, &action.frame.scoped_session_id, &id)
                .await?;
            if described.spec != *spec {
                return Err(ControlError::Conflict);
            }
            let lease = self.hold_for_rollback(action, &id).await?;
            let stage = self
                .reservations
                .lock()
                .map_err(|_| ControlError::Unavailable)?
                .get(&action.frame.scoped_session_id)
                .map(|r| r.bytes)
                .unwrap_or(0);
            let op = source_intent(action, spec.clone(), id, lease, stage)?;
            self.journal.claim(op.clone()).map_err(journal_error)?;
            self.release(action)?;
            self.manager
                .sync_snapshot_operation(&op.operation_id)
                .await
                .map_err(terminal_error)?;
            op
        };
        if op.phase == Phase::RolledBack && self.active_binding(&op).await {
            self.verify_running_source(&op).await?;
            return Ok(result(&op));
        }
        self.hold_for_rollback(action, &op.box_id).await?;
        tokio::select! {
            _ = action.cancel.cancelled() => return Err(ControlError::Busy),
            drained = tokio::time::timeout(self.config.drain_timeout, self.drain_existing(action, &op.box_id, true)) => drained.map_err(|_| ControlError::Busy)??,
        }
        self.pins
            .lock()
            .map_err(|_| ControlError::Unavailable)?
            .remove(&op.operation_id);
        self.transfer.wait_io_idle().await.map_err(transfer_error)?;
        self.authorize(action).await?;
        let ttl = if action.payload.lease_ttl_sec == 0 {
            None
        } else {
            Some(i32::try_from(action.payload.lease_ttl_sec).map_err(|_| ControlError::Conflict)?)
        };
        let duration = self
            .manager
            .resolve_lease_duration(ttl)
            .map_err(terminal_error)?;
        let lease = op.lease_expires_unix_ms.max(
            now_ms()?
                .checked_add(
                    i64::try_from(duration.as_millis()).map_err(|_| ControlError::Conflict)?,
                )
                .ok_or(ControlError::Conflict)?,
        );
        self.native
            .rollback_source(
                &self.journal,
                &op.operation_id,
                lease,
                self.config.readiness_timeout,
            )
            .await
            .map_err(native_error)?;
        self.manager
            .sync_snapshot_operation(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.authorize(action).await?;
        self.manager
            .bind_snapshot_instance(&op.operation_id)
            .await
            .map_err(terminal_error)?;
        self.manager
            .activate_snapshot_instance(
                &action.connection,
                &op.scoped_session_id,
                &op.operation_id,
                op.generation,
            )
            .await
            .map_err(terminal_error)?;
        self.current_result(&op.operation_id)
    }
    async fn hold_for_rollback(&self, action: &ActionContext, id: &str) -> Outcome<i64> {
        let mut state = self.manager.sessions.lock().await;
        if state.failed
            || !state.connected
            || state.connection.as_deref() != Some(&action.connection)
        {
            return Err(ControlError::Unavailable);
        }
        if state
            .fences
            .get(&action.frame.scoped_session_id)
            .is_some_and(|f| {
                f.permanent
                    || f.terminal
                    || f.generation != action.frame.generation
                    || (!f.active && f.operation != action.frame.operation_id)
                    || (!f.box_id.is_empty() && f.box_id != id)
            })
        {
            return Err(ControlError::Conflict);
        }
        let lease = if let Some(session) = state.get(&action.frame.scoped_session_id) {
            if session.box_id != id || session.destroying {
                return Err(ControlError::Conflict);
            }
            to_unix_millis(session.lease_expires_at)
        } else {
            0
        };
        state.set_fence(
            action.frame.scoped_session_id.clone(),
            Fence {
                operation: action.frame.operation_id.clone(),
                generation: action.frame.generation,
                box_id: id.into(),
                active: false,
                held: true,
                capacity: true,
                terminal: false,
                permanent: false,
            },
        );
        self.manager.persistence_changed.notify_waiters();
        Ok(lease)
    }
    async fn drain_existing(
        &self,
        action: &ActionContext,
        id: &str,
        allow_fault: bool,
    ) -> Outcome<()> {
        loop {
            let changed = self.manager.persistence_changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            {
                let state = self.manager.sessions.lock().await;
                if state.failed
                    || state
                        .fences
                        .get(&action.frame.scoped_session_id)
                        .is_none_or(|f| {
                            !f.held
                                || f.operation != action.frame.operation_id
                                || f.box_id != id
                                || f.generation != action.frame.generation
                        })
                {
                    return Err(ControlError::Conflict);
                }
                match state.get(&action.frame.scoped_session_id) {
                    Some(session)
                        if session.destroying
                            || session.box_id != id
                            || (!allow_fault && session.persistence_fault) =>
                    {
                        return Err(ControlError::Conflict)
                    }
                    Some(session)
                        if session.inflight != 0
                            || session.proxy_inflight.load(Ordering::SeqCst) != 0 => {}
                    _ => return Ok(()),
                }
            }
            changed.await;
        }
    }
    async fn verify_running_source(&self, op: &Operation) -> Outcome<()> {
        let inspected = self
            .native
            .inspect_source(&self.journal, &op.operation_id)
            .await
            .map_err(native_error)?;
        let process = op.started_process.as_ref().ok_or(ControlError::Conflict)?;
        if inspected.status != boxlite::BoxStatus::Running
            || inspected.pid != Some(process.pid)
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
    async fn active_binding(&self, op: &Operation) -> bool {
        let state = self.manager.sessions.lock().await;
        state
            .fences
            .get(&op.scoped_session_id)
            .is_some_and(|f| f.operation == op.operation_id)
            && state
                .check_access(&op.scoped_session_id, op.generation)
                .is_ok()
    }
    fn current_result(&self, operation: &str) -> Outcome<ResultPayload> {
        let state = self.journal.snapshot().map_err(journal_error)?;
        Ok(result(
            state
                .operations
                .get(operation)
                .ok_or(ControlError::Conflict)?,
        ))
    }
    fn reserve(&self, action: &ActionContext, bytes: i64) -> Outcome<()> {
        let mut reservations = self
            .reservations
            .lock()
            .map_err(|_| ControlError::Unavailable)?;
        if let Some(old) = reservations.get(&action.frame.scoped_session_id) {
            return if old.operation == action.frame.operation_id
                && old.generation == action.frame.generation
                && bytes <= old.bytes
            {
                Ok(())
            } else {
                Err(ControlError::Conflict)
            };
        }
        let state = self.journal.snapshot().map_err(journal_error)?;
        let mut reserved = 0_i64;
        let mut count = reservations.len();
        for reservation in reservations.values() {
            reserved = reserved
                .checked_add(reservation.bytes)
                .ok_or(ControlError::Quota)?;
        }
        let held: HashSet<_> = state
            .current
            .values()
            .filter(|id| state.operations[*id].held())
            .cloned()
            .collect();
        for id in &held {
            count += 1;
            reserved = reserved
                .checked_add(state.operations[id].staging_bytes)
                .ok_or(ControlError::Quota)?;
        }
        if count >= self.config.max_inflight {
            return Err(ControlError::OperationCapacity);
        }
        let future = reserved.checked_add(bytes).ok_or(ControlError::Quota)?;
        if self
            .native
            .available_staging_bytes()
            .map_err(native_error)?
            < future as u64
            || self
                .journal
                .available_artifact_bytes()
                .map_err(journal_error)?
                < future as u64
        {
            return Err(ControlError::Quota);
        }
        // SAFETY: geteuid reads process identity.
        let owner = unsafe { libc::geteuid() };
        for op in state.operations.values().filter(|op| {
            !held.contains(&op.operation_id) && matches!(op.kind, Kind::Source | Kind::Target)
        }) {
            let archive = self
                .journal
                .artifact_path(&op.scoped_session_id, &op.operation_id, op.generation)
                .map_err(journal_error)?;
            for path in [&archive, &archive.with_extension("boxlite.partial")] {
                match std::fs::symlink_metadata(path) {
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
                    Ok(meta)
                        if meta.is_file()
                            && meta.mode() & 0o077 == 0
                            && meta.nlink() <= 2
                            && meta.uid() == owner =>
                    {
                        reserved = reserved
                            .checked_add(
                                i64::try_from(meta.len()).map_err(|_| ControlError::Quota)?,
                            )
                            .ok_or(ControlError::Quota)?;
                    }
                    _ => return Err(ControlError::Storage),
                }
            }
        }
        if bytes > self.config.tmp_quota_bytes.saturating_sub(reserved) {
            return Err(ControlError::Quota);
        }
        reservations.insert(
            action.frame.scoped_session_id.clone(),
            Reservation {
                operation: action.frame.operation_id.clone(),
                generation: action.frame.generation,
                bytes,
            },
        );
        Ok(())
    }
    fn release(&self, action: &ActionContext) -> Outcome<()> {
        self.reservations
            .lock()
            .map_err(|_| ControlError::Unavailable)?
            .remove(&action.frame.scoped_session_id);
        Ok(())
    }
    fn archive_file(&self, op: &Operation) -> Outcome<File> {
        let path = self
            .journal
            .artifact_path(&op.scoped_session_id, &op.operation_id, op.generation)
            .map_err(journal_error)?;
        let file = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(path)
            .map_err(|_| ControlError::Integrity)?;
        let meta = file.metadata().map_err(|_| ControlError::Storage)?;
        // SAFETY: geteuid reads process identity.
        if !meta.is_file()
            || meta.mode() & 0o077 != 0
            || meta.nlink() != 1
            || meta.uid() != unsafe { libc::geteuid() }
        {
            return Err(ControlError::Integrity);
        }
        Ok(file)
    }
}
#[async_trait]
impl LifecycleEngine for NativeControlEngine {
    async fn execute(&self, action: ActionContext) -> Outcome<ResultPayload> {
        let outcome = self.run(&action).await;
        if self.journal.snapshot().is_err() {
            self.failed.store(true, Ordering::SeqCst);
            self.manager.sessions.lock().await.fail();
            self.manager.persistence_changed.notify_waiters();
            return Err(ControlError::Unavailable);
        }
        outcome
    }
    fn fail_closed(&self) {
        self.failed.store(true, Ordering::SeqCst);
        let manager = self.manager.clone();
        tokio::spawn(async move {
            manager.sessions.lock().await.fail();
            manager.persistence_changed.notify_waiters();
        });
    }
}
fn source(known: &Option<Operation>) -> Outcome<Operation> {
    known
        .as_ref()
        .filter(|op| op.kind == Kind::Source)
        .cloned()
        .ok_or(ControlError::Conflict)
}
fn require_frozen(op: &Operation, action: &ActionContext) -> Outcome<()> {
    if op.phase != Phase::Frozen
        || op.archive.is_none()
        || action
            .payload
            .archive
            .as_ref()
            .is_some_and(|archive| Some(archive) != op.archive.as_ref())
    {
        Err(ControlError::Conflict)
    } else {
        Ok(())
    }
}
fn limits(action: &ActionContext) -> Outcome<ArchiveLimits> {
    let p = &action.payload;
    if p.max_bytes <= 0
        || p.max_bytes > MAX_BYTES
        || p.max_expanded_bytes <= 0
        || p.max_expanded_bytes > MAX_BYTES
        || p.drain_timeout_sec < 0
    {
        return Err(ControlError::Conflict);
    }
    Ok(ArchiveLimits {
        max_bytes: p.max_bytes,
        max_expanded_bytes: p.max_expanded_bytes,
    })
}
fn source_stage(limits: ArchiveLimits) -> Outcome<i64> {
    let bytes = limits
        .max_bytes
        .checked_mul(2)
        .and_then(|n| n.checked_add(limits.max_expanded_bytes))
        .ok_or(ControlError::Quota)?;
    let stage = bytes.checked_add(bytes / 5).ok_or(ControlError::Quota)?;
    if stage > MAX_BYTES {
        Err(ControlError::Quota)
    } else {
        Ok(stage)
    }
}
fn source_intent(
    action: &ActionContext,
    spec: RestoreSpec,
    id: String,
    lease: i64,
    stage: i64,
) -> Outcome<Operation> {
    serde_json::from_value(serde_json::json!({
        "operation_id":action.frame.operation_id, "scoped_session_id":action.frame.scoped_session_id,
        "generation":action.frame.generation, "kind":"source", "phase":"preparing",
        "restore_spec":spec, "box_id":id, "lease_expires_unix_ms":lease, "staging_bytes":stage,
    })).map_err(|_| ControlError::Conflict)
}
fn result(op: &Operation) -> ResultPayload {
    let state = match op.phase {
        Phase::Preparing | Phase::RollingBack | Phase::Canceling => "busy",
        Phase::RolledBack => "running",
        Phase::Frozen => "frozen",
        Phase::Committed => "committed",
        Phase::Finalized => "finalized",
        _ => "busy",
    };
    ResultPayload {
        state: state.into(),
        restore_spec: op.restore_spec.clone(),
        archive: op.archive.clone(),
        instance_id: op.box_id.clone(),
        lease_expires_unix_ms: op.lease_expires_unix_ms,
        ..Default::default()
    }
}
fn state_result(state: &str) -> ResultPayload {
    ResultPayload {
        state: state.into(),
        ..Default::default()
    }
}
fn now_ms() -> Outcome<i64> {
    SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()
        .and_then(|n| i64::try_from(n.as_millis()).ok())
        .ok_or(ControlError::Unavailable)
}
fn journal_error(error: JournalError) -> ControlError {
    match error {
        JournalError::Conflict => ControlError::Conflict,
        JournalError::Unavailable => ControlError::Unavailable,
    }
}
fn native_error(error: NativeError) -> ControlError {
    match error {
        NativeError::Conflict | NativeError::Missing => ControlError::Conflict,
        NativeError::Unavailable | NativeError::Unsupported => ControlError::Unavailable,
        NativeError::ArchiveInvalid => ControlError::Integrity,
        NativeError::Budget => ControlError::Quota,
    }
}
fn transfer_error(error: TransferError) -> ControlError {
    match error {
        TransferError::Retry => ControlError::Busy,
        TransferError::Integrity => ControlError::Integrity,
        TransferError::Local => ControlError::Storage,
    }
}
fn terminal_error(error: TerminalOperationError) -> ControlError {
    match error {
        TerminalOperationError::Terminal(error)
            if error.code == TERMINAL_EXEC_CODE_SESSION_CAPACITY_EXCEEDED =>
        {
            ControlError::Capacity
        }
        TerminalOperationError::DeadlineExceeded => ControlError::Busy,
        _ => ControlError::Conflict,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn stage_reserves_two_archives_and_expanded_disk_with_checked_headroom() {
        assert_eq!(
            source_stage(ArchiveLimits {
                max_bytes: 1024,
                max_expanded_bytes: 4096
            }),
            Ok(7372)
        );
        assert_eq!(
            source_stage(ArchiveLimits {
                max_bytes: MAX_BYTES,
                max_expanded_bytes: 1
            }),
            Err(ControlError::Quota)
        );
    }
}
