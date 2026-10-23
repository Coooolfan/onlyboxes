use std::collections::{HashMap, HashSet};
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use boxlite::{
    BoxCommand, BoxConnection, BoxOptions, BoxStatus, BoxliteOptions, BoxliteRuntime, CopyOptions,
    ExecStderr, ExecStdout, LiteBox, RootfsSpec,
};
use tokio::task::JoinHandle;
use tokio_stream::StreamExt;
use tokio_util::sync::CancellationToken;

use crate::config::Config;

const BOX_CLEANUP_TIMEOUT: Duration = Duration::from_secs(3);
const TERMINAL_EXEC_IDLE_COMMAND: &str = "while true; do sleep 3600; done";
const TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE: &str = "terminal session manager is closed";

static RUNTIME: OnceLock<Result<BoxliteRuntime, String>> = OnceLock::new();
static PYTHON_RUNTIME: OnceLock<Result<BoxliteRuntime, String>> = OnceLock::new();
static RUNTIME_INIT: Mutex<()> = Mutex::new(());
static HOME_OWNER: OnceLock<Arc<worker_boxlite::persistence::RuntimeHomeGuard>> = OnceLock::new();
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
    let runtime = python_runtime(cfg)?;
    let litebox = runtime
        .create(build_python_exec_box_options(cfg), None)
        .await
        .map_err(|err| {
            BoxliteCommandError::ExecutionFailed(format!("pythonExec create failed: {err}"))
        })?;

    let box_id = litebox.id().as_str().to_owned();
    if let Err(err) = litebox.start().await {
        remove_runtime_box(runtime, &box_id).await;
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
    remove_runtime_box(runtime, &box_id).await;

    execution_result.map(|output| PythonExecRunResult {
        output: output.stdout,
        stderr: output.stderr,
        exit_code: output.exit_code,
    })
}

pub(crate) async fn create_guarded_terminal_session_box(
    cfg: &Config,
    scope: &str,
    journal: Arc<worker_boxlite::persistence::Journal>,
    guard: worker_boxlite::persistence::LeaseGuardClient,
    expires_unix_ms: i64,
) -> Result<String, BoxliteCommandError> {
    let owner = HOME_OWNER.get().ok_or_else(|| {
        BoxliteCommandError::ExecutionFailed(
            "managed creation requires the protected BoxLite home".into(),
        )
    })?;
    owner
        .verify(Path::new(&cfg.boxlite_home), &cfg.worker_id)
        .map_err(|_| {
            BoxliteCommandError::ExecutionFailed("BoxLite home ownership is unavailable".into())
        })?;
    if !Arc::ptr_eq(&owner.journal(), &journal) {
        return Err(BoxliteCommandError::ExecutionFailed(
            "BoxLite journal identity conflicts with its home".into(),
        ));
    }
    let native = worker_boxlite::persistence::NativeRuntime::new(
        Arc::new(runtime(cfg)?.clone()),
        &cfg.boxlite_home,
    )
    .map_err(|e| BoxliteCommandError::ExecutionFailed(e.to_string()))?
    .with_execution_guard(guard);
    let instance = native
        .create_initial_session(
            &journal,
            scope,
            build_terminal_exec_box_options(cfg),
            expires_unix_ms,
        )
        .await
        .map_err(|e| BoxliteCommandError::ExecutionFailed(e.to_string()))?;
    recover_pinned_terminal_session_box(cfg, &instance.box_id)
        .await?
        .ok_or(BoxliteCommandError::MissingBox)
}

pub(crate) async fn create_terminal_session_box(
    cfg: &Config,
    box_name: &str,
    shutdown: CancellationToken,
    deadline_unix_ms: i64,
) -> Result<String, BoxliteCommandError> {
    create_terminal_session_box_once(cfg, box_name, shutdown, deadline_unix_ms).await
}

async fn create_terminal_session_box_once(
    cfg: &Config,
    box_name: &str,
    shutdown: CancellationToken,
    deadline_unix_ms: i64,
) -> Result<String, BoxliteCommandError> {
    if shutdown.is_cancelled() {
        return Err(BoxliteCommandError::ExecutionFailed(
            TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE.to_owned(),
        ));
    }
    if remaining_until_deadline(deadline_unix_ms) == Some(Duration::ZERO) {
        return Err(BoxliteCommandError::DeadlineExceeded);
    }

    let runtime = runtime(cfg)?;
    let box_options = build_terminal_exec_box_options(cfg);
    let litebox = {
        let create = runtime.create(box_options, Some(box_name.trim().to_owned()));
        tokio::pin!(create);

        match remaining_until_deadline(deadline_unix_ms) {
            Some(remaining) => tokio::select! {
                _ = shutdown.cancelled() => Err(BoxliteCommandError::ExecutionFailed(
                    TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE.to_owned(),
                )),
                _ = tokio::time::sleep(remaining) => Err(BoxliteCommandError::DeadlineExceeded),
                result = &mut create => result.map_err(|err| {
                    BoxliteCommandError::ExecutionFailed(format!("terminalExec create failed: {err}"))
                }),
            },
            None => tokio::select! {
                _ = shutdown.cancelled() => Err(BoxliteCommandError::ExecutionFailed(
                    TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE.to_owned(),
                )),
                result = &mut create => result.map_err(|err| {
                    BoxliteCommandError::ExecutionFailed(format!("terminalExec create failed: {err}"))
                }),
            },
        }
    }?;

    let box_id = litebox.id().as_str().to_owned();
    let start_result = {
        let start = litebox.start();
        tokio::pin!(start);

        match remaining_until_deadline(deadline_unix_ms) {
            Some(remaining) if remaining.is_zero() => Err(BoxliteCommandError::DeadlineExceeded),
            Some(remaining) => tokio::select! {
                _ = shutdown.cancelled() => Err(BoxliteCommandError::ExecutionFailed(
                    TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE.to_owned(),
                )),
                _ = tokio::time::sleep(remaining) => Err(BoxliteCommandError::DeadlineExceeded),
                result = &mut start => result.map_err(|err| {
                    BoxliteCommandError::ExecutionFailed(format!("terminalExec start failed: {err}"))
                }),
            },
            None => tokio::select! {
                _ = shutdown.cancelled() => Err(BoxliteCommandError::ExecutionFailed(
                    TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE.to_owned(),
                )),
                result = &mut start => result.map_err(|err| {
                    BoxliteCommandError::ExecutionFailed(format!("terminalExec start failed: {err}"))
                }),
            },
        }
    };
    if let Err(err) = start_result {
        remove_box(cfg, &box_id).await;
        return Err(err);
    }
    if shutdown.is_cancelled() {
        remove_box(cfg, &box_id).await;
        return Err(BoxliteCommandError::ExecutionFailed(
            TERMINAL_SESSION_CREATE_CANCELLED_MESSAGE.to_owned(),
        ));
    }
    if remaining_until_deadline(deadline_unix_ms) == Some(Duration::ZERO) {
        remove_box(cfg, &box_id).await;
        return Err(BoxliteCommandError::DeadlineExceeded);
    }

    cache_terminal_session_box(litebox);
    Ok(box_id)
}

pub(crate) async fn recover_terminal_session_box(
    cfg: &Config,
    box_name: &str,
) -> Result<Option<String>, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let Some(litebox) = runtime
        .get(box_name.trim())
        .await
        .map_err(|err| BoxliteCommandError::ExecutionFailed(format!("lookup box failed: {err}")))?
    else {
        return Ok(None);
    };

    let status = litebox
        .info()
        .await
        .map_err(|err| {
            BoxliteCommandError::ExecutionFailed(format!("inspect terminalExec box: {err}"))
        })?
        .status;
    match status {
        BoxStatus::Running => {}
        BoxStatus::Configured | BoxStatus::Stopped => {
            litebox.start().await.map_err(|err| {
                BoxliteCommandError::ExecutionFailed(format!("restart terminalExec box: {err}"))
            })?;
        }
        BoxStatus::Unknown | BoxStatus::Stopping | BoxStatus::Paused | BoxStatus::Failed => {
            return Err(BoxliteCommandError::ExecutionFailed(format!(
                "terminalExec box has invalid status: {:?}",
                status
            )));
        }
    }

    let box_id = litebox.id().as_str().to_owned();
    cache_terminal_session_box(litebox);
    Ok(Some(box_id))
}

/// Recover only the already running, durably selected ID. Ordinary name-based
/// recovery can restart a stopped box and is not used for lifecycle-owned roots.
pub(crate) async fn recover_pinned_terminal_session_box(
    cfg: &Config,
    box_id: &str,
) -> Result<Option<String>, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let Some(litebox) = runtime.get(box_id).await.map_err(|_| {
        BoxliteCommandError::ExecutionFailed("inspect persistent terminal box failed".into())
    })?
    else {
        return Ok(None);
    };
    let info = litebox.info().await.map_err(|_| {
        BoxliteCommandError::ExecutionFailed("inspect persistent terminal box failed".into())
    })?;
    if info.id.as_str() != box_id || info.status != BoxStatus::Running || info.pid.is_none() {
        return Err(BoxliteCommandError::ExecutionFailed(
            "persistent terminal instance is not running".into(),
        ));
    }
    let id = litebox.id().as_str().to_owned();
    cache_terminal_session_box(litebox);
    Ok(Some(id))
}

pub(crate) async fn cleanup_orphan_terminal_session_boxes(
    cfg: &Config,
    expected_names: &HashSet<String>,
) -> usize {
    let Ok(runtime) = runtime(cfg) else {
        return 0;
    };
    let infos = match runtime.list_info().await {
        Ok(infos) => infos,
        Err(err) => {
            tracing::warn!(error = %err, "list terminal Box orphans failed");
            return 0;
        }
    };

    let mut cleaned = 0;
    for info in infos {
        let Some(name) = info.name.as_deref() else {
            continue;
        };
        if !is_onlyboxes_terminal_session_box_name(name) || expected_names.contains(name) {
            continue;
        }
        remove_box(cfg, info.id.as_str()).await;
        cleaned += 1;
    }
    cleaned
}

fn is_onlyboxes_terminal_session_box_name(name: &str) -> bool {
    const PREFIX: &str = "onlyboxes-terminal-v1-";
    let Some(hash) = name.strip_prefix(PREFIX) else {
        return false;
    };
    hash.len() == 64
        && hash
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
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
    let copy_options = CopyOptions::default().non_recursive();

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

    remove_runtime_box(runtime, box_id).await;
}

async fn remove_runtime_box(runtime: &BoxliteRuntime, box_id: &str) {
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
    for runtime in [RUNTIME.get(), PYTHON_RUNTIME.get()]
        .into_iter()
        .flatten()
        .flatten()
    {
        if let Err(err) = runtime.shutdown(None).await {
            tracing::warn!(error = %err, "boxlite runtime shutdown failed");
        }
    }
}

fn python_runtime(cfg: &Config) -> Result<&'static BoxliteRuntime, BoxliteCommandError> {
    if !cfg.persistence.enabled {
        return runtime(cfg);
    }
    if HOME_OWNER.get().is_none() {
        return Err(BoxliteCommandError::ExecutionFailed(
            "persistent runtime startup has not completed".into(),
        ));
    }
    match PYTHON_RUNTIME.get_or_init(|| {
        cfg.persistence.validate(&cfg.boxlite_home)?;
        for marker in [".onlyboxes-owner.json", ".onlyboxes-owner.lock"] {
            match std::fs::symlink_metadata(Path::new(&cfg.persistence.python_home).join(marker)) {
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
                _ => return Err("pythonExec home belongs to a persistent runtime".into()),
            }
        }
        BoxliteRuntime::new(BoxliteOptions {
            home_dir: PathBuf::from(&cfg.persistence.python_home),
            ..Default::default()
        })
        .map_err(|err| format!("initialize pythonExec runtime: {err}"))
    }) {
        Ok(runtime) => Ok(runtime),
        Err(error) => Err(BoxliteCommandError::ExecutionFailed(error.clone())),
    }
}

pub(crate) fn runtime(cfg: &Config) -> Result<&'static BoxliteRuntime, BoxliteCommandError> {
    let _initializing = RUNTIME_INIT.lock().map_err(|_| {
        BoxliteCommandError::ExecutionFailed("Boxlite startup ownership is unavailable".into())
    })?;
    runtime_owned(cfg)
}

#[allow(dead_code)]
pub(crate) fn initialize_guarded(
    cfg: &Config,
    guard: Arc<worker_boxlite::persistence::RuntimeHomeGuard>,
) -> Result<&'static BoxliteRuntime, BoxliteCommandError> {
    let _initializing = RUNTIME_INIT.lock().map_err(|_| {
        BoxliteCommandError::ExecutionFailed("Boxlite startup ownership is unavailable".into())
    })?;
    if RUNTIME.get().is_some() {
        return Err(BoxliteCommandError::ExecutionFailed(
            "Boxlite runtime was already initialized".into(),
        ));
    }
    guard
        .verify(Path::new(&cfg.boxlite_home), &cfg.worker_id)
        .map_err(|_| {
            BoxliteCommandError::ExecutionFailed("Boxlite startup ownership is unavailable".into())
        })?;
    guard.census().map_err(|_| {
        BoxliteCommandError::ExecutionFailed("Boxlite startup census is unavailable".into())
    })?;
    if HOME_OWNER.set(guard).is_err() {
        return Err(BoxliteCommandError::ExecutionFailed(
            "Boxlite runtime was already initialized".into(),
        ));
    }
    runtime_owned(cfg)
}

fn runtime_owned(cfg: &Config) -> Result<&'static BoxliteRuntime, BoxliteCommandError> {
    if cfg.persistence.enabled && HOME_OWNER.get().is_none() {
        return Err(BoxliteCommandError::ExecutionFailed(
            "persistent BoxLite home requires startup ownership".into(),
        ));
    }
    if let Some(owner) = HOME_OWNER.get() {
        owner
            .verify(Path::new(&cfg.boxlite_home), &cfg.worker_id)
            .map_err(|_| {
                BoxliteCommandError::ExecutionFailed(
                    "Boxlite startup ownership is unavailable".into(),
                )
            })?;
    }
    match RUNTIME.get_or_init(|| init_runtime(cfg)) {
        Ok(runtime) => Ok(runtime),
        Err(message) => Err(BoxliteCommandError::ExecutionFailed(message.clone())),
    }
}

pub(crate) fn validate_unmanaged_home(cfg: &Config) -> Result<(), BoxliteCommandError> {
    let home = if cfg.boxlite_home.trim().is_empty() {
        BoxliteOptions::default().home_dir
    } else {
        PathBuf::from(cfg.boxlite_home.trim())
    };
    check_unmanaged_home(&home).map_err(BoxliteCommandError::ExecutionFailed)
}
fn check_unmanaged_home(home: &Path) -> Result<(), String> {
    for marker in [".onlyboxes-owner.json", ".onlyboxes-owner.lock"] {
        match std::fs::symlink_metadata(home.join(marker)) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
            _ => return Err("Boxlite home requires guarded journal ownership".into()),
        }
    }
    Ok(())
}

fn init_runtime(cfg: &Config) -> Result<BoxliteRuntime, String> {
    let mut options = BoxliteOptions::default();
    if !cfg.boxlite_home.trim().is_empty() {
        options.home_dir = PathBuf::from(cfg.boxlite_home.trim());
    }

    if HOME_OWNER.get().is_none() {
        check_unmanaged_home(&options.home_dir)?;
    }
    BoxliteRuntime::new(options).map_err(|err| format!("initialize Boxlite runtime: {err}"))
}

fn build_python_exec_box_options(cfg: &Config) -> BoxOptions {
    let mut options = BoxOptions {
        cpus: Some(resolve_cpus(cfg.python_exec_cpus)),
        memory_mib: Some(cfg.python_exec_memory_mib),
        rootfs: RootfsSpec::Image(cfg.python_exec_image.clone()),
        auto_delete: Some(0),
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
        auto_delete: Some(0),
        detach: true,
        entrypoint: Some(vec!["sh".to_owned(), "-lc".to_owned()]),
        cmd: Some(vec![TERMINAL_EXEC_IDLE_COMMAND.to_owned()]),
        ..Default::default()
    };
    options.advanced.security.resource_limits.max_processes =
        Some(cfg.terminal_exec_max_processes as u64);
    options
}

pub(crate) async fn open_terminal_proxy_connection(
    cfg: &Config,
    box_id: &str,
    guest_port: u16,
) -> Result<BoxConnection, BoxliteCommandError> {
    let runtime = runtime(cfg)?;
    let litebox = get_terminal_session_box(runtime, box_id).await?;
    let guest_ip = boxlite::net::constants::GUEST_IP.parse().map_err(|err| {
        BoxliteCommandError::ExecutionFailed(format!("parse BoxLite guest IP: {err}"))
    })?;
    let tunnel = litebox
        .network()
        .tunnel(SocketAddr::new(guest_ip, guest_port))
        .await
        .map_err(|err| {
            BoxliteCommandError::ExecutionFailed(format!("open sandbox tunnel: {err}"))
        })?;
    tunnel.connect().await.map_err(|err| {
        BoxliteCommandError::ExecutionFailed(format!("connect sandbox tunnel: {err}"))
    })
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

pub(crate) fn forget_terminal_session_box(box_id: &str) {
    drop_terminal_session_box(box_id);
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
    fn orphan_cleanup_name_filter_rejects_foreign_boxes() {
        let valid = format!("onlyboxes-terminal-v1-{}", "a".repeat(64));
        assert!(is_onlyboxes_terminal_session_box_name(&valid));
        assert!(!is_onlyboxes_terminal_session_box_name("developer-box"));
        assert!(!is_onlyboxes_terminal_session_box_name(
            "onlyboxes-terminal-v1-not-a-session-hash"
        ));
        assert!(!is_onlyboxes_terminal_session_box_name(&format!(
            "onlyboxes-terminal-v1-{}",
            "A".repeat(64)
        )));
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
            labels: Default::default(),
            boxlite_home: String::new(),
            persistence: Default::default(),
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
            proxy_enabled: true,
            proxy_listen_addr: "0.0.0.0:8091".to_owned(),
            proxy_advertise_addr: String::new(),
            proxy_sandbox_ports: vec![3000, 8080],
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
        assert_eq!(options.auto_delete, Some(0));
        assert!(options.ports.is_empty());
        assert!(options.detach);
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
            labels: Default::default(),
            boxlite_home: String::new(),
            persistence: Default::default(),
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

#[cfg(test)]
mod ownership_startup_tests {
    use super::*;
    #[test]
    fn disabled_persistence_cannot_register_around_a_protected_home() {
        let root =
            std::env::temp_dir().join(format!("onlyboxes-disabled-home-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir(&root).unwrap();
        let mut cfg = Config::load();
        cfg.boxlite_home = root.to_str().unwrap().into();
        validate_unmanaged_home(&cfg).unwrap();
        std::fs::write(root.join(".onlyboxes-owner.lock"), []).unwrap();
        assert!(validate_unmanaged_home(&cfg).is_err());
        assert_eq!(std::fs::read_dir(&root).unwrap().count(), 1);
        std::fs::remove_dir_all(root).unwrap();
    }
}
