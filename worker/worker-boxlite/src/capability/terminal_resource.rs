use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Instant;

use boxlite::BoxCommand;
use futures::StreamExt;
use serde::{Deserialize, Serialize};

use super::CapabilityError;
use super::terminal_exec::SessionManager;
use crate::config::Config;

const PROBE_SCRIPT: &str = r#"
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

// ---------- Request / Response ----------

#[derive(Deserialize)]
struct TerminalResourceRequest {
    session_id: String,
    file_path: String,
    #[serde(default)]
    action: Option<String>,
}

#[derive(Serialize)]
struct TerminalResourceResponse {
    session_id: String,
    file_path: String,
    mime_type: String,
    size_bytes: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    blob: Option<String>,
}

#[derive(Deserialize)]
struct ProbeResult {
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    message: Option<String>,
    #[serde(default)]
    mime_type: Option<String>,
    #[serde(default)]
    size_bytes: i64,
    #[serde(default)]
    blob: Option<String>,
}

// ---------- Handler ----------

pub async fn handle_terminal_resource(
    session_mgr: &Arc<SessionManager>,
    config: &Config,
    payload: &[u8],
    deadline: Option<Instant>,
) -> Result<Vec<u8>, CapabilityError> {
    let req: TerminalResourceRequest = serde_json::from_slice(payload)
        .map_err(|e| CapabilityError::new("invalid_payload", format!("bad JSON: {e}")))?;

    let session_id = req.session_id.trim().to_string();
    let file_path = req.file_path.trim().to_string();

    if session_id.is_empty() || file_path.is_empty() {
        return Err(CapabilityError::new(
            "invalid_payload",
            "session_id and file_path are required",
        ));
    }

    let action = normalize_action(req.action.as_deref());
    if action.is_empty() {
        return Err(CapabilityError::new(
            "invalid_payload",
            "action must be validate or read",
        ));
    }

    tracing::debug!(
        action = %action,
        session_id_present = true,
        file_path_len = file_path.len(),
        "terminalResource"
    );

    // Lookup session
    let session = session_mgr
        .get_session(&session_id)
        .await
        .ok_or_else(|| CapabilityError::new("session_not_found", "session not found"))?;

    // Acquire busy lock
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

    let session_for_guard = session.clone();
    let _busy_guard = scopeguard::guard((), move |_| {
        session_for_guard.busy.store(false, Ordering::Release);
    });

    // Execute probe script
    let limit = config.terminal_output_limit_bytes;
    let exec_fut = async {
        let cmd = BoxCommand::new("python3").args([
            "-c",
            PROBE_SCRIPT,
            "--action",
            &action,
            "--file-path",
            &file_path,
            "--max-read-bytes",
            &limit.to_string(),
        ]);

        let mut execution = session
            .litebox
            .exec(cmd)
            .await
            .map_err(|e| CapabilityError::new("execution_failed", format!("exec: {e}")))?;

        let stdout = collect_stdout(execution.stdout()).await;
        let result = execution
            .wait()
            .await
            .map_err(|e| CapabilityError::new("execution_failed", format!("wait: {e}")))?;

        Ok::<_, CapabilityError>((stdout, result.exit_code))
    };

    let (stdout, exit_code) = match deadline {
        Some(dl) => {
            let timeout = dl.saturating_duration_since(Instant::now());
            tokio::time::timeout(timeout, exec_fut)
                .await
                .map_err(|_| CapabilityError::new("deadline_exceeded", "execution timed out"))?
        }
        None => exec_fut.await,
    }?;

    // Parse probe output
    let trimmed = stdout.trim();
    if trimmed.is_empty() {
        return Err(CapabilityError::new(
            "execution_failed",
            "empty probe output",
        ));
    }

    let probe: ProbeResult = serde_json::from_str(trimmed)
        .map_err(|e| CapabilityError::new("execution_failed", format!("parse probe: {e}")))?;

    // Check for domain errors from probe script
    if let Some(error_code) = &probe.error {
        let code = error_code.trim();
        if !code.is_empty() {
            let message = probe
                .message
                .as_deref()
                .map(|m| m.trim())
                .filter(|m| !m.is_empty())
                .unwrap_or_else(|| default_error_message(code));
            return Err(CapabilityError::new(code, message));
        }
    }

    if exit_code != 0 {
        return Err(CapabilityError::new(
            "execution_failed",
            format!("probe exited with code {exit_code}"),
        ));
    }

    let mime_type = probe
        .mime_type
        .as_deref()
        .map(|m| m.trim())
        .filter(|m| !m.is_empty())
        .unwrap_or("application/octet-stream")
        .to_string();

    let resp = TerminalResourceResponse {
        session_id: session_id.clone(),
        file_path: file_path.clone(),
        mime_type,
        size_bytes: probe.size_bytes,
        blob: if action == "read" {
            Some(probe.blob.unwrap_or_default())
        } else {
            None
        },
    };

    serde_json::to_vec(&resp)
        .map_err(|e| CapabilityError::new("encode_failed", format!("JSON encode: {e}")))
}

fn normalize_action(action: Option<&str>) -> String {
    match action.map(|a| a.trim().to_lowercase()).as_deref() {
        None | Some("") | Some("validate") => "validate".to_string(),
        Some("read") => "read".to_string(),
        _ => String::new(),
    }
}

fn default_error_message(code: &str) -> &str {
    match code {
        "file_not_found" => "file not found",
        "path_is_directory" => "path is directory",
        "file_too_large" => "file exceeds read limit",
        _ => "terminal resource operation failed",
    }
}

async fn collect_stdout(stream: Option<impl futures::Stream<Item = String> + Unpin>) -> String {
    match stream {
        Some(mut s) => {
            let mut buf = String::new();
            while let Some(chunk) = s.next().await {
                buf.push_str(&chunk);
            }
            buf
        }
        None => String::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_normalize_action_defaults_to_validate() {
        assert_eq!(normalize_action(None), "validate");
        assert_eq!(normalize_action(Some("")), "validate");
        assert_eq!(normalize_action(Some("  ")), "validate");
    }

    #[test]
    fn test_normalize_action_validate() {
        assert_eq!(normalize_action(Some("validate")), "validate");
        assert_eq!(normalize_action(Some("VALIDATE")), "validate");
        assert_eq!(normalize_action(Some(" Validate ")), "validate");
    }

    #[test]
    fn test_normalize_action_read() {
        assert_eq!(normalize_action(Some("read")), "read");
        assert_eq!(normalize_action(Some("READ")), "read");
    }

    #[test]
    fn test_normalize_action_invalid() {
        assert_eq!(normalize_action(Some("delete")), "");
        assert_eq!(normalize_action(Some("write")), "");
    }

    #[test]
    fn test_default_error_message_known_codes() {
        assert_eq!(default_error_message("file_not_found"), "file not found");
        assert_eq!(default_error_message("path_is_directory"), "path is directory");
        assert_eq!(default_error_message("file_too_large"), "file exceeds read limit");
    }

    #[test]
    fn test_default_error_message_unknown_code() {
        assert_eq!(
            default_error_message("something_else"),
            "terminal resource operation failed"
        );
    }
}
