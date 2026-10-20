//! Exercise real BoxLite source lifecycle actions through management frames.
//! The temporary object service/coordinator does not prove Console/S3 transactions.
#![allow(dead_code, unused_imports)]
#[path = "../src/boxlite_runtime.rs"]
mod boxlite_runtime;
#[path = "../src/buildinfo.rs"]
mod buildinfo;
#[path = "../src/config.rs"]
mod config;
#[path = "../src/config_source.rs"]
mod config_source;
#[path = "../src/proto.rs"]
mod proto;
#[path = "../src/runner/mod.rs"]
mod runner;
#[path = "support/snapshot_store.rs"]
mod snapshot_store;
use proto::registryv1::{
    sandbox_persistence_control::Action, SandboxPersistenceControl, SandboxPersistenceReport,
};
use runner::snapshot_control::{Control, ControlDispatcher, ResultPayload};
use runner::terminal_session_manager::{
    SourceControlConfig, SourceControlEngine, TerminalExecRequest, TerminalSessionManager,
};
use std::{
    os::unix::fs::PermissionsExt,
    path::Path,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use worker_boxlite::persistence::{Journal, NativeRuntime, UploadPart};

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
const SCOPE: &str = "obx:smoke:source-controls";
const EPOCH: &str = "source-control-epoch";
const FIRST: &str = "rollback-source-operation";
const SECOND: &str = "committed-source-operation";
const IMAGE: &str =
    "python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258";
fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}
fn private(path: &Path) -> Result<()> {
    std::fs::create_dir(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}
fn exec(generation: i64, command: &str) -> TerminalExecRequest {
    TerminalExecRequest {
        session_id: SCOPE.into(),
        generation,
        connection_id: Some(EPOCH.into()),
        command: command.into(),
        create_if_missing: true,
        lease_ttl_sec: Some(300),
        deadline_unix_ms: now_ms() + 30_000,
    }
}
async fn control(
    dispatcher: &Arc<ControlDispatcher>,
    operation: &str,
    action: Action,
    payload: Control,
) -> Result<SandboxPersistenceReport> {
    Ok(dispatcher
        .submit(
            EPOCH,
            SandboxPersistenceControl {
                request_id: uuid::Uuid::new_v4().to_string(),
                operation_id: operation.into(),
                scoped_session_id: SCOPE.into(),
                generation: 1,
                action: action as i32,
                deadline_unix_ms: now_ms() + 300_000,
                payload_json: serde_json::to_vec(&payload)?,
            },
        )
        .await?)
}
fn value(report: SandboxPersistenceReport) -> Result<ResultPayload> {
    if let Some(error) = report.error {
        return Err(format!("control failed: {}", error.code).into());
    }
    Ok(serde_json::from_slice(&report.payload_json)?)
}
fn rejected(report: SandboxPersistenceReport, code: &str) -> Result<()> {
    if report.error.as_ref().map(|e| e.code.as_str()) != Some(code) {
        return Err("expected lifecycle rejection missing".into());
    }
    Ok(())
}
#[tokio::main]
async fn main() -> Result<()> {
    let root = std::env::temp_dir().join(format!(
        "onlyboxes-source-controls-{}",
        uuid::Uuid::new_v4()
    ));
    private(&root)?;
    let home = root.join("sdk-home");
    private(&home)?;
    let mut cfg = config::Config::load();
    cfg.worker_id = "source-controls-worker".into();
    cfg.worker_secret = "fixture-only".into();
    cfg.boxlite_home = home.to_str().ok_or("invalid home")?.into();
    cfg.terminal_exec_image = IMAGE.into();
    cfg.terminal_max_active_sessions = 1;
    cfg.proxy_enabled = false;
    let mut manager = TerminalSessionManager::from_config(&cfg);
    let journal = Arc::new(Journal::open(root.join("journal"), &cfg.worker_id)?);
    let sdk = Arc::new(boxlite_runtime::runtime(&cfg)?.clone());
    let native = Arc::new(NativeRuntime::new(sdk.clone(), &home)?);
    let settings = SourceControlConfig {
        max_inflight: 1,
        tmp_quota_bytes: 128 << 30,
        drain_timeout: Duration::from_secs(30),
        readiness_timeout: Duration::from_secs(30),
        cache_ttl: Duration::from_secs(300),
        transfer_concurrency: 4,
        transfer_idle: Duration::from_secs(30),
    };
    let engine = SourceControlEngine::new(
        manager.clone(),
        journal.clone(),
        native.clone(),
        settings.clone(),
    )
    .await
    .map_err(|_| "source engine configuration failed")?;
    let mut dispatcher =
        ControlDispatcher::new(engine, 2).map_err(|_| "dispatcher configuration failed")?;
    dispatcher
        .begin_connection(EPOCH)
        .await
        .map_err(|_| "connection barrier failed")?;
    manager
        .protect_persistence_recovery(EPOCH, &[], &[])
        .await?;
    manager.finish_persistence_recovery(EPOCH).await?;
    dispatcher
        .finish_connection(EPOCH)
        .map_err(|_| "connection recovery failed")?;
    let store = snapshot_store::Store::new(&root.join("object-store")).await?;
    println!(
        "source management-frame acceptance; root={}",
        root.display()
    );
    let outcome: Result<()> = async {
        manager.execute(exec(0, "python -c \"from pathlib import Path; Path('/source-control-data').write_text('preserved')\"" )).await?;
        let initial = value(control(&dispatcher, FIRST, Action::Inspect, Control::default()).await?)?;
        if initial.state != "running" { return Err("source inspection failed".into()); }
        let spec = initial.restore_spec.ok_or("spec missing")?;
        let id = initial.instance_id;
        if manager.execute(exec(0, "stale-generation")).await.is_ok() { return Err("bootstrap accepted stale generation".into()); }
        let mut oversized = Control { restore_spec: Some(spec.clone()), max_bytes: 1 << 59, max_expanded_bytes: 1 << 59, ..Default::default() };
        rejected(control(&dispatcher, FIRST, Action::PrepareSnapshot, oversized.clone()).await?, "snapshot_quota_exceeded")?;
        if manager.execute(exec(1, "cat /source-control-data")).await?.stdout != "preserved" { return Err("budget rejection held or changed the source".into()); }
        oversized.max_bytes = 256 << 20; oversized.max_expanded_bytes = 64 << 30;
        rejected(control(&dispatcher, "unclaimed-source-operation", Action::Cancel, Control { rollback: true, ..Default::default() }).await?, "session_state_conflict")?;
        if journal.snapshot()?.operations.contains_key("unclaimed-source-operation") {
            return Err("missing source metadata created a cancellation tombstone".into());
        }
        let restored_unknown = value(control(&dispatcher, "unclaimed-source-operation", Action::Cancel, Control { restore_spec: Some(spec.clone()), rollback: true, ..Default::default() }).await?)?;
        if restored_unknown.instance_id != id || manager.execute(exec(1, "cat /source-control-data")).await?.stdout != "preserved" {
            return Err("rollback without a source claim failed to preserve the probed root".into());
        }

        // A real accepted exec times out after source admission closes. Drain
        // must fail without discarding its root; explicit rollback can restore
        // that same responsive VM once accepted request counters reach zero.
        let accepted = tokio::spawn({ let manager = manager.clone(); async move {
            let mut request = exec(1, "python -c \"from pathlib import Path; Path('/accepted-exec-started').write_text('yes'); import time; time.sleep(10)\"");
            request.deadline_unix_ms = now_ms() + 2_000;
            manager.execute(request).await
        }});
        let observed = async {
            loop {
                let probe = sdk.get(&id).await?.ok_or("source missing during exec")?
                    .exec(boxlite::BoxCommand::new("sh").args(["-c", "test -f /accepted-exec-started"])).await?.wait().await?;
                if probe.exit_code == 0 { return Ok::<(), Box<dyn std::error::Error>>(()); }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        };
        tokio::time::timeout(Duration::from_secs(5), observed).await??;
        rejected(control(&dispatcher, "fault-source-operation", Action::PrepareSnapshot, oversized.clone()).await?, "session_state_conflict")?;
        if accepted.await?.is_ok() || sdk.get_info(&id).await?.is_none() { return Err("failed accepted work discarded the held source".into()); }
        let recovered = value(control(&dispatcher, "fault-source-operation", Action::Cancel, Control { rollback: true, restore_spec: Some(spec.clone()), ..Default::default() }).await?)?;
        if recovered.instance_id != id || manager.execute(exec(1, "cat /source-control-data")).await?.stdout != "preserved" {
            return Err("accepted-work failure rollback lost the original root".into());
        }
        println!("PASS: timed-out accepted execution preserves source data and can roll back after drain failure");
        let frozen = value(control(&dispatcher, FIRST, Action::PrepareSnapshot, oversized.clone()).await?)?;
        if frozen.state != "frozen" || frozen.instance_id != id || manager.execute(exec(1, "must-remain-held")).await.is_ok() {
            return Err("source freeze did not preserve identity and gates".into());
        }
        let saturated = dispatcher.submit(EPOCH, SandboxPersistenceControl {
            request_id: uuid::Uuid::new_v4().to_string(), operation_id: "unrelated-source-operation".into(),
            scoped_session_id: "obx:smoke:unrelated".into(), generation: 1, action: Action::PrepareSnapshot as i32,
            deadline_unix_ms: now_ms()+30_000, payload_json: serde_json::to_vec(&oversized)?,
        }).await?;
        rejected(saturated, "no_capacity")?;
        dispatcher.close().await.map_err(|_| "old source dispatcher did not drain")?;
        manager.close_preserving().await;
        manager = TerminalSessionManager::from_config(&cfg);
        let engine = SourceControlEngine::new(manager.clone(), journal.clone(), native.clone(), settings.clone()).await
            .map_err(|_| "reconstructed source engine failed")?;
        dispatcher = ControlDispatcher::new(engine, 2).map_err(|_| "reconstructed dispatcher failed")?;
        dispatcher.begin_connection(EPOCH).await.map_err(|_| "reconstructed connection failed")?;
        manager.protect_persistence_recovery(EPOCH, &[], &[]).await?;
        manager.finish_persistence_recovery(EPOCH).await?;
        dispatcher.finish_connection(EPOCH).map_err(|_| "reconstructed recovery failed")?;
        let replayed = value(control(&dispatcher, FIRST, Action::PrepareSnapshot, oversized.clone()).await?)?;
        if replayed.archive != frozen.archive { return Err("cold source replay changed archive identity".into()); }
        let running = value(control(&dispatcher, FIRST, Action::Cancel, Control { restore_spec: Some(spec.clone()), rollback: true, ..Default::default() }).await?)?;
        if running.state != "running" || running.instance_id != id || manager.execute(exec(1, "cat /source-control-data")).await?.stdout != "preserved" { return Err("rollback did not restore original root".into()); }
        let replay = value(control(&dispatcher, FIRST, Action::Cancel, Control { rollback: true, ..Default::default() }).await?)?;
        if replay.instance_id != id { return Err("rollback replay replaced source".into()); }
        println!("PASS: management frames preserve CID through missing-claim rollback, frozen replay after manager reconstruction and VM restart");
        let frozen = value(control(&dispatcher, SECOND, Action::PrepareSnapshot, oversized).await?)?;
        let archive = frozen.archive.ok_or("archive missing")?;
        rejected(control(&dispatcher, SECOND, Action::FinalizeSuspend, Control { snapshot_committed: true, ..Default::default() }).await?, "session_state_conflict")?;
        if sdk.get_info(&id).await?.is_none() { return Err("unverified source was removed".into()); }
        let mut parts = Vec::new(); let mut offset = 0;
        while offset < archive.size_bytes {
            let size = (archive.size_bytes-offset).min(8 << 20); let number = parts.len() as i32+1;
            parts.push(UploadPart { part_number: number, grant: snapshot_store::grant(&store.url, "PUT", size, offset, Some(number)) }); offset += size;
        }
        let mut receipts = Vec::new();
        for batch in parts.chunks(16) {
            receipts.extend(value(control(&dispatcher, SECOND, Action::UploadSnapshot, Control { parts: batch.to_vec(), ..Default::default() }).await?)?.parts);
        }
        // Only the fixture coordinator completes multipart; worker frames never do.
        store.complete(&receipts, &archive)?;
        let verified = value(control(&dispatcher, SECOND, Action::VerifySnapshot, Control { archive: Some(archive.clone()), download: Some(snapshot_store::grant(&store.url, "GET", archive.size_bytes, 0, None)), ..Default::default() }).await?)?;
        if verified.state != "verified" { return Err("remote verification failed".into()); }
        rejected(control(&dispatcher, SECOND, Action::FinalizeSuspend, Control::default()).await?, "session_state_conflict")?;
        let finalized = value(control(&dispatcher, SECOND, Action::FinalizeSuspend, Control { snapshot_committed: true, ..Default::default() }).await?)?;
        let replay = value(control(&dispatcher, SECOND, Action::FinalizeSuspend, Control { snapshot_committed: true, ..Default::default() }).await?)?;
        if finalized.state != "finalized" || replay.state != "finalized" || sdk.get_info(&id).await?.is_some()
            || manager.active_session_count().await != 0 || manager.execute(exec(1, "must-remain-suspended")).await.is_ok()
        { return Err("source cleanup ACK did not revoke root and release capacity".into()); }
        let bytes = std::fs::read(root.join("journal/journal.json"))?;
        if String::from_utf8_lossy(&bytes).contains(&store.url) || String::from_utf8_lossy(&bytes).contains("x-onlyboxes-fixture") {
            return Err("ephemeral transfer grants reached the journal".into());
        }
        println!("PASS: frames upload real archive parts, full-GET verify, require commit authority and finalize with gates closed");
        Ok(())
    }.await;
    let _ = dispatcher.close().await;
    manager.close_preserving().await;
    boxlite_runtime::shutdown().await;
    if outcome.is_ok() {
        journal.close()?;
        std::fs::remove_dir_all(&root)?;
    } else {
        eprintln!("retained source-control evidence at {}", root.display());
    }
    outcome
}
