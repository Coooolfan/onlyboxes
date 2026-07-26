use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::sync::OnceLock;
use std::time::{Duration, SystemTime};

use async_trait::async_trait;
use base64::Engine;
use reqwest::StatusCode;
use serde::{Deserialize, Serialize};
use tokio::sync::{watch, Mutex};
use tokio_util::io::ReaderStream;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

use crate::boxlite_runtime::{self, BoxliteCommandError, CollectedExecOutput};
use crate::config::Config;

pub(crate) const TERMINAL_EXEC_JANITOR_INTERVAL: Duration = Duration::from_secs(5);
pub(crate) const TERMINAL_EXEC_NO_SESSION_MESSAGE: &str = "session not found";
pub(crate) const TERMINAL_EXEC_BUSY_MESSAGE: &str = "session is busy";
pub(crate) const TERMINAL_EXEC_CODE_SESSION_NOT_FOUND: &str = "session_not_found";
pub(crate) const TERMINAL_EXEC_CODE_SESSION_BUSY: &str = "session_busy";
pub(crate) const TERMINAL_EXEC_CODE_INVALID_PAYLOAD: &str = "invalid_payload";
pub(crate) const TERMINAL_RESOURCE_ACTION_VALIDATE: &str = "validate";
pub(crate) const TERMINAL_RESOURCE_ACTION_READ: &str = "read";
pub(crate) const TERMINAL_RESOURCE_ACTION_EXPORT: &str = "export";
pub(crate) const TERMINAL_RESOURCE_CODE_FILE_NOT_FOUND: &str = "file_not_found";
pub(crate) const TERMINAL_RESOURCE_CODE_PATH_IS_DIR: &str = "path_is_directory";
pub(crate) const TERMINAL_RESOURCE_CODE_PATH_NOT_ALLOWED: &str = "path_not_allowed";
pub(crate) const TERMINAL_RESOURCE_CODE_FILE_TOO_LARGE: &str = "file_too_large";

pub(crate) const TERMINAL_RESOURCE_PROBE_SCRIPT: &str = r#"
import argparse
import base64
import json
import mimetypes
import os
import sys

parser = argparse.ArgumentParser()
parser.add_argument("--action", choices=["validate", "read"], default="validate")
parser.add_argument("--file-path", required=True)
parser.add_argument("--max-read-bytes", type=int, required=True)
args = parser.parse_args()

target = args.file_path
if not os.path.exists(target):
    print(json.dumps({"error": "file_not_found", "message": "file not found"}))
    sys.exit(10)
if os.path.isdir(target):
    print(json.dumps({"error": "path_is_directory", "message": "path is directory"}))
    sys.exit(11)

size_bytes = os.path.getsize(target)
mime_type, _ = mimetypes.guess_type(target)
if not mime_type:
    mime_type = "application/octet-stream"

if args.action == "validate":
    print(json.dumps({"mime_type": mime_type, "size_bytes": size_bytes}))
    sys.exit(0)

limit = args.max_read_bytes
if size_bytes > limit:
    print(json.dumps({
        "error": "file_too_large",
        "message": "file exceeds read limit",
        "mime_type": mime_type,
        "size_bytes": size_bytes,
    }))
    sys.exit(12)

with open(target, "rb") as fh:
    content = fh.read(limit + 1)
if len(content) > limit:
    print(json.dumps({
        "error": "file_too_large",
        "message": "file exceeds read limit",
        "mime_type": mime_type,
        "size_bytes": len(content),
    }))
    sys.exit(12)

print(json.dumps({
    "mime_type": mime_type,
    "size_bytes": len(content),
    "blob": base64.b64encode(content).decode("ascii"),
}))
"#;

static TERMINAL_SESSION_MANAGER: OnceLock<Arc<TerminalSessionManager>> = OnceLock::new();

#[derive(Debug, Clone)]
pub(crate) struct TerminalExecRequest {
    pub command: String,
    pub session_id: String,
    pub create_if_missing: bool,
    pub lease_ttl_sec: Option<i32>,
    pub deadline_unix_ms: i64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub(crate) struct TerminalExecRunResult {
    pub session_id: String,
    pub created: bool,
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
    pub stdout_truncated: bool,
    pub stderr_truncated: bool,
    pub lease_expires_unix_ms: i64,
}

#[derive(Debug, Clone)]
pub(crate) struct TerminalResourceRequest {
    pub session_id: String,
    pub file_path: String,
    pub action: String,
    pub signed_url: String,
    pub headers: HashMap<String, String>,
    pub deadline_unix_ms: i64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub(crate) struct TerminalResourceRunResult {
    pub session_id: String,
    pub file_path: String,
    pub mime_type: String,
    pub size_bytes: i64,
    #[serde(
        default,
        skip_serializing_if = "Vec::is_empty",
        serialize_with = "serialize_resource_blob",
        deserialize_with = "deserialize_resource_blob"
    )]
    pub blob: Vec<u8>,
}

#[derive(Debug, Deserialize)]
struct TerminalResourceProbeResult {
    #[serde(default)]
    error: String,
    #[serde(default)]
    message: String,
    #[serde(default)]
    mime_type: String,
    #[serde(default)]
    size_bytes: i64,
    #[serde(default)]
    blob: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct TerminalExecError {
    code: String,
    message: String,
}

impl TerminalExecError {
    pub(crate) fn code(&self) -> &str {
        &self.code
    }
}

impl std::fmt::Display for TerminalExecError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.message)
    }
}

impl std::error::Error for TerminalExecError {}

#[derive(Debug, thiserror::Error)]
pub(crate) enum TerminalOperationError {
    #[error("command deadline exceeded")]
    DeadlineExceeded,
    #[error(transparent)]
    Terminal(#[from] TerminalExecError),
    #[error("{0}")]
    ExecutionFailed(String),
}

#[async_trait]
pub(crate) trait TerminalBackend: Send + Sync {
    async fn create_session_box(&self) -> Result<String, String>;
    async fn exec_shell_command(
        &self,
        box_id: &str,
        command: &str,
        deadline_unix_ms: i64,
    ) -> Result<CollectedExecOutput, BoxliteCommandError>;
    async fn exec_resource_probe(
        &self,
        box_id: &str,
        action: &str,
        file_path: &str,
        max_read_bytes: usize,
        deadline_unix_ms: i64,
    ) -> Result<CollectedExecOutput, BoxliteCommandError>;
    async fn copy_out_file(
        &self,
        box_id: &str,
        container_src: &str,
        host_dst: &Path,
        deadline_unix_ms: i64,
    ) -> Result<(), BoxliteCommandError>;
    async fn remove_box(&self, box_id: &str);
}

/// Publishes the outcome of box creation to callers waiting on a session.
#[derive(Clone, Debug, PartialEq, Eq)]
enum SessionReadyState {
    Pending,
    Ready(String),
    Failed(String),
}

#[derive(Debug)]
struct TerminalSession {
    session_id: String,
    box_id: String,
    lease_expires_at: SystemTime,
    /// Commands currently executing against this session. A session is idle,
    /// and therefore reclaimable, only at zero.
    inflight: u32,
    /// Stops the session accepting new commands. The box is removed by whichever
    /// caller drops inflight to zero.
    destroying: bool,
    /// Gates command execution on box creation. boxlite inserts sessions with an
    /// empty box_id before the microVM exists, so concurrent callers must wait
    /// here rather than exec against an empty id.
    ready_tx: watch::Sender<SessionReadyState>,
}

#[derive(Clone)]
pub(crate) struct TerminalSessionManagerConfig {
    pub lease_min_sec: u32,
    pub lease_max_sec: u32,
    pub lease_default_sec: u32,
    pub output_limit_bytes: usize,
    pub export_max_bytes: usize,
    pub session_max_inflight: u32,
}

pub(crate) struct TerminalSessionManager {
    backend: Arc<dyn TerminalBackend>,
    sessions: Mutex<HashMap<String, TerminalSession>>,
    lease_min_sec: u32,
    lease_max_sec: u32,
    lease_default_sec: u32,
    output_limit_bytes: usize,
    export_max_bytes: usize,
    session_max_inflight: u32,
    shutdown: CancellationToken,
    closed: AtomicBool,
}

pub(crate) fn shared_terminal_session_manager(cfg: &Config) -> Arc<TerminalSessionManager> {
    TERMINAL_SESSION_MANAGER
        .get_or_init(|| {
            TerminalSessionManager::new(
                TerminalSessionManagerConfig {
                    lease_min_sec: cfg.terminal_lease_min_sec,
                    lease_max_sec: cfg.terminal_lease_max_sec,
                    lease_default_sec: cfg.terminal_lease_default_sec,
                    output_limit_bytes: cfg.terminal_output_limit_bytes,
                    export_max_bytes: cfg.terminal_export_max_bytes,
                    session_max_inflight: cfg.terminal_session_max_inflight,
                },
                Arc::new(BoxliteTerminalBackend::new(cfg.clone())),
            )
        })
        .clone()
}

pub(crate) async fn shutdown_shared_terminal_sessions() {
    if let Some(manager) = TERMINAL_SESSION_MANAGER.get() {
        manager.close().await;
    }
}

pub(crate) async fn shared_active_session_count() -> i32 {
    match TERMINAL_SESSION_MANAGER.get() {
        Some(manager) => manager.active_session_count().await,
        None => 0,
    }
}

impl TerminalSessionManager {
    pub(crate) fn new(
        cfg: TerminalSessionManagerConfig,
        backend: Arc<dyn TerminalBackend>,
    ) -> Arc<Self> {
        let lease_min_sec = cfg.lease_min_sec.max(1);
        let mut lease_max_sec = cfg.lease_max_sec.max(lease_min_sec);
        if lease_max_sec < lease_min_sec {
            lease_max_sec = lease_min_sec;
        }

        let mut lease_default_sec = cfg.lease_default_sec.max(lease_min_sec);
        if lease_default_sec > lease_max_sec {
            lease_default_sec = lease_max_sec;
        }

        let manager = Arc::new(Self {
            backend,
            sessions: Mutex::new(HashMap::new()),
            lease_min_sec,
            lease_max_sec,
            lease_default_sec,
            output_limit_bytes: cfg.output_limit_bytes.max(1),
            export_max_bytes: cfg.export_max_bytes,
            session_max_inflight: cfg.session_max_inflight.max(1),
            shutdown: CancellationToken::new(),
            closed: AtomicBool::new(false),
        });

        let janitor = manager.clone();
        tokio::spawn(async move {
            janitor.janitor_loop().await;
        });

        manager
    }

    pub(crate) async fn execute(
        &self,
        req: TerminalExecRequest,
    ) -> Result<TerminalExecRunResult, TerminalOperationError> {
        let command = req.command.trim().to_owned();
        if command.is_empty() {
            return Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_INVALID_PAYLOAD.to_owned(),
                message: "command is required".to_owned(),
            }
            .into());
        }

        let lease_duration = self.resolve_lease_duration(req.lease_ttl_sec)?;
        let lease_target = add_duration(SystemTime::now(), lease_duration);
        let session_id = req.session_id.trim().to_owned();

        let claimed = self
            .claim_session_for_exec(session_id, req.create_if_missing, lease_target)
            .await?;

        let box_id = if claimed.created {
            match self.backend.create_session_box().await {
                Ok(box_id) => {
                    self.publish_session_ready(&claimed.session_id, &box_id)
                        .await;
                    box_id
                }
                Err(err) => {
                    self.publish_session_failed(&claimed.session_id, &err).await;
                    self.release_and_destroy_session(&claimed.session_id).await;
                    return Err(TerminalOperationError::ExecutionFailed(err));
                }
            }
        } else {
            match self.await_session_ready(&claimed).await {
                Ok(box_id) => box_id,
                Err(err) => {
                    self.release_session(&claimed.session_id).await;
                    return Err(err);
                }
            }
        };

        let exec_result = self
            .backend
            .exec_shell_command(&box_id, &command, req.deadline_unix_ms)
            .await;

        match exec_result {
            Ok(output) => {
                let (stdout, stdout_truncated) =
                    truncate_by_bytes(&output.stdout, self.output_limit_bytes);
                let (stderr, stderr_truncated) =
                    truncate_by_bytes(&output.stderr, self.output_limit_bytes);
                let lease_expires_at =
                    self.release_session(&claimed.session_id)
                        .await
                        .ok_or_else(|| TerminalExecError {
                            code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                            message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
                        })?;

                Ok(TerminalExecRunResult {
                    session_id: claimed.session_id,
                    created: claimed.created,
                    stdout,
                    stderr,
                    exit_code: output.exit_code,
                    stdout_truncated,
                    stderr_truncated,
                    lease_expires_unix_ms: to_unix_millis(lease_expires_at),
                })
            }
            Err(BoxliteCommandError::DeadlineExceeded) => {
                self.release_and_destroy_session(&claimed.session_id).await;
                Err(TerminalOperationError::DeadlineExceeded)
            }
            Err(BoxliteCommandError::MissingBox) => {
                self.release_and_destroy_session(&claimed.session_id).await;
                Err(TerminalExecError {
                    code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                    message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
                }
                .into())
            }
            Err(BoxliteCommandError::ExecutionFailed(message)) => {
                self.release_session(&claimed.session_id).await;
                Err(TerminalOperationError::ExecutionFailed(message))
            }
        }
    }

    pub(crate) async fn resolve_resource(
        &self,
        req: TerminalResourceRequest,
    ) -> Result<TerminalResourceRunResult, TerminalOperationError> {
        let session_id = req.session_id.trim().to_owned();
        let file_path = req.file_path.trim().to_owned();
        if session_id.is_empty() || file_path.is_empty() {
            return Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_INVALID_PAYLOAD.to_owned(),
                message: "session_id and file_path are required".to_owned(),
            }
            .into());
        }

        let action =
            normalize_terminal_resource_action(&req.action).ok_or_else(|| TerminalExecError {
                code: TERMINAL_EXEC_CODE_INVALID_PAYLOAD.to_owned(),
                message: "action must be validate, read, or export".to_owned(),
            })?;
        let signed_url = req.signed_url.trim().to_owned();
        if action == TERMINAL_RESOURCE_ACTION_EXPORT && signed_url.is_empty() {
            return Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_INVALID_PAYLOAD.to_owned(),
                message: "signed_url is required for export".to_owned(),
            }
            .into());
        }
        if action == TERMINAL_RESOURCE_ACTION_EXPORT
            && is_terminal_resource_export_path_disallowed(&file_path)
        {
            return Err(TerminalExecError {
                code: TERMINAL_RESOURCE_CODE_PATH_NOT_ALLOWED.to_owned(),
                message: "paths under /tmp are not allowed for export".to_owned(),
            }
            .into());
        }

        let claimed = self.claim_existing_session(&session_id).await?;
        let box_id = match self.await_session_ready(&claimed).await {
            Ok(box_id) => box_id,
            Err(err) => {
                self.release_session(&session_id).await;
                return Err(err);
            }
        };

        // The slot is released exactly once, here, so the inflight count stays
        // correct no matter which path the resource operation takes.
        match self
            .resolve_resource_in_session(
                &box_id,
                &session_id,
                &file_path,
                action,
                &signed_url,
                &req,
            )
            .await
        {
            Ok(result) => {
                self.release_session(&session_id).await;
                Ok(result)
            }
            Err(failure) => {
                if failure.destroy {
                    self.release_and_destroy_session(&session_id).await;
                } else {
                    self.release_session(&session_id).await;
                }
                Err(failure.error)
            }
        }
    }

    async fn resolve_resource_in_session(
        &self,
        box_id: &str,
        session_id: &str,
        file_path: &str,
        action: &str,
        signed_url: &str,
        req: &TerminalResourceRequest,
    ) -> Result<TerminalResourceRunResult, ResourceFailure> {
        let probe_action = if action == TERMINAL_RESOURCE_ACTION_EXPORT {
            TERMINAL_RESOURCE_ACTION_VALIDATE
        } else {
            action
        };

        let output = self
            .backend
            .exec_resource_probe(
                box_id,
                probe_action,
                file_path,
                self.output_limit_bytes,
                req.deadline_unix_ms,
            )
            .await
            .map_err(ResourceFailure::from_boxlite)?;

        let probe = decode_terminal_resource_probe_output(&output.stdout).map_err(|err| {
            ResourceFailure::retain(TerminalOperationError::ExecutionFailed(format!(
                "invalid terminalResource result: {err}"
            )))
        })?;
        if !probe.error.trim().is_empty() {
            return Err(ResourceFailure::retain(
                TerminalExecError {
                    code: probe.error.trim().to_owned(),
                    message: terminal_resource_error_message(&probe.error, &probe.message),
                }
                .into(),
            ));
        }
        if output.exit_code != 0 {
            return Err(ResourceFailure::retain(
                TerminalOperationError::ExecutionFailed(format!(
                    "terminalResource probe failed: exit_code={} stderr={}",
                    output.exit_code,
                    output.stderr.trim()
                )),
            ));
        }

        let mime_type = if probe.mime_type.trim().is_empty() {
            "application/octet-stream".to_owned()
        } else {
            probe.mime_type.trim().to_owned()
        };

        let result = TerminalResourceRunResult {
            session_id: session_id.to_owned(),
            file_path: file_path.to_owned(),
            mime_type,
            size_bytes: probe.size_bytes,
            blob: Vec::new(),
        };

        if action == TERMINAL_RESOURCE_ACTION_EXPORT {
            if self.export_max_bytes > 0 && probe.size_bytes > self.export_max_bytes as i64 {
                return Err(ResourceFailure::retain(
                    TerminalExecError {
                        code: TERMINAL_RESOURCE_CODE_FILE_TOO_LARGE.to_owned(),
                        message: "file exceeds export limit".to_owned(),
                    }
                    .into(),
                ));
            }

            let temp_path = build_export_temp_path();
            let _temp_path_guard = TempPathGuard::new(temp_path.clone());
            self.backend
                .copy_out_file(box_id, &result.file_path, &temp_path, req.deadline_unix_ms)
                .await
                .map_err(ResourceFailure::from_boxlite)?;
            put_file_to_signed_url(signed_url, &temp_path, &req.headers)
                .await
                .map_err(|err| {
                    ResourceFailure::retain(TerminalOperationError::ExecutionFailed(err))
                })?;
            return Ok(result);
        }

        let blob = if action == TERMINAL_RESOURCE_ACTION_READ {
            let encoded = probe.blob.trim();
            if encoded.is_empty() {
                Vec::new()
            } else {
                base64::engine::general_purpose::STANDARD
                    .decode(encoded)
                    .map_err(|err| {
                        ResourceFailure::retain(TerminalOperationError::ExecutionFailed(format!(
                            "decode resource blob: {err}"
                        )))
                    })?
            }
        } else {
            Vec::new()
        };

        Ok(TerminalResourceRunResult { blob, ..result })
    }

    pub(crate) async fn close(&self) {
        if self.closed.swap(true, Ordering::SeqCst) {
            return;
        }

        self.shutdown.cancel();
        let sessions = {
            let mut sessions = self.sessions.lock().await;
            sessions
                .drain()
                .map(|(_, session)| session.box_id)
                .filter(|box_id| !box_id.trim().is_empty())
                .collect::<Vec<_>>()
        };

        for box_id in sessions {
            self.backend.remove_box(&box_id).await;
        }
    }

    pub(crate) async fn active_session_count(&self) -> i32 {
        let sessions = self.sessions.lock().await;
        sessions
            .values()
            .filter(|session| !session.destroying)
            .count()
            .try_into()
            .unwrap_or(i32::MAX)
    }

    pub(crate) async fn cleanup_expired_sessions(&self) {
        let now = SystemTime::now();
        let expired = {
            let mut sessions = self.sessions.lock().await;
            let mut expired = Vec::new();
            sessions.retain(|_, session| {
                let should_remove =
                    session.inflight == 0 && !session.destroying && session.lease_expires_at <= now;
                if should_remove && !session.box_id.trim().is_empty() {
                    expired.push(session.box_id.clone());
                }
                !should_remove
            });
            expired
        };

        for box_id in expired {
            self.backend.remove_box(&box_id).await;
        }
    }

    async fn janitor_loop(self: Arc<Self>) {
        loop {
            tokio::select! {
                _ = self.shutdown.cancelled() => return,
                _ = tokio::time::sleep(TERMINAL_EXEC_JANITOR_INTERVAL) => {
                    self.cleanup_expired_sessions().await;
                }
            }
        }
    }

    fn resolve_lease_duration(
        &self,
        lease_ttl_sec: Option<i32>,
    ) -> Result<Duration, TerminalOperationError> {
        let lease_sec = lease_ttl_sec.unwrap_or(self.lease_default_sec as i32);
        if lease_sec < self.lease_min_sec as i32 || lease_sec > self.lease_max_sec as i32 {
            return Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_INVALID_PAYLOAD.to_owned(),
                message: format!(
                    "lease_ttl_sec must be between {} and {}",
                    self.lease_min_sec, self.lease_max_sec
                ),
            }
            .into());
        }
        Ok(Duration::from_secs(lease_sec as u64))
    }

    /// Reserves one inflight slot, creating the session when needed. An empty
    /// session_id always allocates a new session.
    async fn claim_session_for_exec(
        &self,
        session_id: String,
        create_if_missing: bool,
        lease_target: SystemTime,
    ) -> Result<ClaimedSession, TerminalOperationError> {
        let mut sessions = self.sessions.lock().await;
        if session_id.is_empty() {
            return Ok(Self::insert_pending_session(
                &mut sessions,
                Uuid::new_v4().to_string(),
                lease_target,
            ));
        }

        match sessions.get_mut(&session_id) {
            Some(session) if !session.destroying => {
                if session.inflight >= self.session_max_inflight {
                    return Err(TerminalExecError {
                        code: TERMINAL_EXEC_CODE_SESSION_BUSY.to_owned(),
                        message: TERMINAL_EXEC_BUSY_MESSAGE.to_owned(),
                    }
                    .into());
                }
                session.inflight += 1;
                if session.lease_expires_at < lease_target {
                    session.lease_expires_at = lease_target;
                }
                Ok(ClaimedSession {
                    session_id: session.session_id.clone(),
                    ready_rx: session.ready_tx.subscribe(),
                    created: false,
                })
            }
            // A session pending destruction still owns its id, so it is reported
            // as missing rather than being silently replaced.
            Some(_) => Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
            }
            .into()),
            None => {
                if !create_if_missing {
                    return Err(TerminalExecError {
                        code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                        message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
                    }
                    .into());
                }
                Ok(Self::insert_pending_session(
                    &mut sessions,
                    session_id,
                    lease_target,
                ))
            }
        }
    }

    fn insert_pending_session(
        sessions: &mut HashMap<String, TerminalSession>,
        session_id: String,
        lease_target: SystemTime,
    ) -> ClaimedSession {
        let (ready_tx, ready_rx) = watch::channel(SessionReadyState::Pending);
        sessions.insert(
            session_id.clone(),
            TerminalSession {
                session_id: session_id.clone(),
                box_id: String::new(),
                lease_expires_at: lease_target,
                inflight: 1,
                destroying: false,
                ready_tx,
            },
        );
        ClaimedSession {
            session_id,
            ready_rx,
            created: true,
        }
    }

    async fn claim_existing_session(
        &self,
        session_id: &str,
    ) -> Result<ClaimedSession, TerminalOperationError> {
        let mut sessions = self.sessions.lock().await;
        match sessions.get_mut(session_id) {
            Some(session) if !session.destroying => {
                if session.inflight >= self.session_max_inflight {
                    return Err(TerminalExecError {
                        code: TERMINAL_EXEC_CODE_SESSION_BUSY.to_owned(),
                        message: TERMINAL_EXEC_BUSY_MESSAGE.to_owned(),
                    }
                    .into());
                }
                session.inflight += 1;
                Ok(ClaimedSession {
                    session_id: session.session_id.clone(),
                    ready_rx: session.ready_tx.subscribe(),
                    created: false,
                })
            }
            _ => Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
            }
            .into()),
        }
    }

    /// Publishes a successfully created box to everyone waiting on the session.
    async fn publish_session_ready(&self, session_id: &str, box_id: &str) {
        let mut sessions = self.sessions.lock().await;
        if let Some(session) = sessions.get_mut(session_id) {
            session.box_id = box_id.to_owned();
            let _ = session
                .ready_tx
                .send(SessionReadyState::Ready(box_id.to_owned()));
        }
    }

    /// Publishes a creation failure so waiters fail with the creator's error
    /// instead of hanging or running against a box that never existed.
    async fn publish_session_failed(&self, session_id: &str, message: &str) {
        let sessions = self.sessions.lock().await;
        if let Some(session) = sessions.get(session_id) {
            let _ = session
                .ready_tx
                .send(SessionReadyState::Failed(message.to_owned()));
        }
    }

    /// Blocks until the session's box exists, then yields its id.
    async fn await_session_ready(
        &self,
        claimed: &ClaimedSession,
    ) -> Result<String, TerminalOperationError> {
        let mut ready_rx = claimed.ready_rx.clone();
        let state = ready_rx
            .wait_for(|state| !matches!(state, SessionReadyState::Pending))
            .await
            .map(|state| state.clone());

        match state {
            Ok(SessionReadyState::Ready(box_id)) => Ok(box_id),
            Ok(SessionReadyState::Failed(message)) => {
                Err(TerminalOperationError::ExecutionFailed(message))
            }
            // Pending is excluded by the predicate; a receive error means the
            // session was dropped while we waited.
            Ok(SessionReadyState::Pending) | Err(_) => Err(TerminalExecError {
                code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
            }
            .into()),
        }
    }

    /// Gives back one inflight slot and reports the current lease. A command
    /// that already produced a result still reports success here, even if the
    /// session is being torn down: the work completed and its output is valid.
    async fn release_session(&self, session_id: &str) -> Option<SystemTime> {
        let (lease, box_id) = {
            let mut sessions = self.sessions.lock().await;
            let session = sessions.get_mut(session_id)?;
            session.inflight = session.inflight.saturating_sub(1);
            let lease = session.lease_expires_at;
            let reclaim = session.destroying && session.inflight == 0;
            let box_id = if reclaim {
                sessions.remove(session_id).map(|session| session.box_id)
            } else {
                None
            };
            (lease, box_id)
        };

        if let Some(box_id) = box_id.filter(|value| !value.trim().is_empty()) {
            self.backend.remove_box(&box_id).await;
        }
        Some(lease)
    }

    /// Retires the caller's slot and marks the session for destruction. The box
    /// survives until the last concurrent command drains, so one command's
    /// timeout cannot kill its siblings.
    async fn release_and_destroy_session(&self, session_id: &str) {
        let box_id = {
            let mut sessions = self.sessions.lock().await;
            match sessions.get_mut(session_id) {
                Some(session) => {
                    session.destroying = true;
                    session.inflight = session.inflight.saturating_sub(1);
                    if session.inflight == 0 {
                        sessions.remove(session_id).map(|session| session.box_id)
                    } else {
                        None
                    }
                }
                None => None,
            }
        };

        if let Some(box_id) = box_id.filter(|value| !value.trim().is_empty()) {
            self.backend.remove_box(&box_id).await;
        }
    }

    #[cfg(test)]
    async fn insert_test_session(
        &self,
        session_id: &str,
        box_id: &str,
        lease_expires_at: SystemTime,
        inflight: u32,
    ) {
        let mut sessions = self.sessions.lock().await;
        let (ready_tx, _) = watch::channel(SessionReadyState::Ready(box_id.to_owned()));
        sessions.insert(
            session_id.to_owned(),
            TerminalSession {
                session_id: session_id.to_owned(),
                box_id: box_id.to_owned(),
                lease_expires_at,
                inflight,
                destroying: false,
                ready_tx,
            },
        );
    }

    #[cfg(test)]
    async fn get_test_session(&self, session_id: &str) -> Option<(String, SystemTime, u32)> {
        let sessions = self.sessions.lock().await;
        sessions.get(session_id).map(|session| {
            (
                session.box_id.clone(),
                session.lease_expires_at,
                session.inflight,
            )
        })
    }
}

struct ClaimedSession {
    session_id: String,
    ready_rx: watch::Receiver<SessionReadyState>,
    created: bool,
}

/// A resource failure plus whether it should tear the session down.
struct ResourceFailure {
    error: TerminalOperationError,
    destroy: bool,
}

impl ResourceFailure {
    /// The session stays usable; only this operation failed.
    fn retain(error: TerminalOperationError) -> Self {
        Self {
            error,
            destroy: false,
        }
    }

    fn from_boxlite(err: BoxliteCommandError) -> Self {
        match err {
            BoxliteCommandError::DeadlineExceeded => Self {
                error: TerminalOperationError::DeadlineExceeded,
                destroy: true,
            },
            BoxliteCommandError::MissingBox => Self {
                error: TerminalExecError {
                    code: TERMINAL_EXEC_CODE_SESSION_NOT_FOUND.to_owned(),
                    message: TERMINAL_EXEC_NO_SESSION_MESSAGE.to_owned(),
                }
                .into(),
                destroy: true,
            },
            BoxliteCommandError::ExecutionFailed(message) => Self {
                error: TerminalOperationError::ExecutionFailed(message),
                destroy: false,
            },
        }
    }
}

struct BoxliteTerminalBackend {
    cfg: Config,
}

impl BoxliteTerminalBackend {
    fn new(cfg: Config) -> Self {
        Self { cfg }
    }
}

#[async_trait]
impl TerminalBackend for BoxliteTerminalBackend {
    async fn create_session_box(&self) -> Result<String, String> {
        let name = format!("worker-boxlite-terminal-{}", Uuid::new_v4());
        boxlite_runtime::create_terminal_session_box(&self.cfg, &name)
            .await
            .map_err(|err| err.to_string())
    }

    async fn exec_shell_command(
        &self,
        box_id: &str,
        command: &str,
        deadline_unix_ms: i64,
    ) -> Result<CollectedExecOutput, BoxliteCommandError> {
        boxlite_runtime::exec_terminal_shell(&self.cfg, box_id, command, deadline_unix_ms).await
    }

    async fn exec_resource_probe(
        &self,
        box_id: &str,
        action: &str,
        file_path: &str,
        max_read_bytes: usize,
        deadline_unix_ms: i64,
    ) -> Result<CollectedExecOutput, BoxliteCommandError> {
        boxlite_runtime::exec_terminal_resource_probe(
            &self.cfg,
            box_id,
            action,
            file_path,
            max_read_bytes,
            deadline_unix_ms,
        )
        .await
    }

    async fn copy_out_file(
        &self,
        box_id: &str,
        container_src: &str,
        host_dst: &Path,
        deadline_unix_ms: i64,
    ) -> Result<(), BoxliteCommandError> {
        boxlite_runtime::copy_out_terminal_file(
            &self.cfg,
            box_id,
            container_src,
            host_dst,
            deadline_unix_ms,
        )
        .await
    }

    async fn remove_box(&self, box_id: &str) {
        boxlite_runtime::remove_box(&self.cfg, box_id).await;
    }
}

fn truncate_by_bytes(value: &str, max_bytes: usize) -> (String, bool) {
    if max_bytes == 0 || value.len() <= max_bytes {
        return (value.to_owned(), false);
    }

    let mut boundary = max_bytes.min(value.len());
    while boundary > 0 && !value.is_char_boundary(boundary) {
        boundary -= 1;
    }
    (value[..boundary].to_owned(), true)
}

fn normalize_terminal_resource_action(action: &str) -> Option<&'static str> {
    match action.trim().to_ascii_lowercase().as_str() {
        "" => Some(TERMINAL_RESOURCE_ACTION_VALIDATE),
        TERMINAL_RESOURCE_ACTION_VALIDATE => Some(TERMINAL_RESOURCE_ACTION_VALIDATE),
        TERMINAL_RESOURCE_ACTION_READ => Some(TERMINAL_RESOURCE_ACTION_READ),
        TERMINAL_RESOURCE_ACTION_EXPORT => Some(TERMINAL_RESOURCE_ACTION_EXPORT),
        _ => None,
    }
}

fn decode_terminal_resource_probe_output(
    stdout: &str,
) -> Result<TerminalResourceProbeResult, String> {
    let trimmed = stdout.trim();
    if trimmed.is_empty() {
        return Err("empty output".to_owned());
    }
    serde_json::from_str(trimmed).map_err(|err| err.to_string())
}

fn terminal_resource_error_message(code: &str, fallback: &str) -> String {
    if !fallback.trim().is_empty() {
        return fallback.trim().to_owned();
    }
    match code.trim() {
        TERMINAL_RESOURCE_CODE_FILE_NOT_FOUND => "file not found".to_owned(),
        TERMINAL_RESOURCE_CODE_PATH_IS_DIR => "path is directory".to_owned(),
        TERMINAL_RESOURCE_CODE_PATH_NOT_ALLOWED => "file path is not allowed".to_owned(),
        TERMINAL_RESOURCE_CODE_FILE_TOO_LARGE => "file exceeds read limit".to_owned(),
        _ => "terminal resource operation failed".to_owned(),
    }
}

fn is_terminal_resource_export_path_disallowed(file_path: &str) -> bool {
    let trimmed = file_path.trim();
    trimmed == "/tmp" || trimmed.starts_with("/tmp/")
}

fn add_duration(time: SystemTime, duration: Duration) -> SystemTime {
    time.checked_add(duration).unwrap_or(time)
}

fn build_export_temp_path() -> PathBuf {
    std::env::temp_dir().join(format!("onlyboxes-export-{}", Uuid::new_v4()))
}

async fn put_file_to_signed_url(
    signed_url: &str,
    file_path: &Path,
    headers: &HashMap<String, String>,
) -> Result<(), String> {
    let upload_path = file_path.to_path_buf();
    let file = tokio::fs::File::open(&upload_path)
        .await
        .map_err(|err| format!("open export file: {err}"))?;
    let file_size = file
        .metadata()
        .await
        .map_err(|err| format!("stat export file: {err}"))?
        .len();
    let payload = reqwest::Body::wrap_stream(ReaderStream::new(file));
    let mut request = reqwest::Client::new()
        .put(signed_url)
        .header(reqwest::header::CONTENT_LENGTH, file_size.to_string());
    for (key, value) in headers {
        let trimmed_key = key.trim();
        if trimmed_key.is_empty() {
            continue;
        }
        request = request.header(trimmed_key, value);
    }
    let response = request
        .body(payload)
        .send()
        .await
        .map_err(|err| format!("upload export file: {err}"))?;
    if response.status().is_success() {
        return Ok(());
    }

    let status = response.status();
    let body = response
        .text()
        .await
        .map_err(|err| format!("read export upload error body: {err}"))?;
    let message = if body.trim().is_empty() {
        status
            .canonical_reason()
            .unwrap_or_else(|| match status {
                StatusCode::BAD_REQUEST => "Bad Request",
                _ => status.as_str(),
            })
            .to_owned()
    } else {
        body.trim().to_owned()
    };
    Err(format!("upload export file failed: {message}"))
}

fn to_unix_millis(time: SystemTime) -> i64 {
    time.duration_since(SystemTime::UNIX_EPOCH)
        .map(|duration| duration.as_millis() as i64)
        .unwrap_or_default()
}

fn serialize_resource_blob<S>(blob: &[u8], serializer: S) -> Result<S::Ok, S::Error>
where
    S: serde::Serializer,
{
    serializer.serialize_str(&base64::engine::general_purpose::STANDARD.encode(blob))
}

fn deserialize_resource_blob<'de, D>(deserializer: D) -> Result<Vec<u8>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let encoded = String::deserialize(deserializer)?;
    if encoded.trim().is_empty() {
        return Ok(Vec::new());
    }
    base64::engine::general_purpose::STANDARD
        .decode(encoded.trim())
        .map_err(serde::de::Error::custom)
}

struct TempPathGuard {
    path: PathBuf,
}

impl TempPathGuard {
    fn new(path: PathBuf) -> Self {
        Self { path }
    }
}

impl Drop for TempPathGuard {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;
    use tokio::sync::Notify;

    struct StatefulShellBackend {
        next_box: Mutex<u32>,
        persisted: Mutex<HashMap<String, String>>,
        removed: Mutex<Vec<String>>,
    }

    impl StatefulShellBackend {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                next_box: Mutex::new(0),
                persisted: Mutex::new(HashMap::new()),
                removed: Mutex::new(Vec::new()),
            })
        }
    }

    #[async_trait]
    impl TerminalBackend for StatefulShellBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            let mut next = self.next_box.lock().await;
            *next += 1;
            Ok(format!("box-{next}"))
        }

        async fn exec_shell_command(
            &self,
            box_id: &str,
            command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            let mut persisted = self.persisted.lock().await;
            match command {
                "set-persist-value" => {
                    persisted.insert(box_id.to_owned(), "persisted\n".to_owned());
                    Ok(CollectedExecOutput {
                        stdout: String::new(),
                        stderr: String::new(),
                        exit_code: 0,
                    })
                }
                "get-persist-value" => Ok(CollectedExecOutput {
                    stdout: persisted.get(box_id).cloned().unwrap_or_default(),
                    stderr: String::new(),
                    exit_code: 0,
                }),
                "big-output" => Ok(CollectedExecOutput {
                    stdout: "1234567890".to_owned(),
                    stderr: "abcdefghij".to_owned(),
                    exit_code: 0,
                }),
                other => Ok(CollectedExecOutput {
                    stdout: other.to_owned(),
                    stderr: String::new(),
                    exit_code: 0,
                }),
            }
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            let stdout = match action {
                TERMINAL_RESOURCE_ACTION_VALIDATE => {
                    r#"{"mime_type":"text/plain","size_bytes":5}"#.to_owned()
                }
                TERMINAL_RESOURCE_ACTION_READ => {
                    r#"{"mime_type":"text/plain","size_bytes":5,"blob":"aGVsbG8="}"#.to_owned()
                }
                _ => String::new(),
            };
            Ok(CollectedExecOutput {
                stdout,
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            std::fs::write(host_dst, b"hello")
                .map_err(|err| BoxliteCommandError::ExecutionFailed(err.to_string()))
        }

        async fn remove_box(&self, box_id: &str) {
            self.removed.lock().await.push(box_id.to_owned());
        }
    }

    struct BlockingShellBackend {
        started: Notify,
        gate: Notify,
        removed: Mutex<Vec<String>>,
    }

    #[async_trait]
    impl TerminalBackend for BlockingShellBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-1".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            if command == "block-command" {
                self.started.notify_waiters();
                self.gate.notified().await;
            }
            Ok(CollectedExecOutput {
                stdout: String::new(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            unreachable!()
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, box_id: &str) {
            self.removed.lock().await.push(box_id.to_owned());
        }
    }

    struct BlockingMixedBackend {
        exec_started: Notify,
        resource_started: Notify,
        gate: Notify,
    }

    #[async_trait]
    impl TerminalBackend for BlockingMixedBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-mixed".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            _command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            self.exec_started.notify_waiters();
            self.gate.notified().await;
            Ok(CollectedExecOutput {
                stdout: String::new(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            self.resource_started.notify_waiters();
            self.gate.notified().await;
            Ok(CollectedExecOutput {
                stdout: r#"{"mime_type":"text/plain","size_bytes":1}"#.to_owned(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            std::fs::write(host_dst, b"x")
                .map_err(|err| BoxliteCommandError::ExecutionFailed(err.to_string()))
        }

        async fn remove_box(&self, _box_id: &str) {}
    }

    struct EmptyResourceBackend;

    #[async_trait]
    impl TerminalBackend for EmptyResourceBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            unreachable!()
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            _command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            unreachable!()
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            Ok(CollectedExecOutput {
                stdout: "   ".to_owned(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, _box_id: &str) {}
    }

    struct TimeoutShellBackend {
        removed: Mutex<Vec<String>>,
    }

    #[async_trait]
    impl TerminalBackend for TimeoutShellBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-timeout".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            _command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            Err(BoxliteCommandError::DeadlineExceeded)
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            Err(BoxliteCommandError::DeadlineExceeded)
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            Err(BoxliteCommandError::DeadlineExceeded)
        }

        async fn remove_box(&self, box_id: &str) {
            self.removed.lock().await.push(box_id.to_owned());
        }
    }

    struct DomainResourceBackend {
        stdout: String,
        exit_code: i32,
    }

    #[async_trait]
    impl TerminalBackend for DomainResourceBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-domain".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            _command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            unreachable!()
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            Ok(CollectedExecOutput {
                stdout: self.stdout.clone(),
                stderr: String::new(),
                exit_code: self.exit_code,
            })
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, _box_id: &str) {}
    }

    #[derive(Debug)]
    struct PutRequestCapture {
        headers: String,
        body: Vec<u8>,
    }

    async fn spawn_put_server(
        status_line: &str,
    ) -> (String, tokio::sync::oneshot::Receiver<PutRequestCapture>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (body_tx, body_rx) = tokio::sync::oneshot::channel();
        let status = status_line.to_owned();

        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut header_buf = Vec::new();
            let mut chunk = [0u8; 1024];
            loop {
                let read = stream.read(&mut chunk).await.unwrap();
                if read == 0 {
                    break;
                }
                header_buf.extend_from_slice(&chunk[..read]);
                if header_buf.windows(4).any(|window| window == b"\r\n\r\n") {
                    break;
                }
            }

            let header_end = header_buf
                .windows(4)
                .position(|window| window == b"\r\n\r\n")
                .map(|index| index + 4)
                .unwrap();
            let headers = String::from_utf8_lossy(&header_buf[..header_end]);
            let content_length = headers
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    if name.eq_ignore_ascii_case("content-length") {
                        value.trim().parse::<usize>().ok()
                    } else {
                        None
                    }
                })
                .unwrap_or(0);

            let mut body = header_buf[header_end..].to_vec();
            while body.len() < content_length {
                let read = stream.read(&mut chunk).await.unwrap();
                if read == 0 {
                    break;
                }
                body.extend_from_slice(&chunk[..read]);
            }
            let _ = body_tx.send(PutRequestCapture {
                headers: headers.into_owned(),
                body,
            });
            let response = format!("{status}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
            stream.write_all(response.as_bytes()).await.unwrap();
        });

        (format!("http://{address}/upload"), body_rx)
    }

    fn manager_with_backend(
        backend: Arc<dyn TerminalBackend>,
        output_limit_bytes: usize,
    ) -> Arc<TerminalSessionManager> {
        TerminalSessionManager::new(
            TerminalSessionManagerConfig {
                lease_min_sec: 60,
                lease_max_sec: 1800,
                lease_default_sec: 60,
                output_limit_bytes,
                export_max_bytes: 0,
                session_max_inflight: 1,
            },
            backend,
        )
    }

    #[tokio::test]
    async fn session_reuse_preserves_box_state() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend, 1024 * 1024);

        let first = manager
            .execute(TerminalExecRequest {
                command: "set-persist-value".to_owned(),
                session_id: String::new(),
                create_if_missing: false,
                lease_ttl_sec: None,
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();
        assert!(first.created);
        assert!(!first.session_id.is_empty());

        let second = manager
            .execute(TerminalExecRequest {
                command: "get-persist-value".to_owned(),
                session_id: first.session_id.clone(),
                create_if_missing: false,
                lease_ttl_sec: None,
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();
        assert!(!second.created);
        assert_eq!(second.stdout, "persisted\n");

        manager.close().await;
    }

    #[tokio::test]
    async fn busy_session_returns_session_busy() {
        let backend = Arc::new(BlockingShellBackend {
            started: Notify::new(),
            gate: Notify::new(),
            removed: Mutex::new(Vec::new()),
        });
        let manager = manager_with_backend(backend.clone(), 1024 * 1024);
        let started = backend.started.notified();

        let manager_for_task = manager.clone();
        let first = tokio::spawn(async move {
            manager_for_task
                .execute(TerminalExecRequest {
                    command: "block-command".to_owned(),
                    session_id: String::new(),
                    create_if_missing: false,
                    lease_ttl_sec: None,
                    deadline_unix_ms: 0,
                })
                .await
        });

        started.await;
        let session_id = manager
            .sessions
            .lock()
            .await
            .keys()
            .next()
            .cloned()
            .expect("session created");

        let err = manager
            .execute(TerminalExecRequest {
                command: "any-command".to_owned(),
                session_id,
                create_if_missing: false,
                lease_ttl_sec: None,
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();

        match err {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_EXEC_CODE_SESSION_BUSY);
            }
            other => panic!("unexpected error: {other}"),
        }

        backend.gate.notify_waiters();
        first.await.unwrap().unwrap();
        manager.close().await;
    }

    #[tokio::test]
    async fn timeout_destroys_session() {
        let backend = Arc::new(TimeoutShellBackend {
            removed: Mutex::new(Vec::new()),
        });
        let manager = manager_with_backend(backend.clone(), 1024);

        let err = manager
            .execute(TerminalExecRequest {
                command: "timeout-command".to_owned(),
                session_id: "timeout-session".to_owned(),
                create_if_missing: true,
                lease_ttl_sec: None,
                deadline_unix_ms: 1,
            })
            .await
            .unwrap_err();
        assert!(matches!(err, TerminalOperationError::DeadlineExceeded));
        assert!(manager.get_test_session("timeout-session").await.is_none());
        assert_eq!(backend.removed.lock().await.as_slice(), &["box-timeout"]);

        manager.close().await;
    }

    #[tokio::test]
    async fn lease_not_reduced_and_output_truncated() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend, 4);

        let high = 120;
        let first = manager
            .execute(TerminalExecRequest {
                command: "big-output".to_owned(),
                session_id: String::new(),
                create_if_missing: false,
                lease_ttl_sec: Some(high),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();
        assert_eq!(first.stdout, "1234");
        assert_eq!(first.stderr, "abcd");
        assert!(first.stdout_truncated);
        assert!(first.stderr_truncated);

        let low = 60;
        let second = manager
            .execute(TerminalExecRequest {
                command: "big-output".to_owned(),
                session_id: first.session_id.clone(),
                create_if_missing: false,
                lease_ttl_sec: Some(low),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();
        assert!(second.lease_expires_unix_ms >= first.lease_expires_unix_ms);

        manager.close().await;
    }

    #[tokio::test]
    async fn output_truncation_keeps_valid_utf8() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend, 5);

        let result = manager
            .execute(TerminalExecRequest {
                command: "你好世界".to_owned(),
                session_id: String::new(),
                create_if_missing: false,
                lease_ttl_sec: None,
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();

        assert_eq!(result.stdout, "你");
        assert!(result.stdout_truncated);
        assert!(manager.get_test_session(&result.session_id).await.is_some());

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_validate_and_read() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend, 1024);
        manager
            .insert_test_session(
                "sess-1",
                "box-1",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;

        let validate = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-1".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_VALIDATE.to_owned(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();
        assert_eq!(validate.size_bytes, 5);
        assert_eq!(validate.mime_type, "text/plain");

        let read = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-1".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_READ.to_owned(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();
        assert_eq!(read.blob, b"hello");

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_export_copies_and_uploads() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend, 1024);
        manager
            .insert_test_session(
                "sess-export",
                "box-export",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;
        let (upload_url, uploaded_request) = spawn_put_server("HTTP/1.1 200 OK").await;

        let result = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-export".to_owned(),
                file_path: "/root/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_EXPORT.to_owned(),
                signed_url: upload_url,
                headers: HashMap::from([("x-amz-acl".to_owned(), "public-read".to_owned())]),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap();

        assert_eq!(result.mime_type, "text/plain");
        assert_eq!(result.size_bytes, 5);
        assert!(result.blob.is_empty());
        let uploaded_request = uploaded_request.await.unwrap();
        assert!(uploaded_request
            .headers
            .lines()
            .any(|line| line.eq_ignore_ascii_case("content-length: 5")));
        assert!(uploaded_request
            .headers
            .lines()
            .any(|line| line.eq_ignore_ascii_case("x-amz-acl: public-read")));
        assert_eq!(uploaded_request.body, b"hello");

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_export_requires_signed_url() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend, 1024);
        manager
            .insert_test_session(
                "sess-export-missing-url",
                "box-export-missing-url",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;

        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-export-missing-url".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_EXPORT.to_owned(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();

        match err {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_EXEC_CODE_INVALID_PAYLOAD)
            }
            other => panic!("unexpected error: {other}"),
        }

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_export_rejects_tmp_paths_before_session_lookup() {
        let manager = manager_with_backend(StatefulShellBackend::new(), 1024);

        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "missing-session".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_EXPORT.to_owned(),
                signed_url: "https://uploads.example.com/put".to_owned(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();

        match err {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_RESOURCE_CODE_PATH_NOT_ALLOWED)
            }
            other => panic!("unexpected error: {other}"),
        }

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_export_rejects_oversized_file() {
        let backend = StatefulShellBackend::new() as Arc<dyn TerminalBackend>;
        let manager = TerminalSessionManager::new(
            TerminalSessionManagerConfig {
                lease_min_sec: 60,
                lease_max_sec: 1800,
                lease_default_sec: 60,
                output_limit_bytes: 1024,
                export_max_bytes: 3,
                session_max_inflight: 1,
            },
            backend,
        );
        manager
            .insert_test_session(
                "sess-export-oversized",
                "box-export-oversized",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;

        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-export-oversized".to_owned(),
                file_path: "/root/large.bin".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_EXPORT.to_owned(),
                signed_url: "https://uploads.example.com/put".to_owned(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();

        match err {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_RESOURCE_CODE_FILE_TOO_LARGE)
            }
            other => panic!("unexpected error: {other}"),
        }

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_domain_errors_map_to_fixed_codes() {
        let cases = [
            (
                TERMINAL_RESOURCE_CODE_FILE_NOT_FOUND,
                r#"{"error":"file_not_found","message":"file not found"}"#,
                10,
            ),
            (
                TERMINAL_RESOURCE_CODE_PATH_IS_DIR,
                r#"{"error":"path_is_directory","message":"path is directory"}"#,
                11,
            ),
            (
                TERMINAL_RESOURCE_CODE_FILE_TOO_LARGE,
                r#"{"error":"file_too_large","message":"file exceeds read limit"}"#,
                12,
            ),
        ];

        for (code, stdout, exit_code) in cases {
            let manager = manager_with_backend(
                Arc::new(DomainResourceBackend {
                    stdout: stdout.to_owned(),
                    exit_code,
                }),
                1024,
            );
            manager
                .insert_test_session(
                    "sess-1",
                    "box-1",
                    add_duration(SystemTime::now(), Duration::from_secs(60)),
                    0,
                )
                .await;

            let err = manager
                .resolve_resource(TerminalResourceRequest {
                    session_id: "sess-1".to_owned(),
                    file_path: "/tmp/hello.txt".to_owned(),
                    action: TERMINAL_RESOURCE_ACTION_READ.to_owned(),
                    signed_url: String::new(),
                    headers: HashMap::new(),
                    deadline_unix_ms: 0,
                })
                .await
                .unwrap_err();

            match err {
                TerminalOperationError::Terminal(err) => assert_eq!(err.code(), code),
                other => panic!("unexpected error: {other}"),
            }

            manager.close().await;
        }
    }

    #[tokio::test]
    async fn resolve_resource_empty_output_reports_empty_output() {
        let manager = manager_with_backend(Arc::new(EmptyResourceBackend), 1024);
        manager
            .insert_test_session(
                "sess-empty",
                "box-empty",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;

        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-empty".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_VALIDATE.to_owned(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();

        match err {
            TerminalOperationError::ExecutionFailed(message) => {
                assert_eq!(message, "invalid terminalResource result: empty output");
            }
            other => panic!("unexpected error: {other}"),
        }

        manager.close().await;
    }

    #[tokio::test]
    async fn resolve_resource_enforces_session_rules_and_timeout_cleanup() {
        let timeout_backend = Arc::new(TimeoutShellBackend {
            removed: Mutex::new(Vec::new()),
        });
        let manager = manager_with_backend(timeout_backend.clone(), 1024);

        let missing = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "missing".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: String::new(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();
        match missing {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_EXEC_CODE_SESSION_NOT_FOUND)
            }
            other => panic!("unexpected error: {other}"),
        }

        manager
            .insert_test_session(
                "busy",
                "box-busy",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                1,
            )
            .await;
        let busy = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "busy".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: String::new(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();
        match busy {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_EXEC_CODE_SESSION_BUSY)
            }
            other => panic!("unexpected error: {other}"),
        }

        manager
            .insert_test_session(
                "sess-timeout",
                "box-timeout",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;
        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-timeout".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_READ.to_owned(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();
        assert!(matches!(err, TerminalOperationError::DeadlineExceeded));
        assert!(manager.get_test_session("sess-timeout").await.is_none());
        assert_eq!(
            timeout_backend.removed.lock().await.as_slice(),
            &["box-timeout"]
        );

        manager.close().await;
    }

    #[tokio::test]
    async fn same_session_exec_and_resource_share_busy_state() {
        let backend = Arc::new(BlockingMixedBackend {
            exec_started: Notify::new(),
            resource_started: Notify::new(),
            gate: Notify::new(),
        });
        let manager = manager_with_backend(backend.clone(), 1024);
        let exec_started = backend.exec_started.notified();

        let manager_for_task = manager.clone();
        let first = tokio::spawn(async move {
            manager_for_task
                .execute(TerminalExecRequest {
                    command: "block-command".to_owned(),
                    session_id: "sess-shared".to_owned(),
                    create_if_missing: true,
                    lease_ttl_sec: None,
                    deadline_unix_ms: 0,
                })
                .await
        });

        exec_started.await;
        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-shared".to_owned(),
                file_path: "/tmp/hello.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_VALIDATE.to_owned(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .unwrap_err();

        match err {
            TerminalOperationError::Terminal(err) => {
                assert_eq!(err.code(), TERMINAL_EXEC_CODE_SESSION_BUSY)
            }
            other => panic!("unexpected error: {other}"),
        }

        backend.gate.notify_waiters();
        first.await.unwrap().unwrap();
        manager.close().await;
    }

    #[tokio::test]
    async fn different_sessions_can_run_concurrently_across_terminal_capabilities() {
        let backend = Arc::new(BlockingMixedBackend {
            exec_started: Notify::new(),
            resource_started: Notify::new(),
            gate: Notify::new(),
        });
        let manager = manager_with_backend(backend.clone(), 1024);
        let exec_started = backend.exec_started.notified();
        let resource_started = backend.resource_started.notified();
        manager
            .insert_test_session(
                "sess-resource",
                "box-resource",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;

        let exec_manager = manager.clone();
        let exec_task = tokio::spawn(async move {
            exec_manager
                .execute(TerminalExecRequest {
                    command: "block-command".to_owned(),
                    session_id: "sess-exec".to_owned(),
                    create_if_missing: true,
                    lease_ttl_sec: None,
                    deadline_unix_ms: 0,
                })
                .await
        });

        let resource_manager = manager.clone();
        let resource_task = tokio::spawn(async move {
            resource_manager
                .resolve_resource(TerminalResourceRequest {
                    session_id: "sess-resource".to_owned(),
                    file_path: "/tmp/hello.txt".to_owned(),
                    action: TERMINAL_RESOURCE_ACTION_VALIDATE.to_owned(),
                    signed_url: String::new(),
                    headers: HashMap::new(),
                    deadline_unix_ms: 0,
                })
                .await
        });

        exec_started.await;
        resource_started.await;
        backend.gate.notify_waiters();
        backend.gate.notify_waiters();

        let exec_result = exec_task.await.unwrap().unwrap();
        let resource_result = resource_task.await.unwrap().unwrap();
        assert_eq!(exec_result.session_id, "sess-exec");
        assert_eq!(resource_result.session_id, "sess-resource");

        manager.close().await;
    }

    #[tokio::test]
    async fn cleanup_expired_sessions_only_removes_idle_entries() {
        let backend = StatefulShellBackend::new();
        let manager = manager_with_backend(backend.clone(), 1024);
        manager
            .insert_test_session(
                "expired",
                "box-expired",
                SystemTime::now() - Duration::from_secs(1),
                0,
            )
            .await;
        manager
            .insert_test_session(
                "busy",
                "box-busy",
                SystemTime::now() - Duration::from_secs(1),
                1,
            )
            .await;
        manager
            .insert_test_session(
                "active",
                "box-active",
                add_duration(SystemTime::now(), Duration::from_secs(60)),
                0,
            )
            .await;

        manager.cleanup_expired_sessions().await;

        assert!(manager.get_test_session("expired").await.is_none());
        assert!(manager.get_test_session("busy").await.is_some());
        assert!(manager.get_test_session("active").await.is_some());
        assert_eq!(backend.removed.lock().await.as_slice(), &["box-expired"]);

        manager.close().await;
    }

    // ---- per-session concurrency ----

    fn manager_with_limit(
        backend: Arc<dyn TerminalBackend>,
        session_max_inflight: u32,
    ) -> Arc<TerminalSessionManager> {
        TerminalSessionManager::new(
            TerminalSessionManagerConfig {
                lease_min_sec: 60,
                lease_max_sec: 1800,
                lease_default_sec: 60,
                output_limit_bytes: 1024 * 1024,
                export_max_bytes: 0,
                session_max_inflight,
            },
            backend,
        )
    }

    /// A gate that never loses a wakeup, unlike `Notify::notify_waiters`.
    struct Gate {
        tx: watch::Sender<bool>,
    }

    impl Gate {
        fn new() -> Self {
            Self {
                tx: watch::channel(false).0,
            }
        }

        async fn wait(&self) {
            let mut rx = self.tx.subscribe();
            let _ = rx.wait_for(|open| *open).await;
        }

        fn open(&self) {
            let _ = self.tx.send(true);
        }
    }

    /// Holds box creation open so a second caller can race the creator.
    struct GatedCreateBackend {
        create_gate: Gate,
        create_started: Gate,
        create_result: Result<String, String>,
        exec_box_ids: Mutex<Vec<String>>,
        removed: Mutex<Vec<String>>,
    }

    impl GatedCreateBackend {
        fn new(create_result: Result<String, String>) -> Arc<Self> {
            Arc::new(Self {
                create_gate: Gate::new(),
                create_started: Gate::new(),
                create_result,
                exec_box_ids: Mutex::new(Vec::new()),
                removed: Mutex::new(Vec::new()),
            })
        }
    }

    #[async_trait]
    impl TerminalBackend for GatedCreateBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            self.create_started.open();
            self.create_gate.wait().await;
            self.create_result.clone()
        }

        async fn exec_shell_command(
            &self,
            box_id: &str,
            _command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            self.exec_box_ids.lock().await.push(box_id.to_owned());
            Ok(CollectedExecOutput {
                stdout: String::new(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            unreachable!()
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, box_id: &str) {
            self.removed.lock().await.push(box_id.to_owned());
        }
    }

    /// One command blocks until released; another reports a deadline.
    struct SiblingBackend {
        sibling_started: Gate,
        sibling_gate: Gate,
        removed: Mutex<Vec<String>>,
    }

    impl SiblingBackend {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                sibling_started: Gate::new(),
                sibling_gate: Gate::new(),
                removed: Mutex::new(Vec::new()),
            })
        }
    }

    #[async_trait]
    impl TerminalBackend for SiblingBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-sibling".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            match command {
                "sibling" => {
                    self.sibling_started.open();
                    self.sibling_gate.wait().await;
                    Ok(CollectedExecOutput {
                        stdout: "sibling-ok".to_owned(),
                        stderr: String::new(),
                        exit_code: 0,
                    })
                }
                "doomed" => Err(BoxliteCommandError::DeadlineExceeded),
                _ => Ok(CollectedExecOutput {
                    stdout: String::new(),
                    stderr: String::new(),
                    exit_code: 0,
                }),
            }
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            unreachable!()
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, box_id: &str) {
            self.removed.lock().await.push(box_id.to_owned());
        }
    }

    fn exec_req(session_id: &str, command: &str, create_if_missing: bool) -> TerminalExecRequest {
        TerminalExecRequest {
            command: command.to_owned(),
            session_id: session_id.to_owned(),
            create_if_missing,
            lease_ttl_sec: None,
            deadline_unix_ms: 0,
        }
    }

    /// Counts how many commands are parked inside the backend at once.
    struct ConcurrentExecBackend {
        started: Mutex<u32>,
        gate: Gate,
        removed: Mutex<Vec<String>>,
    }

    impl ConcurrentExecBackend {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                started: Mutex::new(0),
                gate: Gate::new(),
                removed: Mutex::new(Vec::new()),
            })
        }

        async fn started_count(&self) -> u32 {
            *self.started.lock().await
        }
    }

    #[async_trait]
    impl TerminalBackend for ConcurrentExecBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-concurrent".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            if command == "block-command" {
                *self.started.lock().await += 1;
                self.gate.wait().await;
            }
            Ok(CollectedExecOutput {
                stdout: String::new(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            unreachable!()
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, box_id: &str) {
            self.removed.lock().await.push(box_id.to_owned());
        }
    }

    async fn wait_until<F, Fut>(label: &str, mut condition: F)
    where
        F: FnMut() -> Fut,
        Fut: std::future::Future<Output = bool>,
    {
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        loop {
            if condition().await {
                return;
            }
            assert!(std::time::Instant::now() < deadline, "timed out: {label}");
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    #[tokio::test]
    async fn concurrent_exec_allowed_up_to_session_limit() {
        let backend = ConcurrentExecBackend::new();
        let manager = manager_with_limit(backend.clone(), 2);

        let seed = manager
            .execute(exec_req("", "seed", false))
            .await
            .expect("seed exec");
        let session_id = seed.session_id.clone();

        let mut handles = Vec::new();
        for _ in 0..2 {
            let manager = manager.clone();
            let session_id = session_id.clone();
            handles.push(tokio::spawn(async move {
                manager
                    .execute(exec_req(&session_id, "block-command", false))
                    .await
            }));
        }

        // Both commands must be genuinely inside the backend at the same time.
        wait_until("two concurrent commands in flight", || {
            let backend = backend.clone();
            async move { backend.started_count().await >= 2 }
        })
        .await;

        let err = manager
            .execute(exec_req(&session_id, "overflow", false))
            .await
            .expect_err("third command must exceed the session limit");
        assert!(matches!(
            &err,
            TerminalOperationError::Terminal(terminal)
                if terminal.code == TERMINAL_EXEC_CODE_SESSION_BUSY
        ));

        backend.gate.open();
        for handle in handles {
            handle.await.expect("join").expect("concurrent exec");
        }

        manager.close().await;
    }

    #[tokio::test]
    async fn default_limit_keeps_session_serial() {
        let backend = ConcurrentExecBackend::new();
        // Default limit of 1 must behave exactly as before this feature.
        let manager = manager_with_limit(backend.clone(), 1);

        let seed = manager
            .execute(exec_req("", "seed", false))
            .await
            .expect("seed exec");
        let session_id = seed.session_id.clone();

        let blocking = {
            let manager = manager.clone();
            let session_id = session_id.clone();
            tokio::spawn(async move {
                manager
                    .execute(exec_req(&session_id, "block-command", false))
                    .await
            })
        };

        wait_until("blocking command in flight", || {
            let backend = backend.clone();
            async move { backend.started_count().await >= 1 }
        })
        .await;

        let err = manager
            .execute(exec_req(&session_id, "second", false))
            .await
            .expect_err("second command must be rejected at the default limit");
        assert!(matches!(
            &err,
            TerminalOperationError::Terminal(terminal)
                if terminal.code == TERMINAL_EXEC_CODE_SESSION_BUSY
        ));

        backend.gate.open();
        blocking.await.expect("join").expect("exec");

        manager.close().await;
    }

    #[tokio::test]
    async fn readiness_gate_never_execs_against_empty_box_id() {
        let backend = GatedCreateBackend::new(Ok("box-gated".to_owned()));
        let manager = manager_with_limit(backend.clone(), 2);

        let creator = {
            let manager = manager.clone();
            tokio::spawn(async move {
                manager
                    .execute(exec_req("sess-gate", "creator", true))
                    .await
            })
        };

        // Only race the creator once it is inside box creation.
        backend.create_started.wait().await;

        let waiter = {
            let manager = manager.clone();
            tokio::spawn(
                async move { manager.execute(exec_req("sess-gate", "waiter", true)).await },
            )
        };

        // Give the waiter a chance to run against the not-yet-created box.
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(
            backend.exec_box_ids.lock().await.is_empty(),
            "a command ran before the box existed"
        );

        backend.create_gate.open();
        creator.await.expect("join").expect("creator exec");
        waiter.await.expect("join").expect("waiter exec");

        let box_ids = backend.exec_box_ids.lock().await.clone();
        assert_eq!(box_ids.len(), 2);
        for box_id in box_ids {
            assert_eq!(box_id, "box-gated", "command ran against a wrong box id");
        }

        manager.close().await;
    }

    #[tokio::test]
    async fn create_failure_reaches_all_waiters() {
        let backend = GatedCreateBackend::new(Err("create boom".to_owned()));
        let manager = manager_with_limit(backend.clone(), 3);

        let creator = {
            let manager = manager.clone();
            tokio::spawn(async move {
                manager
                    .execute(exec_req("sess-fail", "creator", true))
                    .await
            })
        };
        backend.create_started.wait().await;

        let waiter = {
            let manager = manager.clone();
            tokio::spawn(
                async move { manager.execute(exec_req("sess-fail", "waiter", true)).await },
            )
        };
        tokio::time::sleep(Duration::from_millis(100)).await;
        backend.create_gate.open();

        for result in [creator.await.expect("join"), waiter.await.expect("join")] {
            let err = result.expect_err("creation failure must propagate");
            assert!(
                matches!(&err, TerminalOperationError::ExecutionFailed(message) if message.contains("create boom")),
                "unexpected error: {err:?}"
            );
        }

        assert!(backend.exec_box_ids.lock().await.is_empty());
        assert!(
            manager.get_test_session("sess-fail").await.is_none(),
            "failed session must be cleaned up"
        );

        manager.close().await;
    }

    #[tokio::test]
    async fn deferred_destroy_keeps_sibling_alive() {
        let backend = SiblingBackend::new();
        let manager = manager_with_limit(backend.clone(), 2);

        let seed = manager
            .execute(exec_req("", "seed", false))
            .await
            .expect("seed exec");
        let session_id = seed.session_id.clone();

        let sibling = {
            let manager = manager.clone();
            let session_id = session_id.clone();
            tokio::spawn(async move {
                manager
                    .execute(exec_req(&session_id, "sibling", false))
                    .await
            })
        };
        backend.sibling_started.wait().await;

        // This command times out and asks for the session to be destroyed.
        let err = manager
            .execute(exec_req(&session_id, "doomed", false))
            .await
            .expect_err("doomed command must fail");
        assert!(matches!(err, TerminalOperationError::DeadlineExceeded));

        // The box must survive while the sibling is still running.
        assert!(
            backend.removed.lock().await.is_empty(),
            "box removed while a sibling command was still in flight"
        );

        backend.sibling_gate.open();
        let result = sibling
            .await
            .expect("join")
            .expect("sibling command must survive the sibling timeout");
        assert_eq!(result.stdout, "sibling-ok");

        assert_eq!(
            backend.removed.lock().await.as_slice(),
            &["box-sibling"],
            "box must be reclaimed once inflight drains"
        );
        assert!(manager.get_test_session(&session_id).await.is_none());

        manager.close().await;
    }

    #[tokio::test]
    async fn cleanup_skips_sessions_with_inflight_commands() {
        let backend = Arc::new(BlockingShellBackend {
            started: Notify::new(),
            gate: Notify::new(),
            removed: Mutex::new(Vec::new()),
        });
        let manager = manager_with_limit(backend.clone(), 2);

        let expired = SystemTime::now() - Duration::from_secs(60);
        manager
            .insert_test_session("inflight", "box-inflight", expired, 1)
            .await;
        manager
            .insert_test_session("idle", "box-idle", expired, 0)
            .await;

        manager.cleanup_expired_sessions().await;

        assert!(
            manager.get_test_session("inflight").await.is_some(),
            "session with inflight commands must not be reclaimed"
        );
        assert!(manager.get_test_session("idle").await.is_none());
        assert_eq!(backend.removed.lock().await.as_slice(), &["box-idle"]);

        // Draining the last command makes it reclaimable.
        manager
            .insert_test_session("inflight", "box-inflight", expired, 0)
            .await;
        manager.cleanup_expired_sessions().await;
        assert!(manager.get_test_session("inflight").await.is_none());

        manager.close().await;
    }

    /// terminalExec blocks on a gate; terminalResource probes return immediately.
    struct ExecBlockingResourceFreeBackend {
        exec_started: Gate,
        exec_gate: Gate,
    }

    impl ExecBlockingResourceFreeBackend {
        fn new() -> Arc<Self> {
            Arc::new(Self {
                exec_started: Gate::new(),
                exec_gate: Gate::new(),
            })
        }
    }

    #[async_trait]
    impl TerminalBackend for ExecBlockingResourceFreeBackend {
        async fn create_session_box(&self) -> Result<String, String> {
            Ok("box-mixed".to_owned())
        }

        async fn exec_shell_command(
            &self,
            _box_id: &str,
            _command: &str,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            self.exec_started.open();
            self.exec_gate.wait().await;
            Ok(CollectedExecOutput {
                stdout: String::new(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn exec_resource_probe(
            &self,
            _box_id: &str,
            _action: &str,
            _file_path: &str,
            _max_read_bytes: usize,
            _deadline_unix_ms: i64,
        ) -> Result<CollectedExecOutput, BoxliteCommandError> {
            Ok(CollectedExecOutput {
                stdout: r#"{"mime_type":"text/plain","size_bytes":1}"#.to_owned(),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn copy_out_file(
            &self,
            _box_id: &str,
            _container_src: &str,
            _host_dst: &Path,
            _deadline_unix_ms: i64,
        ) -> Result<(), BoxliteCommandError> {
            unreachable!()
        }

        async fn remove_box(&self, _box_id: &str) {}
    }

    #[tokio::test]
    async fn exec_and_resource_run_concurrently_in_one_session() {
        let backend = ExecBlockingResourceFreeBackend::new();
        let manager = manager_with_limit(backend.clone(), 2);

        let expires = SystemTime::now() + Duration::from_secs(300);
        manager
            .insert_test_session("sess-mixed", "box-mixed", expires, 0)
            .await;

        let exec_handle = {
            let manager = manager.clone();
            tokio::spawn(async move {
                manager
                    .execute(exec_req("sess-mixed", "block-command", false))
                    .await
            })
        };

        // Wait until terminalExec is actually inside the backend.
        backend.exec_started.wait().await;

        // terminalResource must not be blocked by the in-flight terminalExec.
        let resource = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-mixed".to_owned(),
                file_path: "/workspace/a.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_VALIDATE.to_owned(),
                signed_url: String::new(),
                headers: Default::default(),
                deadline_unix_ms: 0,
            })
            .await
            .expect("terminalResource must run alongside terminalExec");
        assert_eq!(resource.session_id, "sess-mixed");

        backend.exec_gate.open();
        exec_handle.await.expect("join").expect("exec");

        manager.close().await;
    }

    #[tokio::test]
    async fn resource_rejected_beyond_session_limit() {
        let backend = ExecBlockingResourceFreeBackend::new();
        // Default limit of 1 keeps terminalExec and terminalResource exclusive.
        let manager = manager_with_limit(backend.clone(), 1);

        let expires = SystemTime::now() + Duration::from_secs(300);
        manager
            .insert_test_session("sess-serial", "box-mixed", expires, 0)
            .await;

        let exec_handle = {
            let manager = manager.clone();
            tokio::spawn(async move {
                manager
                    .execute(exec_req("sess-serial", "block-command", false))
                    .await
            })
        };
        backend.exec_started.wait().await;

        let err = manager
            .resolve_resource(TerminalResourceRequest {
                session_id: "sess-serial".to_owned(),
                file_path: "/workspace/a.txt".to_owned(),
                action: TERMINAL_RESOURCE_ACTION_VALIDATE.to_owned(),
                signed_url: String::new(),
                headers: Default::default(),
                deadline_unix_ms: 0,
            })
            .await
            .expect_err("resource must be rejected at the default limit");
        assert!(matches!(
            &err,
            TerminalOperationError::Terminal(terminal)
                if terminal.code == TERMINAL_EXEC_CODE_SESSION_BUSY
        ));

        backend.exec_gate.open();
        exec_handle.await.expect("join").expect("exec");

        manager.close().await;
    }
}
