use std::sync::Arc;
use std::time::Instant;

use boxlite::{BoxCommand, BoxOptions, BoxliteRuntime, RootfsSpec};
use futures::StreamExt;
use serde::{Deserialize, Serialize};

use super::CapabilityError;
use crate::config::Config;

#[derive(Deserialize)]
struct PythonExecRequest {
    code: String,
}

#[derive(Serialize)]
struct PythonExecResponse {
    output: String,
    stderr: String,
    exit_code: i32,
}

pub async fn handle_python_exec(
    runtime: &Arc<BoxliteRuntime>,
    config: &Config,
    payload: &[u8],
    deadline: Option<Instant>,
) -> Result<Vec<u8>, CapabilityError> {
    let req: PythonExecRequest = serde_json::from_slice(payload)
        .map_err(|e| CapabilityError::new("invalid_payload", format!("bad JSON: {e}")))?;

    tracing::debug!(code_len = req.code.len(), "pythonExec");

    let vm_name = format!("obx-pythonexec-{}", random_hex(8));

    let litebox = runtime
        .create(
            BoxOptions {
                cpus: Some(config.boxlite_default_cpus),
                memory_mib: Some(config.boxlite_default_memory_mib),
                rootfs: RootfsSpec::Image(config.python_exec_image.clone()),
                auto_remove: true,
                ..Default::default()
            },
            Some(vm_name.clone()),
        )
        .await
        .map_err(|e| CapabilityError::new("execution_failed", format!("create VM: {e}")))?;

    // Ensure cleanup on all paths
    let _cleanup = scopeguard::guard((), {
        let rt = runtime.clone();
        let name = vm_name.clone();
        move |_| {
            // Fire-and-forget async cleanup
            tokio::spawn(async move {
                if let Err(e) = rt.remove(&name, true).await {
                    tracing::warn!(vm = %name, error = %e, "pythonExec cleanup failed");
                }
            });
        }
    });

    litebox
        .start()
        .await
        .map_err(|e| CapabilityError::new("execution_failed", format!("start VM: {e}")))?;

    let exec_fut = async {
        let cmd = BoxCommand::new("python3")
            .args(["-c", &req.code]);

        let mut execution = litebox
            .exec(cmd)
            .await
            .map_err(|e| CapabilityError::new("execution_failed", format!("exec: {e}")))?;

        let stdout = collect_stream(execution.stdout()).await;
        let stderr = collect_stream(execution.stderr()).await;

        let result = execution
            .wait()
            .await
            .map_err(|e| CapabilityError::new("execution_failed", format!("wait: {e}")))?;

        Ok::<_, CapabilityError>(PythonExecResponse {
            output: stdout,
            stderr,
            exit_code: result.exit_code,
        })
    };

    let resp = match deadline {
        Some(dl) => {
            let timeout = dl.saturating_duration_since(Instant::now());
            tokio::time::timeout(timeout, exec_fut)
                .await
                .map_err(|_| CapabilityError::new("deadline_exceeded", "execution timed out"))?
        }
        None => exec_fut.await,
    }?;

    serde_json::to_vec(&resp)
        .map_err(|e| CapabilityError::new("encode_failed", format!("JSON encode failed: {e}")))
}

async fn collect_stream(stream: Option<impl futures::Stream<Item = String> + Unpin>) -> String {
    match stream {
        Some(mut s) => {
            let mut buf = String::new();
            while let Some(line) = s.next().await {
                buf.push_str(&line);
            }
            buf
        }
        None => String::new(),
    }
}

fn random_hex(len: usize) -> String {
    use rand::Rng;
    let mut rng = rand::thread_rng();
    (0..len).map(|_| format!("{:02x}", rng.gen::<u8>())).collect()
}
