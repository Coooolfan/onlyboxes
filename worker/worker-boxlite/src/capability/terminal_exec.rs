use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use boxlite::{BoxCommand, BoxOptions, BoxliteRuntime, LiteBox, RootfsSpec};
use futures::StreamExt;
use serde::{Deserialize, Serialize};
use tokio::sync::{watch, Mutex};

use super::{now_unix_ms, CapabilityError};
use crate::config::Config;

const JANITOR_INTERVAL: Duration = Duration::from_secs(5);

// ---------- Request / Response ----------

#[derive(Deserialize)]
pub struct TerminalExecRequest {
    pub command: String,
    #[serde(default)]
    pub session_id: Option<String>,
    #[serde(default)]
    pub create_if_missing: bool,
    #[serde(default)]
    pub lease_ttl_sec: Option<u64>,
}

#[derive(Serialize)]
pub struct TerminalExecResponse {
    pub session_id: String,
    pub created: bool,
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
    pub stdout_truncated: bool,
    pub stderr_truncated: bool,
    pub lease_expires_unix_ms: i64,
}

// ---------- Session ----------

pub struct BoxSession {
    pub id: String,
    pub vm_name: String,
    pub litebox: LiteBox,
    pub busy: AtomicBool,
    pub lease_expiry: Mutex<Instant>,
    pub created_at: Instant,
}

// ---------- SessionManager ----------

pub struct SessionManager {
    sessions: Arc<Mutex<HashMap<String, Arc<BoxSession>>>>,
    runtime: Arc<BoxliteRuntime>,
    config: Config,
}

impl SessionManager {
    pub fn new(
        runtime: Arc<BoxliteRuntime>,
        config: Config,
        shutdown: watch::Receiver<bool>,
    ) -> Arc<Self> {
        let mgr = Arc::new(Self {
            sessions: Arc::new(Mutex::new(HashMap::new())),
            runtime,
            config,
        });

        // Spawn janitor
        let janitor_mgr = mgr.clone();
        tokio::spawn(async move {
            janitor_mgr.janitor_loop(shutdown).await;
        });

        mgr
    }

    pub async fn handle_terminal_exec(
        &self,
        payload: &[u8],
        deadline: Option<Instant>,
    ) -> Result<Vec<u8>, CapabilityError> {
        let req: TerminalExecRequest = serde_json::from_slice(payload)
            .map_err(|e| CapabilityError::new("invalid_payload", format!("bad JSON: {e}")))?;

        if req.command.is_empty() {
            return Err(CapabilityError::new(
                "invalid_payload",
                "command is required",
            ));
        }

        // Validate lease_ttl_sec
        let lease_ttl = req
            .lease_ttl_sec
            .unwrap_or(self.config.terminal_lease_default_sec);
        if lease_ttl < self.config.terminal_lease_min_sec
            || lease_ttl > self.config.terminal_lease_max_sec
        {
            return Err(CapabilityError::new(
                "invalid_payload",
                format!(
                    "lease_ttl_sec must be in [{}, {}]",
                    self.config.terminal_lease_min_sec, self.config.terminal_lease_max_sec
                ),
            ));
        }

        tracing::debug!(
            command_len = req.command.len(),
            session_id_present = req.session_id.is_some(),
            create_if_missing = req.create_if_missing,
            lease_ttl_sec = lease_ttl,
            "terminalExec"
        );

        // Get or create session
        let (session, created) = self
            .get_or_create(req.session_id.as_deref(), req.create_if_missing)
            .await?;

        // Acquire busy lock (CAS)
        if session
            .busy
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .is_err()
        {
            return Err(CapabilityError::new(
                "session_busy",
                "session is executing another command",
            ));
        }

        // Release busy on all exit paths
        let session_for_guard = session.clone();
        let _busy_guard = scopeguard::guard((), move |_| {
            session_for_guard.busy.store(false, Ordering::Release);
        });

        // Extend lease (monotonic)
        {
            let new_expiry = Instant::now() + Duration::from_secs(lease_ttl);
            let mut expiry = session.lease_expiry.lock().await;
            if new_expiry > *expiry {
                *expiry = new_expiry;
            }
        }

        // Execute command
        let exec_fut = async {
            let cmd = BoxCommand::new("sh").args(["-lc", &req.command]);
            let mut execution = session
                .litebox
                .exec(cmd)
                .await
                .map_err(|e| CapabilityError::new("execution_failed", format!("exec: {e}")))?;

            let stdout = collect_stream_truncated(
                execution.stdout(),
                self.config.terminal_output_limit_bytes,
            )
            .await;
            let stderr = collect_stream_truncated(
                execution.stderr(),
                self.config.terminal_output_limit_bytes,
            )
            .await;

            let result = execution
                .wait()
                .await
                .map_err(|e| CapabilityError::new("execution_failed", format!("wait: {e}")))?;

            Ok::<_, CapabilityError>((stdout, stderr, result.exit_code))
        };

        let (stdout_result, stderr_result, exit_code) = match deadline {
            Some(dl) => {
                let timeout = dl.saturating_duration_since(Instant::now());
                match tokio::time::timeout(timeout, exec_fut).await {
                    Ok(result) => result?,
                    Err(_) => {
                        // Timeout: destroy session
                        self.destroy_session(&session.id).await;
                        return Err(CapabilityError::new(
                            "deadline_exceeded",
                            "execution timed out",
                        ));
                    }
                }
            }
            None => exec_fut.await?,
        };

        let lease_expires = {
            let expiry = session.lease_expiry.lock().await;
            let remaining = expiry.saturating_duration_since(Instant::now());
            now_unix_ms() + remaining.as_millis() as i64
        };

        let resp = TerminalExecResponse {
            session_id: session.id.clone(),
            created,
            stdout: stdout_result.content,
            stderr: stderr_result.content,
            exit_code,
            stdout_truncated: stdout_result.truncated,
            stderr_truncated: stderr_result.truncated,
            lease_expires_unix_ms: lease_expires,
        };

        serde_json::to_vec(&resp)
            .map_err(|e| CapabilityError::new("encode_failed", format!("JSON encode: {e}")))
    }

    async fn get_or_create(
        &self,
        session_id: Option<&str>,
        create_if_missing: bool,
    ) -> Result<(Arc<BoxSession>, bool), CapabilityError> {
        match session_id {
            None => {
                // No session_id provided: generate new UUID
                let session = self.create_session(None).await?;
                Ok((session, true))
            }
            Some(id) => {
                let sessions = self.sessions.lock().await;
                if let Some(session) = sessions.get(id) {
                    return Ok((session.clone(), false));
                }
                drop(sessions);

                if !create_if_missing {
                    return Err(CapabilityError::new(
                        "session_not_found",
                        format!("session {id} not found"),
                    ));
                }

                // create_if_missing: reuse the provided session_id
                let session = self.create_session(Some(id.to_string())).await?;
                Ok((session, true))
            }
        }
    }

    async fn create_session(
        &self,
        session_id: Option<String>,
    ) -> Result<Arc<BoxSession>, CapabilityError> {
        let session_id = session_id.unwrap_or_else(|| uuid::Uuid::new_v4().to_string());
        let vm_name = format!("obx-terminalexec-{}", random_hex(8));

        let litebox = self
            .runtime
            .create(
                BoxOptions {
                    cpus: Some(self.config.boxlite_default_cpus),
                    memory_mib: Some(self.config.boxlite_default_memory_mib),
                    rootfs: RootfsSpec::Image(self.config.terminal_exec_image.clone()),
                    auto_remove: false, // managed by session lifecycle
                    ..Default::default()
                },
                Some(vm_name.clone()),
            )
            .await
            .map_err(|e| CapabilityError::new("execution_failed", format!("create VM: {e}")))?;

        litebox
            .start()
            .await
            .map_err(|e| CapabilityError::new("execution_failed", format!("start VM: {e}")))?;

        let session = Arc::new(BoxSession {
            id: session_id.clone(),
            vm_name,
            litebox,
            busy: AtomicBool::new(false),
            lease_expiry: Mutex::new(
                Instant::now() + Duration::from_secs(self.config.terminal_lease_default_sec),
            ),
            created_at: Instant::now(),
        });

        self.sessions
            .lock()
            .await
            .insert(session_id, session.clone());
        Ok(session)
    }

    pub async fn get_session(&self, session_id: &str) -> Option<Arc<BoxSession>> {
        self.sessions.lock().await.get(session_id).cloned()
    }

    async fn destroy_session(&self, session_id: &str) {
        if let Some(session) = self.sessions.lock().await.remove(session_id) {
            if let Err(e) = self.runtime.remove(&session.vm_name, true).await {
                tracing::warn!(
                    session_id = %session_id,
                    vm = %session.vm_name,
                    error = %e,
                    "failed to remove session VM"
                );
            }
        }
    }

    async fn janitor_loop(&self, mut shutdown: watch::Receiver<bool>) {
        let mut interval = tokio::time::interval(JANITOR_INTERVAL);

        loop {
            tokio::select! {
                _ = interval.tick() => {
                    let mut sessions = self.sessions.lock().await;
                    let now = Instant::now();
                    let mut expired = Vec::new();

                    for (id, session) in sessions.iter() {
                        if !session.busy.load(Ordering::Acquire) {
                            let expiry = session.lease_expiry.lock().await;
                            if *expiry < now {
                                expired.push(id.clone());
                            }
                        }
                    }

                    for id in &expired {
                        if let Some(session) = sessions.remove(id) {
                            tracing::info!(
                                session_id = %id,
                                vm = %session.vm_name,
                                "janitor: cleaning expired session"
                            );
                            let rt = self.runtime.clone();
                            let vm = session.vm_name.clone();
                            tokio::spawn(async move {
                                if let Err(e) = rt.remove(&vm, true).await {
                                    tracing::warn!(vm = %vm, error = %e, "janitor cleanup failed");
                                }
                            });
                        }
                    }
                }
                _ = shutdown.changed() => {
                    tracing::info!("janitor: shutdown signal received");
                    break;
                }
            }
        }
    }

    pub async fn shutdown_all(&self) {
        let mut sessions = self.sessions.lock().await;
        for (id, session) in sessions.drain() {
            tracing::info!(session_id = %id, vm = %session.vm_name, "shutdown: cleaning session");
            if let Err(e) = self.runtime.remove(&session.vm_name, true).await {
                tracing::warn!(vm = %session.vm_name, error = %e, "shutdown cleanup failed");
            }
        }
    }
}

// ---------- Helpers ----------

struct TruncatedOutput {
    content: String,
    truncated: bool,
}

async fn collect_stream_truncated(
    stream: Option<impl futures::Stream<Item = String> + Unpin>,
    limit: usize,
) -> TruncatedOutput {
    match stream {
        Some(mut s) => {
            let mut buf = String::new();
            let mut truncated = false;
            while let Some(chunk) = s.next().await {
                buf.push_str(&chunk);
                if buf.len() > limit {
                    buf.truncate(limit);
                    truncated = true;
                    break;
                }
            }
            TruncatedOutput {
                content: buf,
                truncated,
            }
        }
        None => TruncatedOutput {
            content: String::new(),
            truncated: false,
        },
    }
}

fn random_hex(len: usize) -> String {
    use rand::Rng;
    let mut rng = rand::thread_rng();
    (0..len)
        .map(|_| format!("{:02x}", rng.gen::<u8>()))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_random_hex_length() {
        let hex = random_hex(8);
        assert_eq!(hex.len(), 16); // 8 bytes = 16 hex chars
        assert!(hex.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn test_random_hex_uniqueness() {
        let a = random_hex(8);
        let b = random_hex(8);
        assert_ne!(a, b);
    }

    #[tokio::test]
    async fn test_collect_stream_truncated_within_limit() {
        let stream = futures::stream::iter(vec!["hello".to_string(), " world".to_string()]);
        let result = collect_stream_truncated(Some(stream), 1024).await;
        assert_eq!(result.content, "hello world");
        assert!(!result.truncated);
    }

    #[tokio::test]
    async fn test_collect_stream_truncated_exceeds_limit() {
        let stream = futures::stream::iter(vec!["aaaaaaaaaa".to_string()]);
        let result = collect_stream_truncated(Some(stream), 5).await;
        assert_eq!(result.content.len(), 5);
        assert!(result.truncated);
    }

    #[tokio::test]
    async fn test_collect_stream_truncated_none() {
        let result =
            collect_stream_truncated(None::<futures::stream::Empty<String>>, 1024).await;
        assert_eq!(result.content, "");
        assert!(!result.truncated);
    }

    #[tokio::test]
    async fn test_terminal_exec_empty_command() {
        let payload = br#"{"command":""}"#;
        let req: TerminalExecRequest = serde_json::from_slice(payload).unwrap();
        assert!(req.command.is_empty());
    }

    #[test]
    fn test_terminal_exec_request_defaults() {
        let req: TerminalExecRequest =
            serde_json::from_str(r#"{"command":"ls"}"#).unwrap();
        assert_eq!(req.command, "ls");
        assert!(req.session_id.is_none());
        assert!(!req.create_if_missing);
        assert!(req.lease_ttl_sec.is_none());
    }
}
