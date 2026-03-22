pub mod echo;
pub mod python_exec;
pub mod terminal_exec;
pub mod terminal_resource;

use std::sync::Arc;
use std::time::Instant;

use boxlite::BoxliteRuntime;
use tokio::sync::watch;

use crate::config::Config;
use crate::proto;

#[derive(Debug)]
pub struct CapabilityError {
    pub code: String,
    pub message: String,
}

impl CapabilityError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
        }
    }
}

impl std::fmt::Display for CapabilityError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for CapabilityError {}

pub struct CapabilityExecutor {
    runtime: Arc<BoxliteRuntime>,
    session_mgr: Arc<terminal_exec::SessionManager>,
    config: Config,
}

impl CapabilityExecutor {
    pub fn new(
        runtime: Arc<BoxliteRuntime>,
        config: Config,
        shutdown: watch::Receiver<bool>,
    ) -> Self {
        let session_mgr =
            terminal_exec::SessionManager::new(runtime.clone(), config.clone(), shutdown);
        Self {
            runtime,
            session_mgr,
            config,
        }
    }

    pub async fn execute(&self, dispatch: proto::CommandDispatch) -> proto::CommandResult {
        let now_ms = now_unix_ms();

        if dispatch.command_id.is_empty() {
            return proto::CommandResult {
                command_id: String::new(),
                error: Some(proto::CommandError {
                    code: "invalid_command_id".into(),
                    message: "command_id is empty".into(),
                }),
                payload_json: vec![],
                completed_unix_ms: now_ms,
            };
        }

        if dispatch.deadline_unix_ms > 0 && dispatch.deadline_unix_ms < now_ms {
            return proto::CommandResult {
                command_id: dispatch.command_id,
                error: Some(proto::CommandError {
                    code: "deadline_exceeded".into(),
                    message: "command deadline already passed".into(),
                }),
                payload_json: vec![],
                completed_unix_ms: now_ms,
            };
        }

        let deadline = if dispatch.deadline_unix_ms > 0 {
            let remaining_ms = (dispatch.deadline_unix_ms - now_ms) as u64;
            Some(Instant::now() + std::time::Duration::from_millis(remaining_ms))
        } else {
            None
        };

        let result = match dispatch.capability.to_lowercase().as_str() {
            "echo" => echo::handle_echo(&dispatch.payload_json).await,
            "pythonexec" => {
                python_exec::handle_python_exec(
                    &self.runtime,
                    &self.config,
                    &dispatch.payload_json,
                    deadline,
                )
                .await
            }
            "terminalexec" => {
                self.session_mgr
                    .handle_terminal_exec(&dispatch.payload_json, deadline)
                    .await
            }
            "terminalresource" => {
                terminal_resource::handle_terminal_resource(
                    &self.session_mgr,
                    &self.config,
                    &dispatch.payload_json,
                    deadline,
                )
                .await
            }
            _ => Err(CapabilityError::new(
                "unsupported_capability",
                format!("unknown capability: {}", dispatch.capability),
            )),
        };

        let now_ms = now_unix_ms();

        match result {
            Ok(payload) => proto::CommandResult {
                command_id: dispatch.command_id,
                error: None,
                payload_json: payload,
                completed_unix_ms: now_ms,
            },
            Err(e) => proto::CommandResult {
                command_id: dispatch.command_id,
                error: Some(proto::CommandError {
                    code: e.code,
                    message: e.message,
                }),
                payload_json: vec![],
                completed_unix_ms: now_ms,
            },
        }
    }

    pub async fn shutdown(&self) {
        self.session_mgr.shutdown_all().await;
    }
}

pub fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_capability_error_display() {
        let err = CapabilityError::new("test_code", "test message");
        assert_eq!(format!("{err}"), "test_code: test message");
    }

    #[test]
    fn test_now_unix_ms_reasonable() {
        let ms = now_unix_ms();
        // Should be after 2024-01-01
        assert!(ms > 1_704_067_200_000);
    }

    #[tokio::test]
    async fn test_echo_via_handler() {
        let result = echo::handle_echo(br#"{"message":"hello"}"#).await.unwrap();
        let resp: serde_json::Value = serde_json::from_slice(&result).unwrap();
        assert_eq!(resp["message"], "hello");
    }

    #[tokio::test]
    async fn test_echo_invalid_payload_via_handler() {
        let err = echo::handle_echo(b"bad").await.unwrap_err();
        assert_eq!(err.code, "invalid_payload");
    }
}
