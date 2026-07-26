use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use boxlite::{
    BoxCommand, BoxOptions, BoxliteOptions, BoxliteRuntime, CopyOptions, ExecStderr, ExecStdout,
    LiteBox, RootfsSpec,
};
use tokio::task::JoinHandle;
use tokio_stream::StreamExt;

use crate::config::Config;

const BOX_CLEANUP_TIMEOUT: Duration = Duration::from_secs(3);
const TERMINAL_EXEC_IDLE_COMMAND: &str = "while true; do sleep 3600; done";

static RUNTIME: OnceLock<Result<BoxliteRuntime, String>> = OnceLock::new();
// Keep terminal session handles alive so the first exec can reuse the
// already-connected guest session instead of immediately reattaching by box_id.
static TERMINAL_SESSION_BOXES: OnceLock<Mutex<HashMap<String, Arc<LiteBox>>>> = OnceLock::new();

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct PythonExecRunResult {
    pub output: String,
    pub stderr: String,
    pub exit_code: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct CollectedExecOutput {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
}

#[derive(Debug, Clone, thiserror::Error)]
pub(crate) enum BoxliteCommandError {
    #[error("command deadline exceeded")]
    DeadlineExceeded,
    #[error("box not found")]
    MissingBox,
    #[error("{0}")]
    ExecutionFailed(String),
}

pub(crate) async fn run_python_exec(
    cfg: &Config,
    code: &str,
    deadline_unix_ms: i64,
) -> Result<PythonExecRunResult, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let litebox = runtime
        .create(build_python_exec_box_options(cfg), None)
        .await
        .map_err(|err| {
            BoxliteCommandError::ExecutionFailed(format!("pythonExec create failed: {err}"))
        })?;

    let box_id = litebox.id().as_str().to_owned();
    if let Err(err) = litebox.start().await {
        remove_box(cfg, &box_id).await;
        return Err(BoxliteCommandError::ExecutionFailed(format!(
            "pythonExec start failed: {err}"
        )));
    }

    let execution_result = match run_command_in_box(
        runtime,
        &box_id,
        &litebox,
        python_command(code),
        deadline_unix_ms,
    )
    .await
    {
        Ok(output) => Ok(output),
        Err(BoxliteCommandError::MissingBox) => Err(BoxliteCommandError::ExecutionFailed(
            "pythonExec box not found".to_owned(),
        )),
        Err(err) => Err(err),
    };
    remove_box(cfg, &box_id).await;

    execution_result.map(|output| PythonExecRunResult {
        output: output.stdout,
        stderr: output.stderr,
        exit_code: output.exit_code,
    })
}

pub(crate) async fn create_terminal_session_box(
    cfg: &Config,
    box_name: &str,
) -> Result<String, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let litebox = runtime
        .create(
            build_terminal_exec_box_options(cfg),
            Some(box_name.trim().to_owned()),
        )
        .await
        .map_err(|err| {
            BoxliteCommandError::ExecutionFailed(format!("terminalExec create failed: {err}"))
        })?;

    let box_id = litebox.id().as_str().to_owned();
    if let Err(err) = litebox.start().await {
        remove_box(cfg, &box_id).await;
        return Err(BoxliteCommandError::ExecutionFailed(format!(
            "terminalExec start failed: {err}"
        )));
    }

    cache_terminal_session_box(litebox);
    Ok(box_id)
}

pub(crate) async fn exec_terminal_shell(
    cfg: &Config,
    box_id: &str,
    command: &str,
    deadline_unix_ms: i64,
) -> Result<CollectedExecOutput, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let litebox = get_terminal_session_box(runtime, box_id).await?;
    run_command_in_box(
        runtime,
        box_id,
        litebox.as_ref(),
        BoxCommand::new("sh").args(["-lc", command]),
        deadline_unix_ms,
    )
    .await
}

pub(crate) async fn exec_terminal_resource_probe(
    cfg: &Config,
    box_id: &str,
    action: &str,
    file_path: &str,
    max_read_bytes: usize,
    deadline_unix_ms: i64,
) -> Result<CollectedExecOutput, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let litebox = get_terminal_session_box(runtime, box_id).await?;
    run_command_in_box(
        runtime,
        box_id,
        litebox.as_ref(),
        resource_probe_command(action, file_path, max_read_bytes),
        deadline_unix_ms,
    )
    .await
}

pub(crate) async fn copy_out_terminal_file(
    cfg: &Config,
    box_id: &str,
    container_src: &str,
    host_dst: &Path,
    deadline_unix_ms: i64,
) -> Result<(), BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let litebox = get_terminal_session_box(runtime, box_id).await?;
    let copy_options = CopyOptions::default().non_recursive().include_parent(false);

    if let Some(remaining) = remaining_until_deadline(deadline_unix_ms) {
        if remaining.is_zero() {
            return Err(BoxliteCommandError::DeadlineExceeded);
        }
    }

    match remaining_until_deadline(deadline_unix_ms) {
        Some(remaining) => match tokio::time::timeout(
            remaining,
            litebox.copy_out(container_src, host_dst, copy_options),
        )
        .await
        {
            Ok(Ok(())) => Ok(()),
            Ok(Err(err)) => {
                if box_exists(runtime, box_id).await? {
                    Err(BoxliteCommandError::ExecutionFailed(format!(
                        "copy out file failed: {err}"
                    )))
                } else {
                    Err(BoxliteCommandError::MissingBox)
                }
            }
            Err(_) => Err(BoxliteCommandError::DeadlineExceeded),
        },
        None => match litebox
            .copy_out(container_src, host_dst, copy_options)
            .await
        {
            Ok(()) => Ok(()),
            Err(err) => {
                if box_exists(runtime, box_id).await? {
                    Err(BoxliteCommandError::ExecutionFailed(format!(
                        "copy out file failed: {err}"
                    )))
                } else {
                    Err(BoxliteCommandError::MissingBox)
                }
            }
        },
    }
}

pub(crate) async fn remove_box(cfg: &Config, box_id: &str) {
    let box_id = box_id.trim();
    if box_id.is_empty() {
        return;
    }

    drop_terminal_session_box(box_id);

    let Ok(runtime) = runtime(cfg) else {
        return;
    };

    match tokio::time::timeout(BOX_CLEANUP_TIMEOUT, runtime.remove(box_id, true)).await {
        Ok(Ok(())) => {}
        Ok(Err(err)) => {
            tracing::warn!(box_id = %box_id, error = %err, "boxlite box cleanup failed");
        }
        Err(_) => {
            tracing::warn!(box_id = %box_id, "boxlite box cleanup timed out");
        }
    }
}

pub(crate) async fn shutdown() {
    clear_terminal_session_boxes();

    let Some(runtime_state) = RUNTIME.get() else {
        return;
    };

    let Ok(runtime) = runtime_state else {
        return;
    };

    if let Err(err) = runtime.shutdown(None).await {
        tracing::warn!(error = %err, "boxlite runtime shutdown failed");
    }
}

fn runtime(cfg: &Config) -> Result<&'static BoxliteRuntime, BoxliteCommandError> {
    match RUNTIME.get_or_init(|| init_runtime(cfg)) {
        Ok(runtime) => Ok(runtime),
        Err(message) => Err(BoxliteCommandError::ExecutionFailed(message.clone())),
    }
}

fn init_runtime(cfg: &Config) -> Result<BoxliteRuntime, String> {
    let mut options = BoxliteOptions::default();
    if !cfg.boxlite_home.trim().is_empty() {
        options.home_dir = PathBuf::from(cfg.boxlite_home.trim());
    }

    BoxliteRuntime::new(options).map_err(|err| format!("initialize Boxlite runtime: {err}"))
}

fn build_python_exec_box_options(cfg: &Config) -> BoxOptions {
    let mut options = BoxOptions {
        cpus: Some(resolve_cpus(cfg.python_exec_cpus)),
        memory_mib: Some(cfg.python_exec_memory_mib),
        rootfs: RootfsSpec::Image(cfg.python_exec_image.clone()),
        auto_remove: false,
        entrypoint: Some(vec!["sleep".to_owned()]),
        cmd: Some(vec!["infinity".to_owned()]),
        ..Default::default()
    };
    options.advanced.security.resource_limits.max_processes =
        Some(cfg.python_exec_max_processes as u64);
    options
}

fn build_terminal_exec_box_options(cfg: &Config) -> BoxOptions {
    let mut options = BoxOptions {
        cpus: Some(resolve_cpus(cfg.terminal_exec_cpus)),
        memory_mib: Some(cfg.terminal_exec_memory_mib),
        rootfs: RootfsSpec::Image(cfg.terminal_exec_image.clone()),
        auto_remove: false,
        entrypoint: Some(vec!["sh".to_owned(), "-lc".to_owned()]),
        cmd: Some(vec![TERMINAL_EXEC_IDLE_COMMAND.to_owned()]),
        ..Default::default()
    };
    options.advanced.security.resource_limits.max_processes =
        Some(cfg.terminal_exec_max_processes as u64);
    options
}

async fn get_box(runtime: &BoxliteRuntime, box_id: &str) -> Result<LiteBox, BoxliteCommandError> {
    match runtime
        .get(box_id)
        .await
        .map_err(|err| BoxliteCommandError::ExecutionFailed(format!("lookup box failed: {err}")))?
    {
        Some(litebox) => Ok(litebox),
        None => Err(BoxliteCommandError::MissingBox),
    }
}

async fn get_terminal_session_box(
    runtime: &BoxliteRuntime,
    box_id: &str,
) -> Result<Arc<LiteBox>, BoxliteCommandError> {
    if let Some(litebox) = cached_terminal_session_box(box_id) {
        return Ok(litebox);
    }

    let litebox = Arc::new(get_box(runtime, box_id).await?);
    cache_terminal_session_box_arc(box_id, litebox.clone());
    Ok(litebox)
}

fn terminal_session_boxes() -> &'static Mutex<HashMap<String, Arc<LiteBox>>> {
    TERMINAL_SESSION_BOXES.get_or_init(|| Mutex::new(HashMap::new()))
}

fn cache_terminal_session_box(litebox: LiteBox) {
    let box_id = litebox.id().as_str().to_owned();
    cache_terminal_session_box_arc(&box_id, Arc::new(litebox));
}

fn cache_terminal_session_box_arc(box_id: &str, litebox: Arc<LiteBox>) {
    if let Ok(mut boxes) = terminal_session_boxes().lock() {
        boxes.insert(box_id.trim().to_owned(), litebox);
    }
}

fn cached_terminal_session_box(box_id: &str) -> Option<Arc<LiteBox>> {
    terminal_session_boxes()
        .lock()
        .ok()
        .and_then(|boxes| boxes.get(box_id.trim()).cloned())
}

fn drop_terminal_session_box(box_id: &str) {
    if let Ok(mut boxes) = terminal_session_boxes().lock() {
        boxes.remove(box_id.trim());
    }
}

fn clear_terminal_session_boxes() {
    if let Ok(mut boxes) = terminal_session_boxes().lock() {
        boxes.clear();
    }
}

async fn run_command_in_box(
    runtime: &BoxliteRuntime,
    box_id: &str,
    litebox: &LiteBox,
    command: BoxCommand,
    deadline_unix_ms: i64,
) -> Result<CollectedExecOutput, BoxliteCommandError> {
    if let Some(remaining) = remaining_until_deadline(deadline_unix_ms) {
        if remaining.is_zero() {
            return Err(BoxliteCommandError::DeadlineExceeded);
        }
    }

    let mut execution = litebox.exec(command).await.map_err(|err| {
        BoxliteCommandError::ExecutionFailed(format!("start command failed: {err}"))
    })?;

    let stdout_task = spawn_stdout_collector(execution.stdout());
    let stderr_task = spawn_stderr_collector(execution.stderr());

    let execution_result = match remaining_until_deadline(deadline_unix_ms) {
        Some(remaining) => match tokio::time::timeout(remaining, execution.wait()).await {
            Ok(wait_result) => match wait_result {
                Ok(result) => result,
                Err(err) => {
                    if box_exists(runtime, box_id).await? {
                        return Err(BoxliteCommandError::ExecutionFailed(format!(
                            "wait command failed: {err}"
                        )));
                    }
                    return Err(BoxliteCommandError::MissingBox);
                }
            },
            Err(_) => {
                let _ = execution.kill().await;
                stdout_task.abort();
                stderr_task.abort();
                return Err(BoxliteCommandError::DeadlineExceeded);
            }
        },
        None => execution.wait().await.map_err(|err| {
            BoxliteCommandError::ExecutionFailed(format!("wait command failed: {err}"))
        })?,
    };

    let stdout = finish_collector(stdout_task).await;
    let mut stderr = finish_collector(stderr_task).await;
    append_execution_error_message(&mut stderr, execution_result.error_message.as_deref());

    Ok(CollectedExecOutput {
        stdout,
        stderr,
        exit_code: execution_result.exit_code,
    })
}

async fn box_exists(runtime: &BoxliteRuntime, box_id: &str) -> Result<bool, BoxliteCommandError> {
    runtime.exists(box_id).await.map_err(|err| {
        BoxliteCommandError::ExecutionFailed(format!("check box existence failed: {err}"))
    })
}

fn python_command(code: &str) -> BoxCommand {
    BoxCommand::new("sh").args([
        "-c",
        r#"printf '%s' "$1" > /tmp/script.py && uv run /tmp/script.py"#,
        "_",
        code,
    ])
}

fn resource_probe_command(action: &str, file_path: &str, max_read_bytes: usize) -> BoxCommand {
    BoxCommand::new("python3").args([
        "-c",
        crate::runner::terminal_session_manager::TERMINAL_RESOURCE_PROBE_SCRIPT,
        "--action",
        action,
        "--file-path",
        file_path,
        "--max-read-bytes",
        &max_read_bytes.max(1).to_string(),
    ])
}

fn spawn_stdout_collector(stream: Option<ExecStdout>) -> JoinHandle<String> {
    tokio::spawn(async move { collect_stdout(stream).await })
}

fn spawn_stderr_collector(stream: Option<ExecStderr>) -> JoinHandle<String> {
    tokio::spawn(async move { collect_stderr(stream).await })
}

async fn collect_stdout(stream: Option<ExecStdout>) -> String {
    let Some(mut stream) = stream else {
        return String::new();
    };

    let mut output = String::new();
    while let Some(chunk) = stream.next().await {
        output.push_str(&chunk);
    }
    output
}

async fn collect_stderr(stream: Option<ExecStderr>) -> String {
    let Some(mut stream) = stream else {
        return String::new();
    };

    let mut output = String::new();
    while let Some(chunk) = stream.next().await {
        output.push_str(&chunk);
    }
    output
}

async fn finish_collector(handle: JoinHandle<String>) -> String {
    match handle.await {
        Ok(output) => output,
        Err(err) => {
            tracing::warn!(error = %err, "boxlite output collector failed");
            String::new()
        }
    }
}

fn append_execution_error_message(stderr: &mut String, error_message: Option<&str>) {
    let Some(message) = error_message
        .map(str::trim)
        .filter(|message| !message.is_empty())
    else {
        return;
    };

    if !stderr.is_empty() && !stderr.ends_with('\n') {
        stderr.push('\n');
    }
    stderr.push_str(message);
}

pub(crate) fn remaining_until_deadline(deadline_unix_ms: i64) -> Option<Duration> {
    if deadline_unix_ms <= 0 {
        return None;
    }

    let now_ms = now_unix_ms();
    if deadline_unix_ms <= now_ms {
        return Some(Duration::ZERO);
    }

    Some(Duration::from_millis((deadline_unix_ms - now_ms) as u64))
}

fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|duration| duration.as_millis() as i64)
        .unwrap_or_default()
}

fn resolve_cpus(cpus: u32) -> u8 {
    cpus.clamp(1, u8::MAX as u32) as u8
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn append_execution_error_message_preserves_existing_stderr() {
        let mut stderr = "first line".to_owned();
        append_execution_error_message(&mut stderr, Some("second line"));
        assert_eq!(stderr, "first line\nsecond line");
    }

    #[test]
    fn remaining_until_deadline_returns_zero_for_past_deadline() {
        assert_eq!(
            remaining_until_deadline(now_unix_ms() - 1),
            Some(Duration::ZERO)
        );
    }

    #[test]
    fn resolve_cpus_clamps_to_supported_range() {
        assert_eq!(resolve_cpus(0), 1);
        assert_eq!(resolve_cpus(1), 1);
        assert_eq!(resolve_cpus(999), u8::MAX);
    }

    #[test]
    fn terminal_exec_box_options_override_entrypoint_to_idle_loop() {
        let cfg = Config {
            config_file: None,
            console_grpc_target: String::new(),
            console_tls: false,
            worker_id: String::new(),
            worker_secret: String::new(),
            heartbeat_interval: Duration::from_secs(5),
            heartbeat_jitter_pct: 20,
            call_timeout: Duration::from_secs(13),
            node_name: String::new(),
            executor_kind: "boxlite".to_owned(),
            version: "dev".to_owned(),
            labels: Default::default(),
            boxlite_home: String::new(),
            python_exec_image: "ghcr.io/astral-sh/uv:python3.12-bookworm-slim".to_owned(),
            python_exec_memory_mib: 256,
            python_exec_cpus: 1,
            python_exec_max_processes: 128,
            terminal_exec_image: "coolfan1024/onlyboxes-default-worker:0.0.5".to_owned(),
            terminal_exec_memory_mib: 256,
            terminal_exec_cpus: 1,
            terminal_exec_max_processes: 128,
            terminal_lease_min_sec: 60,
            terminal_lease_max_sec: 1800,
            terminal_lease_default_sec: 60,
            terminal_output_limit_bytes: 1024 * 1024,
            terminal_export_max_bytes: 0,
            echo_max_inflight: 4,
            python_exec_max_inflight: 4,
            terminal_exec_max_inflight: 4,
            terminal_resource_max_inflight: 4,
            log_level: "info".to_owned(),
            log_format: "json".to_owned(),
            log_add_source: false,
        };

        let options = build_terminal_exec_box_options(&cfg);
        assert_eq!(
            options.entrypoint,
            Some(vec!["sh".to_owned(), "-lc".to_owned()])
        );
        assert_eq!(
            options.cmd,
            Some(vec![TERMINAL_EXEC_IDLE_COMMAND.to_owned()])
        );
    }

    #[test]
    fn python_exec_box_options_override_entrypoint_to_sleep_infinity() {
        let cfg = Config {
            config_file: None,
            console_grpc_target: String::new(),
            console_tls: false,
            worker_id: String::new(),
            worker_secret: String::new(),
            heartbeat_interval: Duration::from_secs(5),
            heartbeat_jitter_pct: 20,
            call_timeout: Duration::from_secs(13),
            node_name: String::new(),
            executor_kind: "boxlite".to_owned(),
            version: "dev".to_owned(),
            labels: Default::default(),
            boxlite_home: String::new(),
            python_exec_image: "ghcr.io/astral-sh/uv:python3.12-bookworm-slim".to_owned(),
            python_exec_memory_mib: 256,
            python_exec_cpus: 1,
            python_exec_max_processes: 128,
            terminal_exec_image: "coolfan1024/onlyboxes-default-worker:0.0.5".to_owned(),
            terminal_exec_memory_mib: 256,
            terminal_exec_cpus: 1,
            terminal_exec_max_processes: 128,
            terminal_lease_min_sec: 60,
            terminal_lease_max_sec: 1800,
            terminal_lease_default_sec: 60,
            terminal_output_limit_bytes: 1024 * 1024,
            terminal_export_max_bytes: 0,
            echo_max_inflight: 4,
            python_exec_max_inflight: 4,
            terminal_exec_max_inflight: 4,
            terminal_resource_max_inflight: 4,
            log_level: "info".to_owned(),
            log_format: "json".to_owned(),
            log_add_source: false,
        };

        let options = build_python_exec_box_options(&cfg);
        assert_eq!(options.entrypoint, Some(vec!["sleep".to_owned()]));
        assert_eq!(options.cmd, Some(vec!["infinity".to_owned()]));
    }

    #[test]
    fn python_command_uses_uv_run_via_sh() {
        let command = python_command("print('hello')");
        let debug = format!("{command:?}");
        assert!(debug.contains("command: \"sh\""));
        assert!(debug.contains("\"-c\""));
        assert!(debug.contains("uv run"));
        assert!(debug.contains("print('hello')"));
    }

    #[test]
    fn resource_probe_command_uses_python3() {
        let command = resource_probe_command("read", "/workspace/a.png", 123);
        let debug = format!("{command:?}");
        assert!(debug.contains("command: \"python3\""));
        assert!(debug.contains("\"--action\""));
        assert!(debug.contains("\"read\""));
        assert!(debug.contains("\"--file-path\""));
        assert!(debug.contains("\"/workspace/a.png\""));
        assert!(debug.contains("\"--max-read-bytes\""));
        assert!(debug.contains("\"123\""));
    }
}
