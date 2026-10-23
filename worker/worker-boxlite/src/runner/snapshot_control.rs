//! Private lifecycle frames and owned action admission. Transfer grants are
//! ephemeral; these types intentionally never implement Debug or durable I/O.
use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use tokio::sync::{oneshot, Notify};
use tokio_util::sync::CancellationToken;
use worker_boxlite::persistence::{Archive, CompletedPart, RestoreSpec, TransferGrant, UploadPart};

use crate::proto::registryv1::{
    sandbox_persistence_control::Action, CommandError, SandboxPersistenceControl,
    SandboxPersistenceReport,
};

#[derive(Clone, Default, Deserialize, Serialize)]
#[serde(default, deny_unknown_fields)]
pub(crate) struct Control {
    pub restore_spec: Option<RestoreSpec>,
    pub archive: Option<Archive>,
    pub parts: Vec<UploadPart>,
    pub download: Option<TransferGrant>,
    pub max_bytes: i64,
    pub max_expanded_bytes: i64,
    pub staging_bytes: i64,
    pub lease_ttl_sec: i64,
    pub drain_timeout_sec: i64,
    pub rollback: bool,
    pub purge_session: bool,
    pub delete: bool,
    pub snapshot_committed: bool,
}
#[derive(Default, Deserialize, Serialize)]
pub(crate) struct ResultPayload {
    pub state: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub restore_spec: Option<RestoreSpec>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub archive: Option<Archive>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub parts: Vec<CompletedPart>,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub lease_expires_unix_ms: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub instance_id: String,
}
fn is_zero(n: &i64) -> bool {
    *n == 0
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ControlError {
    Conflict,
    Unavailable,
    Integrity,
    Quota,
    Storage,
    Capacity,
    OperationCapacity,
    Busy,
}
impl ControlError {
    fn code(self) -> &'static str {
        match self {
            Self::Conflict => "session_state_conflict",
            Self::Unavailable => "persistence_unavailable",
            Self::Integrity => "snapshot_integrity_error",
            Self::Quota => "snapshot_quota_exceeded",
            Self::Storage => "snapshot_storage_error",
            Self::Capacity => "session_capacity_exceeded",
            Self::OperationCapacity => "no_capacity",
            Self::Busy => "no_capacity",
        }
    }
}

pub(crate) struct ActionContext {
    pub connection: String,
    pub frame: SandboxPersistenceControl,
    pub payload: Control,
    pub cancel: CancellationToken,
}
impl ActionContext {
    /// Check immediately before each new side effect or admission opening.
    /// SDK futures already accepted must be awaited even if this returns false.
    pub fn authorized(&self) -> bool {
        !self.cancel.is_cancelled() && self.frame.deadline_unix_ms > now_ms()
    }
}
#[async_trait]
pub(crate) trait LifecycleEngine: Send + Sync + 'static {
    async fn execute(&self, action: ActionContext) -> Result<ResultPayload, ControlError>;
    /// An action panic is an unknown native outcome. Preserve roots and close
    /// admission; the controller cannot safely run a new scope action afterward.
    fn fail_closed(&self);
}

struct Active {
    ticket: u64,
    operation: String,
    generation: i64,
    cancel: CancellationToken,
}
struct State {
    connection: String,
    recovering: bool,
    closed: bool,
    failed: bool,
    ticket: u64,
    actions: HashMap<String, Active>,
}
pub(crate) struct ControlDispatcher {
    engine: Arc<dyn LifecycleEngine>,
    max_inflight: usize,
    state: Mutex<State>,
    idle: Notify,
}
static SHARED_DISPATCHER: OnceLock<Arc<ControlDispatcher>> = OnceLock::new();
pub(crate) fn shared_dispatcher() -> Option<Arc<ControlDispatcher>> {
    SHARED_DISPATCHER.get().cloned()
}
pub(crate) fn install_dispatcher(dispatcher: Arc<ControlDispatcher>) -> Result<(), ControlError> {
    SHARED_DISPATCHER
        .set(dispatcher)
        .map_err(|_| ControlError::Unavailable)
}

impl ControlDispatcher {
    pub(crate) fn new(
        engine: Arc<dyn LifecycleEngine>,
        max_inflight: usize,
    ) -> Result<Arc<Self>, ControlError> {
        if !(1..=64).contains(&max_inflight) {
            return Err(ControlError::Conflict);
        }
        Ok(Arc::new(Self {
            engine,
            max_inflight,
            state: Mutex::new(State {
                connection: String::new(),
                recovering: true,
                closed: false,
                failed: false,
                ticket: 0,
                actions: HashMap::new(),
            }),
            idle: Notify::new(),
        }))
    }

    pub(crate) fn inflight(&self) -> Result<i32, ControlError> {
        let state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
        if state.closed || state.failed {
            return Err(ControlError::Unavailable);
        }
        i32::try_from(state.actions.len()).map_err(|_| ControlError::Unavailable)
    }
    pub(crate) fn fail_closed(&self) {
        if let Ok(mut state) = self.state.lock() {
            state.failed = true;
            for action in state.actions.values() {
                action.cancel.cancel();
            }
        }
        self.engine.fail_closed();
        self.idle.notify_waiters();
    }

    /// Reconnect cannot race accepted old SDK work. New actions stay closed
    /// until all previous actions finish and Console recovery is acknowledged.
    pub(crate) async fn begin_connection(&self, connection: &str) -> Result<(), ControlError> {
        if !valid_connection(connection) {
            return Err(ControlError::Conflict);
        }
        {
            let mut state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
            if state.closed || state.failed {
                return Err(ControlError::Unavailable);
            }
            state.recovering = true;
            state.connection = connection.to_owned();
            for active in state.actions.values() {
                active.cancel.cancel();
            }
        }
        loop {
            let changed = self.idle.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            {
                let state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
                if state.closed || state.failed {
                    return Err(ControlError::Unavailable);
                }
                if state.connection != connection {
                    return Err(ControlError::Conflict);
                }
                if state.actions.is_empty() {
                    return Ok(());
                }
            }
            changed.await;
        }
    }
    pub(crate) fn finish_connection(&self, connection: &str) -> Result<(), ControlError> {
        let mut state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
        if state.closed || state.failed {
            return Err(ControlError::Unavailable);
        }
        if state.connection != connection || !state.actions.is_empty() {
            return Err(ControlError::Conflict);
        }
        state.recovering = false;
        Ok(())
    }
    pub(crate) fn disconnect(&self, connection: &str) {
        if let Ok(mut state) = self.state.lock() {
            if state.connection != connection {
                return;
            }
            state.recovering = true;
            for action in state.actions.values() {
                action.cancel.cancel();
            }
        }
        self.idle.notify_waiters();
    }
    pub(crate) async fn close(&self) -> Result<(), ControlError> {
        {
            let mut state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
            state.closed = true;
            for action in state.actions.values() {
                action.cancel.cancel();
            }
        }
        loop {
            let changed = self.idle.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            {
                let state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
                if state.actions.is_empty() {
                    return if state.failed {
                        Err(ControlError::Unavailable)
                    } else {
                        Ok(())
                    };
                }
            }
            changed.await;
        }
    }

    /// Only the report receiver escapes. Dropping it never aborts accepted SDK
    /// work or releases the per-scope action reservation prematurely.
    pub(crate) fn submit(
        self: &Arc<Self>,
        connection: &str,
        frame: SandboxPersistenceControl,
    ) -> oneshot::Receiver<SandboxPersistenceReport> {
        let (reply, receiver) = oneshot::channel();
        let result = self.admit(connection, &frame);
        let (ticket, payload, cancel) = match result {
            Ok(admitted) => admitted,
            Err(error) => {
                let _ = reply.send(report(&frame, Err(error)));
                return receiver;
            }
        };
        let dispatcher = self.clone();
        let engine = self.engine.clone();
        let response_frame = frame.clone();
        let context = ActionContext {
            connection: connection.into(),
            frame,
            payload,
            cancel: cancel.clone(),
        };
        tokio::spawn(async move {
            // A nested owned task catches engine panics without detaching the
            // physical operation from the reservation or reconnect barrier.
            let deadline = std::time::Duration::from_millis(
                response_frame
                    .deadline_unix_ms
                    .saturating_sub(now_ms())
                    .max(0) as u64,
            );
            let mut work = tokio::spawn(async move {
                if !context.authorized() {
                    return Err(ControlError::Busy);
                }
                engine.execute(context).await
            });
            let result = tokio::select! {
                result = &mut work => result,
                _ = tokio::time::sleep(deadline) => { cancel.cancel(); work.await }
            };
            let panicked = result.is_err();
            dispatcher.complete(&response_frame.scoped_session_id, ticket, panicked);
            if panicked {
                dispatcher.engine.fail_closed();
            }
            let result = result.unwrap_or(Err(ControlError::Unavailable));
            let _ = reply.send(report(&response_frame, result));
        });
        receiver
    }

    fn admit(
        &self,
        connection: &str,
        frame: &SandboxPersistenceControl,
    ) -> Result<(u64, Control, CancellationToken), ControlError> {
        if !valid_id(&frame.request_id)
            || !valid_id(&frame.operation_id)
            || !valid_scope(&frame.scoped_session_id)
            || frame.generation <= 0
            || frame.deadline_unix_ms <= now_ms()
            || frame.payload_json.len() > 64 << 10
            || !matches!(Action::try_from(frame.action), Ok(action) if action != Action::Unspecified)
        {
            return Err(ControlError::Conflict);
        }
        let payload: Control =
            serde_json::from_slice(&frame.payload_json).map_err(|_| ControlError::Conflict)?;
        let mut state = self.state.lock().map_err(|_| ControlError::Unavailable)?;
        if state.closed || state.failed {
            return Err(ControlError::Unavailable);
        }
        if state.connection != connection || !valid_connection(connection) {
            return Err(ControlError::Conflict);
        }
        if state.recovering {
            return Err(ControlError::Busy);
        }
        if let Some(active) = state.actions.get(&frame.scoped_session_id) {
            if frame.action == Action::Cancel as i32
                && ((active.operation == frame.operation_id
                    && active.generation == frame.generation)
                    || (payload.purge_session && frame.generation > active.generation))
            {
                active.cancel.cancel();
            }
            return Err(ControlError::Busy);
        }
        if state.actions.len() >= self.max_inflight {
            return Err(ControlError::Busy);
        }
        state.ticket = state
            .ticket
            .checked_add(1)
            .ok_or(ControlError::Unavailable)?;
        let ticket = state.ticket;
        let cancel = CancellationToken::new();
        state.actions.insert(
            frame.scoped_session_id.clone(),
            Active {
                ticket,
                operation: frame.operation_id.clone(),
                generation: frame.generation,
                cancel: cancel.clone(),
            },
        );
        Ok((ticket, payload, cancel))
    }
    fn complete(&self, scope: &str, ticket: u64, failed: bool) {
        if let Ok(mut state) = self.state.lock() {
            if failed {
                state.failed = true;
                for active in state.actions.values() {
                    active.cancel.cancel();
                }
            }
            if state
                .actions
                .get(scope)
                .is_some_and(|active| active.ticket == ticket)
            {
                state.actions.remove(scope);
            }
        }
        self.idle.notify_waiters();
    }
}

pub(crate) fn unavailable_report(frame: &SandboxPersistenceControl) -> SandboxPersistenceReport {
    report(frame, Err(ControlError::Unavailable))
}
fn report(
    frame: &SandboxPersistenceControl,
    result: Result<ResultPayload, ControlError>,
) -> SandboxPersistenceReport {
    let mut response = SandboxPersistenceReport {
        request_id: frame.request_id.clone(),
        operation_id: frame.operation_id.clone(),
        scoped_session_id: frame.scoped_session_id.clone(),
        generation: frame.generation,
        action: frame.action,
        completed: true,
        ..Default::default()
    };
    match result {
        Ok(result) => match serde_json::to_vec(&result) {
            Ok(bytes) => {
                response.phase = result.state;
                response.payload_json = bytes;
            }
            Err(_) => {
                response.error = Some(control_error(ControlError::Unavailable));
            }
        },
        Err(ControlError::Busy) => {
            response.phase = "busy".into();
            response.payload_json = br#"{"state":"busy"}"#.to_vec();
        }
        Err(error) => response.error = Some(control_error(error)),
    }
    response
}
fn control_error(error: ControlError) -> CommandError {
    CommandError {
        code: error.code().into(),
        message: "worker could not complete the sandbox lifecycle action".into(),
    }
}
fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|value| i64::try_from(value.as_millis()).ok())
        .unwrap_or(i64::MAX)
}
fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'-')
}
fn valid_connection(id: &str) -> bool {
    !id.is_empty() && id.len() <= 256 && id.trim() == id && !id.chars().any(char::is_control)
}
fn valid_scope(scope: &str) -> bool {
    !scope.is_empty()
        && scope.len() <= 512
        && scope.trim() == scope
        && !scope.chars().any(char::is_control)
}

#[cfg(test)]
#[path = "snapshot_control_tests.rs"]
mod tests;
