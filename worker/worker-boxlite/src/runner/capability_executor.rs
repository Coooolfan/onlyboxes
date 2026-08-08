use std::collections::HashMap;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use crate::boxlite_runtime::{self, BoxliteCommandError, PythonExecRunResult};
use crate::config::Config;
use crate::proto::registryv1::{
    connect_request, CommandDispatch, CommandError, CommandResult, ConnectRequest,
};

use super::terminal_session_manager::{
    shared_terminal_session_manager, TerminalExecRequest, TerminalExecRunResult,
    TerminalOperationError, TerminalResourceRequest, TerminalResourceRunResult,
};
use super::{
    ECHO_CAPABILITY_NAME, PYTHON_EXEC_CAPABILITY_NAME, TERMINAL_EXEC_CAPABILITY_NAME,
    TERMINAL_RESOURCE_CAPABILITY_NAME,
};

#[derive(Debug, Deserialize, Serialize)]
struct EchoPayload {
    message: String,
}

#[derive(Debug, Deserialize)]
struct PythonExecPayload {
    code: String,
}

#[derive(Debug, Serialize)]
struct PythonExecResult {
    output: String,
    stderr: String,
    exit_code: i32,
}

#[derive(Debug, Deserialize)]
struct TerminalExecPayload {
    command: String,
    #[serde(default)]
    session_id: String,
    #[serde(default)]
    create_if_missing: bool,
    #[serde(default)]
    lease_ttl_sec: Option<i32>,
}

#[derive(Debug, Deserialize)]
struct TerminalResourcePayload {
    session_id: String,
    file_path: String,
    #[serde(default)]
    action: String,
    #[serde(default)]
    signed_url: String,
    #[serde(default)]
    headers: HashMap<String, String>,
}

static DEFAULT_CAPABILITY_RUNTIME: DefaultCapabilityRuntime = DefaultCapabilityRuntime;

pub(crate) async fn build_command_result(
    cfg: &Config,
    dispatch: CommandDispatch,
) -> ConnectRequest {
    build_command_result_with_runtime(cfg, dispatch, &DEFAULT_CAPABILITY_RUNTIME).await
}

async fn build_command_result_with_runtime<R>(
    cfg: &Config,
    dispatch: CommandDispatch,
    runtime: &R,
) -> ConnectRequest
where
    R: CapabilityRuntime + Sync,
{
    let command_id = dispatch.command_id.trim().to_owned();
    if command_id.is_empty() {
        return command_error_result("", "invalid_command_id", "command_id is required");
    }

    if dispatch.deadline_unix_ms > 0
        && boxlite_runtime::remaining_until_deadline(dispatch.deadline_unix_ms)
            .map(|duration| duration.is_zero())
            .unwrap_or(false)
    {
        return command_error_result(
            &command_id,
            "deadline_exceeded",
            "command deadline exceeded",
        );
    }

    match dispatch.capability.trim().to_ascii_lowercase().as_str() {
        ECHO_CAPABILITY_NAME => build_echo_result(&command_id, &dispatch.payload_json),
        PYTHON_EXEC_CAPABILITY_NAME => {
            build_python_exec_result(
                cfg,
                &command_id,
                &dispatch.payload_json,
                dispatch.deadline_unix_ms,
                runtime,
            )
            .await
        }
        TERMINAL_EXEC_CAPABILITY_NAME => {
            build_terminal_exec_result(
                cfg,
                &command_id,
                &dispatch.payload_json,
                dispatch.deadline_unix_ms,
                runtime,
            )
            .await
        }
        TERMINAL_RESOURCE_CAPABILITY_NAME => {
            build_terminal_resource_result(
                cfg,
                &command_id,
                &dispatch.payload_json,
                dispatch.deadline_unix_ms,
                runtime,
            )
            .await
        }
        _ => command_error_result(
            &command_id,
            "unsupported_capability",
            &format!("capability {:?} is not supported", dispatch.capability),
        ),
    }
}

async fn build_python_exec_result<R>(
    cfg: &Config,
    command_id: &str,
    payload: &[u8],
    deadline_unix_ms: i64,
    runtime: &R,
) -> ConnectRequest
where
    R: CapabilityRuntime + Sync,
{
    let decoded = match parse_python_exec_payload(payload) {
        Ok(decoded) => decoded,
        Err(error_message) => {
            return command_error_result(command_id, "invalid_payload", error_message);
        }
    };

    let exec_result = match runtime
        .run_python_exec(cfg, &decoded.code, deadline_unix_ms)
        .await
    {
        Ok(exec_result) => exec_result,
        Err(BoxliteCommandError::DeadlineExceeded) => {
            return command_error_result(
                command_id,
                "deadline_exceeded",
                "command deadline exceeded",
            );
        }
        Err(BoxliteCommandError::MissingBox) => {
            return command_error_result(
                command_id,
                "execution_failed",
                "pythonExec execution failed: pythonExec box not found",
            );
        }
        Err(BoxliteCommandError::ExecutionFailed(message)) => {
            return command_error_result(
                command_id,
                "execution_failed",
                &format!("pythonExec execution failed: {message}"),
            );
        }
    };

    encode_python_exec_result(command_id, exec_result)
}

async fn build_terminal_exec_result<R>(
    cfg: &Config,
    command_id: &str,
    payload: &[u8],
    deadline_unix_ms: i64,
    runtime: &R,
) -> ConnectRequest
where
    R: CapabilityRuntime + Sync,
{
    let decoded = match parse_terminal_exec_payload(payload) {
        Ok(decoded) => decoded,
        Err(error_message) => {
            return command_error_result(command_id, "invalid_payload", error_message);
        }
    };

    let exec_result = match runtime
        .run_terminal_exec(
            cfg,
            TerminalExecRequest {
                command: decoded.command,
                session_id: decoded.session_id,
                create_if_missing: decoded.create_if_missing,
                lease_ttl_sec: decoded.lease_ttl_sec,
                deadline_unix_ms,
            },
        )
        .await
    {
        Ok(exec_result) => exec_result,
        Err(TerminalOperationError::DeadlineExceeded) => {
            return command_error_result(
                command_id,
                "deadline_exceeded",
                "command deadline exceeded",
            );
        }
        Err(TerminalOperationError::Terminal(err)) => {
            return command_error_result(command_id, err.code(), &err.to_string());
        }
        Err(TerminalOperationError::ExecutionFailed(message)) => {
            return command_error_result(
                command_id,
                "execution_failed",
                &format!("terminalExec execution failed: {message}"),
            );
        }
    };

    let encoded = match serde_json::to_vec(&exec_result) {
        Ok(encoded) => encoded,
        Err(_) => {
            return command_error_result(
                command_id,
                "encode_failed",
                "failed to encode terminalExec payload",
            )
        }
    };

    connect_request_from_result(CommandResult {
        command_id: command_id.to_owned(),
        error: None,
        payload_json: encoded,
        completed_unix_ms: now_unix_ms(),
    })
}

async fn build_terminal_resource_result<R>(
    cfg: &Config,
    command_id: &str,
    payload: &[u8],
    deadline_unix_ms: i64,
    runtime: &R,
) -> ConnectRequest
where
    R: CapabilityRuntime + Sync,
{
    let decoded = match parse_terminal_resource_payload(payload) {
        Ok(decoded) => decoded,
        Err(error_message) => {
            return command_error_result(command_id, "invalid_payload", error_message);
        }
    };

    let resource_result = match runtime
        .run_terminal_resource(
            cfg,
            TerminalResourceRequest {
                session_id: decoded.session_id,
                file_path: decoded.file_path,
                action: decoded.action,
                signed_url: decoded.signed_url,
                headers: decoded.headers,
                deadline_unix_ms,
            },
        )
        .await
    {
        Ok(exec_result) => exec_result,
        Err(TerminalOperationError::DeadlineExceeded) => {
            return command_error_result(
                command_id,
                "deadline_exceeded",
                "command deadline exceeded",
            );
        }
        Err(TerminalOperationError::Terminal(err)) => {
            return command_error_result(command_id, err.code(), &err.to_string());
        }
        Err(TerminalOperationError::ExecutionFailed(message)) => {
            return command_error_result(
                command_id,
                "execution_failed",
                &format!("terminalResource execution failed: {message}"),
            );
        }
    };

    let encoded = match serde_json::to_vec(&resource_result) {
        Ok(encoded) => encoded,
        Err(_) => {
            return command_error_result(
                command_id,
                "encode_failed",
                "failed to encode terminalResource payload",
            )
        }
    };

    connect_request_from_result(CommandResult {
        command_id: command_id.to_owned(),
        error: None,
        payload_json: encoded,
        completed_unix_ms: now_unix_ms(),
    })
}

fn build_echo_result(command_id: &str, payload: &[u8]) -> ConnectRequest {
    let decoded: EchoPayload = match serde_json::from_slice(payload) {
        Ok(decoded) => decoded,
        Err(_) => {
            return command_error_result(
                command_id,
                "invalid_payload",
                "payload_json is not valid echo payload",
            )
        }
    };
    if decoded.message.trim().is_empty() {
        return command_error_result(command_id, "invalid_payload", "echo payload is required");
    }

    let encoded = match serde_json::to_vec(&decoded) {
        Ok(encoded) => encoded,
        Err(_) => {
            return command_error_result(
                command_id,
                "encode_failed",
                "failed to encode echo payload",
            )
        }
    };

    connect_request_from_result(CommandResult {
        command_id: command_id.to_owned(),
        error: None,
        payload_json: encoded,
        completed_unix_ms: now_unix_ms(),
    })
}

fn encode_python_exec_result(command_id: &str, exec_result: PythonExecRunResult) -> ConnectRequest {
    let encoded = match serde_json::to_vec(&PythonExecResult {
        output: exec_result.output,
        stderr: exec_result.stderr,
        exit_code: exec_result.exit_code,
    }) {
        Ok(encoded) => encoded,
        Err(_) => {
            return command_error_result(
                command_id,
                "encode_failed",
                "failed to encode pythonExec payload",
            )
        }
    };

    connect_request_from_result(CommandResult {
        command_id: command_id.to_owned(),
        error: None,
        payload_json: encoded,
        completed_unix_ms: now_unix_ms(),
    })
}

fn parse_python_exec_payload(payload: &[u8]) -> Result<PythonExecPayload, &'static str> {
    if payload.is_empty() {
        return Err("pythonExec payload is required");
    }
    let decoded: PythonExecPayload = serde_json::from_slice(payload)
        .map_err(|_| "payload_json is not valid pythonExec payload")?;
    if decoded.code.trim().is_empty() {
        return Err("pythonExec code is required");
    }
    Ok(decoded)
}

fn parse_terminal_exec_payload(payload: &[u8]) -> Result<TerminalExecPayload, &'static str> {
    if payload.is_empty() {
        return Err("terminalExec payload is required");
    }
    let decoded: TerminalExecPayload = serde_json::from_slice(payload)
        .map_err(|_| "payload_json is not valid terminalExec payload")?;
    if decoded.command.trim().is_empty() {
        return Err("terminalExec command is required");
    }
    Ok(decoded)
}

fn parse_terminal_resource_payload(
    payload: &[u8],
) -> Result<TerminalResourcePayload, &'static str> {
    if payload.is_empty() {
        return Err("terminalResource payload is required");
    }
    let decoded: TerminalResourcePayload = serde_json::from_slice(payload)
        .map_err(|_| "payload_json is not valid terminalResource payload")?;
    if decoded.session_id.trim().is_empty() || decoded.file_path.trim().is_empty() {
        return Err("terminalResource session_id and file_path are required");
    }
    if decoded.action.trim().eq_ignore_ascii_case("export") && decoded.signed_url.trim().is_empty()
    {
        return Err("terminalResource signed_url is required for export");
    }
    Ok(decoded)
}

fn connect_request_from_result(result: CommandResult) -> ConnectRequest {
    ConnectRequest {
        payload: Some(connect_request::Payload::CommandResult(result)),
    }
}

fn command_error_result(command_id: &str, code: &str, message: &str) -> ConnectRequest {
    connect_request_from_result(CommandResult {
        command_id: command_id.to_owned(),
        error: Some(CommandError {
            code: code.to_owned(),
            message: message.to_owned(),
        }),
        payload_json: Vec::new(),
        completed_unix_ms: now_unix_ms(),
    })
}

pub(crate) fn command_dispatch_summary_for_log(capability: &str, payload: &[u8]) -> String {
    let parse_failed = format!("payload_len={} summary=parse_failed", payload.len());

    match capability.trim().to_ascii_lowercase().as_str() {
        ECHO_CAPABILITY_NAME => {
            let Ok(decoded) = serde_json::from_slice::<EchoPayload>(payload) else {
                return parse_failed;
            };
            if decoded.message.trim().is_empty() {
                return parse_failed;
            }
            format!("message_len={}", decoded.message.len())
        }
        PYTHON_EXEC_CAPABILITY_NAME => {
            let Ok(decoded) = serde_json::from_slice::<PythonExecPayload>(payload) else {
                return parse_failed;
            };
            if decoded.code.trim().is_empty() {
                return parse_failed;
            }
            format!("code_len={}", decoded.code.len())
        }
        TERMINAL_EXEC_CAPABILITY_NAME => {
            let Ok(decoded) = serde_json::from_slice::<TerminalExecPayload>(payload) else {
                return parse_failed;
            };
            if decoded.command.trim().is_empty() {
                return parse_failed;
            }
            let lease_ttl = decoded
                .lease_ttl_sec
                .map(|value| value.to_string())
                .unwrap_or_else(|| "default".to_owned());
            format!(
                "command_len={} session_id_present={} create_if_missing={} lease_ttl_sec={}",
                decoded.command.len(),
                !decoded.session_id.trim().is_empty(),
                decoded.create_if_missing,
                lease_ttl
            )
        }
        TERMINAL_RESOURCE_CAPABILITY_NAME => {
            let Ok(decoded) = serde_json::from_slice::<TerminalResourcePayload>(payload) else {
                return parse_failed;
            };
            if decoded.session_id.trim().is_empty() || decoded.file_path.trim().is_empty() {
                return parse_failed;
            }
            let action = match decoded.action.trim().to_ascii_lowercase().as_str() {
                "" => "default",
                "validate" => "validate",
                "read" => "read",
                "export" => "export",
                _ => "invalid",
            };
            format!(
                "action={} session_id_present=true file_path_len={}",
                action,
                decoded.file_path.len()
            )
        }
        _ => format!(
            "payload_len={} summary=unsupported_capability",
            payload.len()
        ),
    }
}

fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_millis() as i64)
        .unwrap_or_default()
}

struct DefaultCapabilityRuntime;

#[async_trait]
trait CapabilityRuntime {
    async fn run_python_exec(
        &self,
        cfg: &Config,
        code: &str,
        deadline_unix_ms: i64,
    ) -> Result<PythonExecRunResult, BoxliteCommandError>;

    async fn run_terminal_exec(
        &self,
        cfg: &Config,
        req: TerminalExecRequest,
    ) -> Result<TerminalExecRunResult, TerminalOperationError>;

    async fn run_terminal_resource(
        &self,
        cfg: &Config,
        req: TerminalResourceRequest,
    ) -> Result<TerminalResourceRunResult, TerminalOperationError>;
}

#[async_trait]
impl CapabilityRuntime for DefaultCapabilityRuntime {
    async fn run_python_exec(
        &self,
        cfg: &Config,
        code: &str,
        deadline_unix_ms: i64,
    ) -> Result<PythonExecRunResult, BoxliteCommandError> {
        boxlite_runtime::run_python_exec(cfg, code, deadline_unix_ms).await
    }

    async fn run_terminal_exec(
        &self,
        cfg: &Config,
        req: TerminalExecRequest,
    ) -> Result<TerminalExecRunResult, TerminalOperationError> {
        shared_terminal_session_manager(cfg).execute(req).await
    }

    async fn run_terminal_resource(
        &self,
        cfg: &Config,
        req: TerminalResourceRequest,
    ) -> Result<TerminalResourceRunResult, TerminalOperationError> {
        shared_terminal_session_manager(cfg)
            .resolve_resource(req)
            .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    struct FakeCapabilityRuntime;

    struct PythonOnlyCapabilityRuntime {
        result: Result<PythonExecRunResult, BoxliteCommandError>,
    }

    struct TerminalErrorCapabilityRuntime;

    #[async_trait]
    impl CapabilityRuntime for FakeCapabilityRuntime {
        async fn run_python_exec(
            &self,
            _cfg: &Config,
            code: &str,
            _deadline_unix_ms: i64,
        ) -> Result<PythonExecRunResult, BoxliteCommandError> {
            Ok(PythonExecRunResult {
                output: format!("ran:{code}"),
                stderr: String::new(),
                exit_code: 0,
            })
        }

        async fn run_terminal_exec(
            &self,
            _cfg: &Config,
            req: TerminalExecRequest,
        ) -> Result<TerminalExecRunResult, TerminalOperationError> {
            Ok(TerminalExecRunResult {
                session_id: if req.session_id.is_empty() {
                    "sess-1".to_owned()
                } else {
                    req.session_id
                },
                created: true,
                stdout: "hello\n".to_owned(),
                stderr: String::new(),
                exit_code: 0,
                stdout_truncated: false,
                stderr_truncated: false,
                lease_expires_unix_ms: 123456789,
            })
        }

        async fn run_terminal_resource(
            &self,
            _cfg: &Config,
            req: TerminalResourceRequest,
        ) -> Result<TerminalResourceRunResult, TerminalOperationError> {
            if req.file_path == "/with-headers" {
                assert_eq!(
                    req.headers.get("x-amz-acl").map(String::as_str),
                    Some("public-read")
                );
            }
            Ok(TerminalResourceRunResult {
                session_id: req.session_id,
                file_path: req.file_path,
                mime_type: "text/plain".to_owned(),
                size_bytes: 3,
                blob: b"abc".to_vec(),
            })
        }
    }

    #[async_trait]
    impl CapabilityRuntime for TerminalErrorCapabilityRuntime {
        async fn run_python_exec(
            &self,
            _cfg: &Config,
            _code: &str,
            _deadline_unix_ms: i64,
        ) -> Result<PythonExecRunResult, BoxliteCommandError> {
            unreachable!()
        }

        async fn run_terminal_exec(
            &self,
            _cfg: &Config,
            _req: TerminalExecRequest,
        ) -> Result<TerminalExecRunResult, TerminalOperationError> {
            Err(super::super::terminal_session_manager::TerminalExecError::new(
                super::super::terminal_session_manager::TERMINAL_EXEC_CODE_SESSION_CAPACITY_EXCEEDED,
                super::super::terminal_session_manager::TERMINAL_EXEC_CAPACITY_MESSAGE,
            )
            .into())
        }

        async fn run_terminal_resource(
            &self,
            _cfg: &Config,
            _req: TerminalResourceRequest,
        ) -> Result<TerminalResourceRunResult, TerminalOperationError> {
            unreachable!()
        }
    }

    #[async_trait]
    impl CapabilityRuntime for PythonOnlyCapabilityRuntime {
        async fn run_python_exec(
            &self,
            _cfg: &Config,
            _code: &str,
            _deadline_unix_ms: i64,
        ) -> Result<PythonExecRunResult, BoxliteCommandError> {
            self.result.clone()
        }

        async fn run_terminal_exec(
            &self,
            _cfg: &Config,
            _req: TerminalExecRequest,
        ) -> Result<TerminalExecRunResult, TerminalOperationError> {
            unreachable!()
        }

        async fn run_terminal_resource(
            &self,
            _cfg: &Config,
            _req: TerminalResourceRequest,
        ) -> Result<TerminalResourceRunResult, TerminalOperationError> {
            unreachable!()
        }
    }

    fn test_config() -> Config {
        Config {
            config_file: None,
            console_grpc_target: "127.0.0.1:50051".to_owned(),
            console_tls: false,
            worker_id: "worker-12345678".to_owned(),
            worker_secret: "secret".to_owned(),
            heartbeat_interval: Duration::from_secs(5),
            heartbeat_jitter_pct: 20,
            call_timeout: Duration::from_secs(13),
            node_name: String::new(),
            executor_kind: "boxlite".to_owned(),
            labels: Default::default(),
            boxlite_home: String::new(),
            python_exec_image: "ghcr.io/astral-sh/uv:python3.12-bookworm-slim".to_owned(),
            python_exec_memory_mib: 256,
            python_exec_cpus: 1,
            python_exec_max_processes: 128,
            terminal_exec_image: "coolfan1024/onlyboxes-runtime:default".to_owned(),
            terminal_exec_memory_mib: 256,
            terminal_exec_cpus: 1,
            terminal_exec_max_processes: 128,
            terminal_lease_min_sec: 60,
            terminal_lease_max_sec: 1800,
            terminal_lease_default_sec: 60,
            terminal_output_limit_bytes: 1024 * 1024,
            terminal_export_max_bytes: 0,
            terminal_session_max_inflight: 1,
            terminal_max_active_sessions: 0,
            echo_max_inflight: 4,
            python_exec_max_inflight: 4,
            terminal_exec_max_inflight: 4,
            terminal_resource_max_inflight: 4,
            proxy_enabled: false,
            proxy_listen_addr: "0.0.0.0:8091".to_owned(),
            proxy_advertise_addr: String::new(),
            proxy_sandbox_ports: Vec::new(),
            log_level: "info".to_owned(),
            log_format: "json".to_owned(),
            log_add_source: false,
        }
    }

    fn unwrap_command_result(request: ConnectRequest) -> CommandResult {
        let connect_request::Payload::CommandResult(result) = request.payload.unwrap() else {
            panic!("expected command_result payload");
        };
        result
    }

    #[tokio::test]
    async fn build_command_result_round_trips_echo_payload() {
        let request = build_command_result(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-1".to_owned(),
                capability: "echo".to_owned(),
                payload_json: br#"{"message":"hello"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
        )
        .await;

        let result = unwrap_command_result(request);
        assert!(result.error.is_none());
        assert_eq!(result.payload_json, br#"{"message":"hello"}"#.to_vec());
    }

    #[tokio::test]
    async fn build_command_result_rejects_invalid_python_payload() {
        let request = build_command_result(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-2".to_owned(),
                capability: "pythonExec".to_owned(),
                payload_json: br#"{"code":"   "}"#.to_vec(),
                deadline_unix_ms: 0,
            },
        )
        .await;

        let result = unwrap_command_result(request);
        assert_eq!(result.error.unwrap().code, "invalid_payload");
    }

    #[tokio::test]
    async fn build_command_result_encodes_python_exec_success() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-py-1".to_owned(),
                capability: "pythonExec".to_owned(),
                payload_json: br#"{"code":"print('hi')"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &PythonOnlyCapabilityRuntime {
                result: Ok(PythonExecRunResult {
                    output: "hi\n".to_owned(),
                    stderr: "warn\n".to_owned(),
                    exit_code: 0,
                }),
            },
        )
        .await;

        let result = unwrap_command_result(request);
        assert!(result.error.is_none());
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&result.payload_json).unwrap(),
            serde_json::json!({
                "output": "hi\n",
                "stderr": "warn\n",
                "exit_code": 0
            })
        );
    }

    #[tokio::test]
    async fn build_command_result_keeps_python_exec_non_zero_exit_in_payload() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-py-2".to_owned(),
                capability: "pythonExec".to_owned(),
                payload_json: br#"{"code":"raise SystemExit(2)"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &PythonOnlyCapabilityRuntime {
                result: Ok(PythonExecRunResult {
                    output: String::new(),
                    stderr: "boom\n".to_owned(),
                    exit_code: 2,
                }),
            },
        )
        .await;

        let result = unwrap_command_result(request);
        assert!(result.error.is_none());
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&result.payload_json).unwrap(),
            serde_json::json!({
                "output": "",
                "stderr": "boom\n",
                "exit_code": 2
            })
        );
    }

    #[tokio::test]
    async fn build_command_result_maps_python_exec_deadline_exceeded() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-py-3".to_owned(),
                capability: "pythonExec".to_owned(),
                payload_json: br#"{"code":"while True: pass"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &PythonOnlyCapabilityRuntime {
                result: Err(BoxliteCommandError::DeadlineExceeded),
            },
        )
        .await;

        let result = unwrap_command_result(request);
        let error = result.error.expect("expected deadline error");
        assert_eq!(error.code, "deadline_exceeded");
        assert_eq!(error.message, "command deadline exceeded");
    }

    #[tokio::test]
    async fn build_command_result_maps_python_exec_missing_box_to_execution_failed() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-py-4".to_owned(),
                capability: "pythonExec".to_owned(),
                payload_json: br#"{"code":"print('hi')"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &PythonOnlyCapabilityRuntime {
                result: Err(BoxliteCommandError::MissingBox),
            },
        )
        .await;

        let result = unwrap_command_result(request);
        let error = result.error.expect("expected execution_failed");
        assert_eq!(error.code, "execution_failed");
        assert_eq!(
            error.message,
            "pythonExec execution failed: pythonExec box not found"
        );
    }

    #[tokio::test]
    async fn build_command_result_maps_python_exec_execution_failed() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-py-5".to_owned(),
                capability: "pythonExec".to_owned(),
                payload_json: br#"{"code":"print('hi')"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &PythonOnlyCapabilityRuntime {
                result: Err(BoxliteCommandError::ExecutionFailed(
                    "python crashed".to_owned(),
                )),
            },
        )
        .await;

        let result = unwrap_command_result(request);
        let error = result.error.expect("expected execution_failed");
        assert_eq!(error.code, "execution_failed");
        assert_eq!(error.message, "pythonExec execution failed: python crashed");
    }

    #[tokio::test]
    async fn build_command_result_encodes_terminal_exec_result() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-term-1".to_owned(),
                capability: "terminalExec".to_owned(),
                payload_json: br#"{"command":"echo hello"}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &FakeCapabilityRuntime,
        )
        .await;

        let result = unwrap_command_result(request);
        assert!(result.error.is_none());
        let decoded: TerminalExecRunResult = serde_json::from_slice(&result.payload_json).unwrap();
        assert_eq!(decoded.session_id, "sess-1");
        assert_eq!(decoded.stdout, "hello\n");
    }

    #[tokio::test]
    async fn build_command_result_preserves_terminal_capacity_error() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-term-capacity".to_owned(),
                capability: "terminalExec".to_owned(),
                payload_json: br#"{"command":"pwd","session_id":"new","create_if_missing":true}"#
                    .to_vec(),
                deadline_unix_ms: 0,
            },
            &TerminalErrorCapabilityRuntime,
        )
        .await;

        let error = unwrap_command_result(request)
            .error
            .expect("expected capacity error");
        assert_eq!(error.code, "session_capacity_exceeded");
        assert_eq!(error.message, "terminal session capacity exceeded");
    }

    #[tokio::test]
    async fn build_command_result_encodes_terminal_resource_result() {
        let request =
            build_command_result_with_runtime(
                &test_config(),
                CommandDispatch {
                    command_id: "cmd-term-res-1".to_owned(),
                    capability: "terminalResource".to_owned(),
                    payload_json:
                        br#"{"session_id":"sess-1","file_path":"app/main.py","action":"read"}"#
                            .to_vec(),
                    deadline_unix_ms: 0,
                },
                &FakeCapabilityRuntime,
            )
            .await;

        let result = unwrap_command_result(request);
        assert!(result.error.is_none());
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&result.payload_json).unwrap(),
            serde_json::json!({
                "session_id": "sess-1",
                "file_path": "app/main.py",
                "mime_type": "text/plain",
                "size_bytes": 3,
                "blob": "YWJj"
            })
        );
        let decoded: TerminalResourceRunResult =
            serde_json::from_slice(&result.payload_json).unwrap();
        assert_eq!(decoded.session_id, "sess-1");
        assert_eq!(decoded.file_path, "app/main.py");
        assert_eq!(decoded.blob, b"abc");
    }

    #[tokio::test]
    async fn build_command_result_forwards_terminal_resource_headers() {
        let request = build_command_result_with_runtime(
            &test_config(),
            CommandDispatch {
                command_id: "cmd-term-res-headers".to_owned(),
                capability: "terminalResource".to_owned(),
                payload_json: br#"{"session_id":"sess-1","file_path":"/with-headers","action":"export","signed_url":"https://uploads.example.com/put","headers":{"x-amz-acl":"public-read"}}"#.to_vec(),
                deadline_unix_ms: 0,
            },
            &FakeCapabilityRuntime,
        )
        .await;

        let result = unwrap_command_result(request);
        assert!(result.error.is_none());
    }

    #[test]
    fn command_dispatch_summary_matches_terminal_exec_shape() {
        let summary = command_dispatch_summary_for_log(
            "terminalExec",
            br#"{"command":"pwd","session_id":"sess-1","create_if_missing":true,"lease_ttl_sec":90}"#,
        );
        assert_eq!(
            summary,
            "command_len=3 session_id_present=true create_if_missing=true lease_ttl_sec=90"
        );
    }
}
