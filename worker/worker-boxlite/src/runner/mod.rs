mod capability_executor;
mod hello_builder;
mod session_client;
pub(crate) mod terminal_session_manager;

use std::time::Duration;

use tokio_util::sync::CancellationToken;
use tonic::Code;

use crate::config::Config;

pub(crate) const MIN_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(1);
pub(crate) const INITIAL_RECONNECT_DELAY: Duration = Duration::from_secs(1);
pub(crate) const MAX_RECONNECT_DELAY: Duration = Duration::from_secs(15);
pub(crate) const ECHO_CAPABILITY_NAME: &str = "echo";
pub(crate) const PYTHON_EXEC_CAPABILITY_NAME: &str = "pythonexec";
pub(crate) const PYTHON_EXEC_CAPABILITY_DECLARED: &str = "pythonExec";
pub(crate) const TERMINAL_EXEC_CAPABILITY_NAME: &str = "terminalexec";
pub(crate) const TERMINAL_EXEC_CAPABILITY_DECLARED: &str = "terminalExec";
pub(crate) const TERMINAL_RESOURCE_CAPABILITY_NAME: &str = "terminalresource";
pub(crate) const TERMINAL_RESOURCE_CAPABILITY_DECLARED: &str = "terminalResource";

pub(crate) use capability_executor::build_command_result;
pub(crate) use capability_executor::command_dispatch_summary_for_log;
pub(crate) use hello_builder::build_hello;
pub(crate) use session_client::run_session;
pub(crate) use terminal_session_manager::shutdown_shared_terminal_sessions;

#[derive(Debug, thiserror::Error)]
pub enum RunnerError {
    #[error("{0}")]
    Message(String),
    #[error(transparent)]
    Status(#[from] tonic::Status),
    #[error(transparent)]
    Transport(#[from] tonic::transport::Error),
    #[error("worker cancelled")]
    Cancelled,
}

impl RunnerError {
    fn grpc_code(&self) -> Option<Code> {
        match self {
            Self::Status(status) => Some(status.code()),
            _ => None,
        }
    }
}

pub async fn run(shutdown: CancellationToken, cfg: Config) -> Result<(), RunnerError> {
    if cfg.worker_id.trim().is_empty() {
        return Err(RunnerError::Message("WORKER_ID is required".to_owned()));
    }
    if cfg.worker_secret.trim().is_empty() {
        return Err(RunnerError::Message("WORKER_SECRET is required".to_owned()));
    }

    tracing::info!(
        image = %cfg.python_exec_image,
        memory_mib = cfg.python_exec_memory_mib,
        cpus = cfg.python_exec_cpus,
        max_processes = cfg.python_exec_max_processes,
        "pythonExec configured"
    );
    tracing::info!(
        image = %cfg.terminal_exec_image,
        memory_mib = cfg.terminal_exec_memory_mib,
        cpus = cfg.terminal_exec_cpus,
        max_processes = cfg.terminal_exec_max_processes,
        lease_min_sec = cfg.terminal_lease_min_sec,
        lease_max_sec = cfg.terminal_lease_max_sec,
        lease_default_sec = cfg.terminal_lease_default_sec,
        output_limit_bytes = cfg.terminal_output_limit_bytes,
        "terminalExec configured"
    );

    let mut reconnect_delay = INITIAL_RECONNECT_DELAY;
    loop {
        if shutdown.is_cancelled() {
            return Err(RunnerError::Cancelled);
        }

        match run_session(shutdown.clone(), &cfg).await {
            Ok(()) => return Ok(()),
            Err(RunnerError::Cancelled) => return Err(RunnerError::Cancelled),
            Err(err) => {
                if err.grpc_code() == Some(Code::FailedPrecondition) {
                    tracing::warn!(
                        node_id = %cfg.worker_id,
                        "registry session replaced, reconnecting"
                    );
                    reconnect_delay = INITIAL_RECONNECT_DELAY;
                } else {
                    tracing::warn!(error = %err, "registry session interrupted");
                }

                wait_reconnect_delay(shutdown.clone(), reconnect_delay).await?;
                reconnect_delay = next_reconnect_delay(reconnect_delay);
            }
        }
    }
}

pub async fn shutdown() {
    shutdown_shared_terminal_sessions().await;
}

pub(crate) fn next_reconnect_delay(current: Duration) -> Duration {
    if current.is_zero() {
        return INITIAL_RECONNECT_DELAY;
    }
    (current * 2).min(MAX_RECONNECT_DELAY)
}

async fn wait_reconnect_delay(
    shutdown: CancellationToken,
    delay: Duration,
) -> Result<(), RunnerError> {
    let wait_for = if delay.is_zero() {
        INITIAL_RECONNECT_DELAY
    } else {
        delay
    };

    tokio::select! {
        _ = shutdown.cancelled() => Err(RunnerError::Cancelled),
        _ = tokio::time::sleep(wait_for) => Ok(()),
    }
}

pub(crate) fn duration_from_server(seconds: i32, fallback: Duration) -> Duration {
    if seconds > 0 {
        return Duration::from_secs(seconds as u64);
    }
    if fallback >= MIN_HEARTBEAT_INTERVAL {
        return fallback;
    }
    MIN_HEARTBEAT_INTERVAL
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn next_reconnect_delay_caps_at_maximum() {
        assert_eq!(
            next_reconnect_delay(Duration::from_secs(0)),
            INITIAL_RECONNECT_DELAY
        );
        assert_eq!(
            next_reconnect_delay(Duration::from_secs(10)),
            MAX_RECONNECT_DELAY
        );
    }

    #[test]
    fn duration_from_server_uses_fallback_when_invalid() {
        assert_eq!(
            duration_from_server(0, Duration::from_secs(7)),
            Duration::from_secs(7)
        );
        assert_eq!(
            duration_from_server(0, Duration::from_millis(500)),
            MIN_HEARTBEAT_INTERVAL
        );
    }
}
