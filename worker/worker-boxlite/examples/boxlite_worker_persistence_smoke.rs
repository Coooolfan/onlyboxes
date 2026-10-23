//! Real runner startup, registration, command and snapshot transport. The
//! registry is a local fixture; this does not prove Console/S3 publication.
#![allow(dead_code, unused_imports)]
#[path = "../src/boxlite_runtime.rs"]
mod boxlite_runtime;
#[path = "../src/buildinfo.rs"]
mod buildinfo;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/config_source.rs"]
mod config_source;
#[path = "support/lease_guard.rs"]
mod lease_guard;
#[path = "../src/proto.rs"]
mod proto;
#[path = "../src/runner/mod.rs"]
mod runner;
#[path = "support/worker_registry.rs"]
mod worker_registry;
use runner::snapshot_control::{Control, ResultPayload};
use std::{
    os::unix::fs::PermissionsExt,
    path::Path,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio_util::sync::CancellationToken;
use worker_boxlite::persistence::{JournalState, ARCHIVE_FORMAT, RUNTIME_VERSION};
use worker_registry::proto::*;
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
const SCOPE: &str = "obx:smoke:production-worker";
fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}
async fn command(
    registry: &mut worker_registry::Registry,
    capability: &str,
    value: serde_json::Value,
) -> Result<serde_json::Value> {
    let id = uuid::Uuid::new_v4().to_string();
    registry
        .send(connect_response::Payload::CommandDispatch(
            CommandDispatch {
                command_id: id.clone(),
                capability: capability.into(),
                payload_json: serde_json::to_vec(&value)?,
                deadline_unix_ms: now() + 150_000,
            },
        ))
        .await?;
    loop {
        if let connect_request::Payload::CommandResult(result) = registry.event().await? {
            if result.command_id != id {
                return Err("wrong command receipt".into());
            }
            if let Some(error) = result.error {
                return Err(format!("{}: {}", error.code, error.message).into());
            }
            return Ok(serde_json::from_slice(&result.payload_json)?);
        }
    }
}
async fn control(
    registry: &mut worker_registry::Registry,
    action: sandbox_persistence_control::Action,
    value: Control,
) -> Result<ResultPayload> {
    let id = uuid::Uuid::new_v4().to_string();
    registry
        .send(connect_response::Payload::SandboxPersistenceControl(
            SandboxPersistenceControl {
                request_id: id.clone(),
                operation_id: "worker-snapshot".into(),
                scoped_session_id: SCOPE.into(),
                generation: 1,
                action: action as i32,
                payload_json: serde_json::to_vec(&value)?,
                deadline_unix_ms: now() + 150_000,
            },
        ))
        .await?;
    loop {
        if let connect_request::Payload::SandboxPersistenceReport(report) = registry.event().await?
        {
            if !report.completed {
                continue;
            }
            if report.request_id != id {
                return Err("wrong snapshot receipt".into());
            }
            if let Some(error) = report.error {
                return Err(format!("{}: {}", error.code, error.message).into());
            }
            return Ok(serde_json::from_slice(&report.payload_json)?);
        }
    }
}
fn journal(root: &Path) -> Result<JournalState> {
    Ok(serde_json::from_slice(&std::fs::read(
        root.join("journal/journal.json"),
    )?)?)
}
#[tokio::main]
async fn main() -> Result<()> {
    let root = std::env::temp_dir().join(format!(
        "onlyboxes-worker-persistence-{}",
        uuid::Uuid::new_v4()
    ));
    std::fs::create_dir(&root)?;
    std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700))?;
    let mut registry = worker_registry::Registry::start().await?;
    let mut cfg = config::Config::load();
    cfg.worker_id = "actual-persistence-worker".into();
    cfg.worker_secret = "fixture-only".into();
    cfg.console_grpc_target = registry.target.clone();
    cfg.console_tls = false;
    cfg.proxy_enabled = false;
    cfg.heartbeat_jitter_pct = 0;
    cfg.terminal_exec_image =
        "python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258".into();
    cfg.python_exec_image = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim".into();
    cfg.boxlite_home = root.join("sdk-home").to_str().unwrap().into();
    let mut guard = lease_guard::Guard::start(&root, &cfg.worker_id).await?;
    cfg.persistence.enabled = true;
    cfg.persistence.journal_dir = root.join("journal").to_str().unwrap().into();
    cfg.persistence.lease_state_dir = root.join("execution-leases").to_str().unwrap().into();
    cfg.persistence.lease_socket_dir = guard.socket_directory().to_str().unwrap().into();
    cfg.persistence.lease_store_id = guard.client.identity().store_id.clone();
    cfg.persistence.python_home = root.join("python-home").to_str().unwrap().into();
    let shutdown = CancellationToken::new();
    let mut worker = tokio::spawn(runner::run(shutdown.clone(), cfg.clone()));
    let mut killed = false;
    let outcome: Result<()> = async {
        let connect_request::Payload::Hello(hello) = registry.event().await? else { return Err("missing startup hello".into()); };
        let caps = hello.sandbox_persistence.ok_or("production capability missing")?;
        if !caps.enabled || caps.runtime != "boxlite" || caps.runtime_version != RUNTIME_VERSION
            || caps.write_formats != [ARCHIVE_FORMAT] || caps.read_formats != [ARCHIVE_FORMAT]
            || caps.max_inflight != cfg.persistence.max_inflight as i32 || caps.available_staging_bytes <= 0 {
            return Err("invalid production persistence declaration".into());
        }
        loop { if matches!(registry.event().await?, connect_request::Payload::TerminalSessionRecoveryReport(_)) { break; } }
        println!("PASS: actual runner binds the external store and protected SDK home before registering exact runtime, archive formats and staging capacity");
        let first = command(&mut registry,"terminalExec",serde_json::json!({"session_id":SCOPE,"generation":0,"create_if_missing":true,"lease_ttl_sec":300,"command":"printf preserved > /worker-persistent-data"})).await?;
        if !first["created"].as_bool().unwrap_or(false) || !journal(&root)?.adoptions[SCOPE].confirmed { return Err("first VM was published without its guard".into()); }
        let python = command(&mut registry,"pythonExec",serde_json::json!({"code":"print(42)"})).await?;
        if python["output"].as_str().unwrap_or_default().trim() != "42" { return Err(format!("stateless python execution failed: {python}").into()); }
        if std::fs::read_dir(root.join("sdk-home/boxes"))?.count() != 1 { return Err("pythonExec entered the protected terminal home".into()); }
        println!("PASS: ordinary gRPC commands use guarded first creation while stateless pythonExec uses its separate home");
        loop { if let connect_request::Payload::Heartbeat(frame) = registry.event().await? {
            let usage = frame.sandbox_persistence.ok_or("heartbeat omitted persistence usage")?;
            if usage.inflight != 0 || usage.available_staging_bytes <= 0 { return Err("invalid persistence usage".into()); }
            break;
        } }
        let inspected = control(&mut registry,sandbox_persistence_control::Action::Inspect,Control::default()).await?;
        let spec = inspected.restore_spec.ok_or("initial restore spec missing")?;
        let frozen = control(&mut registry,sandbox_persistence_control::Action::PrepareSnapshot,Control {
            restore_spec:Some(spec.clone()), max_bytes:512<<20, max_expanded_bytes:48<<30, ..Default::default()
        }).await?;
        if frozen.state != "frozen" || frozen.archive.is_none() { return Err("registered Worker did not freeze and export".into()); }
        let running = control(&mut registry,sandbox_persistence_control::Action::Cancel,Control {
            restore_spec:Some(spec), rollback:true, lease_ttl_sec:300, ..Default::default()
        }).await?;
        if running.state != "running" || running.instance_id != inspected.instance_id { return Err("rollback replaced the original root".into()); }
        let read = command(&mut registry,"terminalExec",serde_json::json!({"session_id":SCOPE,"generation":1,"command":"cat /worker-persistent-data","lease_ttl_sec":300})).await?;
        if read["stdout"].as_str()!=Some("preserved") { return Err("snapshot/rollback lost the original bytes".into()); }
        println!("PASS: registry snapshot frames freeze/export the actual VM and explicit rollback restarts the same root before generation-one execution");
        guard.stop_abruptly()?; killed = true;
        let result = tokio::time::timeout(Duration::from_secs(10), &mut worker).await
            .map_err(|_| "runner did not report companion failure")??;
        if result.is_ok() { return Err("Worker ignored companion loss".into()); }
        println!("PASS: companion loss closes and fails the production runner");
        runner::shutdown().await; boxlite_runtime::shutdown().await;
        let state = journal(&root)?;
        let writer = state.operations["worker-snapshot"].started_process.as_ref().ok_or("writer identity missing")?;
        tokio::time::timeout(Duration::from_secs(5), async {
            while !writer.writers_exited().unwrap_or(false) {
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        }).await.map_err(|_| "exact writer exit was not confirmed after emergency stop and SDK shutdown")?;
        let disk = root.join("sdk-home/boxes").join(&running.instance_id).join("disks/disk.qcow2");
        if !writer.writers_exited()? || !std::fs::metadata(&disk).is_ok_and(|m| m.is_file() && m.len() > 0) {
            return Err("companion failure did not stop the exact VM while retaining its root".into());
        }
        println!("PASS: independent companion loss fails the actual runner, closes admission and stops the exact VM while preserving its disk");
        Ok(())
    }.await;
    if let Err(error) = &outcome {
        eprintln!(
            "actual Worker acceptance failed: {error}; evidence={}",
            root.display()
        );
    }
    shutdown.cancel();
    if !worker.is_finished() {
        let _ = tokio::time::timeout(Duration::from_secs(10), &mut worker).await;
    }
    runner::shutdown().await;
    boxlite_runtime::shutdown().await;
    if !killed {
        let _ = guard.shutdown().await;
    }
    registry.close().await;
    if outcome.is_ok() {
        std::fs::remove_dir_all(root)?;
    }
    outcome
}
