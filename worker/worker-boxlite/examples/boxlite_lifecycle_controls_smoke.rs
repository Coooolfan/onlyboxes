//! Exercise real BoxLite lifecycle actions through management frames.
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
    NativeControlConfig, NativeControlEngine, TerminalExecRequest, TerminalSessionManager,
};
use std::{
    os::unix::fs::{FileExt, MetadataExt, PermissionsExt},
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
    control_at(dispatcher, operation, 1, action, payload).await
}
async fn control_at(
    dispatcher: &Arc<ControlDispatcher>,
    operation: &str,
    generation: i64,
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
                generation,
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
    let settings = NativeControlConfig {
        max_inflight: 1,
        tmp_quota_bytes: 128 << 30,
        drain_timeout: Duration::from_secs(30),
        readiness_timeout: Duration::from_secs(30),
        cache_ttl: Duration::from_secs(300),
        cache_max_bytes: 64 << 20,
        transfer_concurrency: 4,
        transfer_idle: Duration::from_secs(30),
    };
    let engine = NativeControlEngine::new(
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
        "native management-frame acceptance; root={}",
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
        rejected(control(&dispatcher, "unknown-target-on-live-source", Action::Cancel, Control::default()).await?, "session_state_conflict")?;
        if journal.snapshot()?.operations.contains_key("unknown-target-on-live-source") {
            return Err("unknown target cancellation changed live source ownership".into());
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
        let engine = NativeControlEngine::new(manager.clone(), journal.clone(), native.clone(), settings.clone()).await
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
        let prepare = Control { restore_spec:Some(spec.clone()), archive:Some(archive.clone()),
            max_bytes:archive.size_bytes, max_expanded_bytes:archive.expanded_bytes,
            staging_bytes:archive.size_bytes + archive.expanded_bytes, lease_ttl_sec:300,
            ..Default::default() };
        let prepared = value(control_at(&dispatcher, "prepared-canceled-target", 2, Action::PrepareRestore, prepare.clone()).await?)?;
        if prepared.state != "prepared" || manager.active_session_count().await != 1
            || manager.execute(exec(2, "early-target")).await.is_ok() { return Err("prepared target did not reserve gated capacity".into()); }
        let canceled = value(control_at(&dispatcher, "prepared-canceled-target", 2, Action::Cancel, Control::default()).await?)?;
        if canceled.state != "canceled" || manager.active_session_count().await != 0 { return Err("prepared cancellation did not release capacity".into()); }
        rejected(control_at(&dispatcher, "prepared-canceled-target", 2, Action::PrepareRestore, prepare.clone()).await?, "session_state_conflict")?;
        let restored_payload = Control { archive:Some(archive.clone()),
            download:Some(snapshot_store::grant(&store.url, "GET", archive.size_bytes, 0, None)), ..Default::default() };
        value(control_at(&dispatcher, "revoked-target", 3, Action::PrepareRestore, prepare.clone()).await?)?;
        let gets_before_warm = store.get_requests.load(std::sync::atomic::Ordering::SeqCst);
        // No transfer grant at all: only a fully verified matching local cache
        // can satisfy this restore. It must still import a new writable root.
        let ready = value(control_at(&dispatcher, "revoked-target", 3, Action::Restore, Control { archive:Some(archive.clone()), ..Default::default() }).await?)?;
        if store.get_requests.load(std::sync::atomic::Ordering::SeqCst) != gets_before_warm {
            return Err("warm restore fetched the shared object".into());
        }
        let source_cache = journal.artifact_path(SCOPE, SECOND, 1)?;
        let target_cache = journal.artifact_path(SCOPE, "revoked-target", 3)?;
        let source_meta = std::fs::metadata(&source_cache)?;
        let target_meta = std::fs::metadata(&target_cache)?;
        if (source_meta.dev(), source_meta.ino()) == (target_meta.dev(), target_meta.ino())
            || source_meta.nlink() != 1 || target_meta.nlink() != 1 {
            return Err("warm restore shared a mutable archive inode across operations".into());
        }
        println!("PASS: same-node committed-cache restore uses no GET/grant and copies into a distinct operation inode before native import");
        if ready.state != "ready" || ready.instance_id == id || manager.execute(exec(3, "before-activate")).await.is_ok() {
            return Err("imported target lost identity or opened before activation".into());
        }
        let selected_process = journal.snapshot()?.operations["revoked-target"].started_process.clone();
        dispatcher.close().await.map_err(|_| "ready dispatcher did not drain")?;
        manager.close_preserving().await;
        manager = TerminalSessionManager::from_config(&cfg);
        let engine = NativeControlEngine::new(manager.clone(), journal.clone(), native.clone(), settings.clone()).await
            .map_err(|_| "ready engine reconstruction failed")?;
        dispatcher = ControlDispatcher::new(engine, 2).map_err(|_| "ready dispatcher reconstruction failed")?;
        dispatcher.begin_connection(EPOCH).await.map_err(|_| "ready connection failed")?;
        manager.protect_persistence_recovery(EPOCH, &[], &[]).await?;
        manager.finish_persistence_recovery(EPOCH).await?;
        dispatcher.finish_connection(EPOCH).map_err(|_| "ready recovery failed")?;
        let replay = value(control_at(&dispatcher, "revoked-target", 3, Action::Restore, restored_payload.clone()).await?)?;
        if replay.state != "ready" || replay.instance_id != ready.instance_id
            || replay.lease_expires_unix_ms != ready.lease_expires_unix_ms
            || journal.snapshot()?.operations["revoked-target"].started_process != selected_process
            || manager.execute(exec(3, "still-inactive")).await.is_ok() { return Err("ready replay rebooted, renewed lease or opened gates".into()); }
        let active = value(control_at(&dispatcher, "revoked-target", 3, Action::Activate, Control::default()).await?)?;
        let replay = value(control_at(&dispatcher, "revoked-target", 3, Action::Activate, Control::default()).await?)?;
        if active.state != "active" || replay.instance_id != ready.instance_id
            || manager.execute(exec(3, "cat /source-control-data")).await?.stdout != "preserved"
            || manager.execute(exec(1, "stale-generation")).await.is_ok() { return Err("explicit activation lost restored data or fencing".into()); }
        rejected(control_at(&dispatcher, "revoked-target", 3, Action::Cancel, Control::default()).await?, "session_state_conflict")?;
        let accepted = tokio::spawn({ let manager = manager.clone(); async move {
            manager.execute(exec(3, "python -c \"from pathlib import Path; Path('/revoke-exec-started').write_text('yes'); import time; time.sleep(1); print('finished')\"")).await
        }});
        let observed = async {
            loop {
                let probe = sdk.get(&ready.instance_id).await?.ok_or("target missing before revocation")?
                    .exec(boxlite::BoxCommand::new("sh").args(["-c", "test -f /revoke-exec-started"])).await?.wait().await?;
                if probe.exit_code == 0 { return Ok::<(), Box<dyn std::error::Error>>(()); }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        };
        tokio::time::timeout(Duration::from_secs(5), observed).await??;
        let revoked = value(control_at(&dispatcher, "revoked-target", 3, Action::Cancel, Control { delete:true, ..Default::default() }).await?)?;
        if accepted.await??.stdout.trim() != "finished" { return Err("revocation removed the target before accepted execution drained".into()); }

        if revoked.state != "canceled" || sdk.get_info(&ready.instance_id).await?.is_some()
            || manager.active_session_count().await != 0 || manager.execute(exec(3, "after-revoke")).await.is_ok() {
            return Err("explicit target revocation retained execution or root".into());
        }
        println!("PASS: restore frames import verified bytes, preserve inactive readiness across manager reconstruction and require explicit activation/revocation");
        value(control_at(&dispatcher, "permanently-deleted-target", 4, Action::PrepareRestore, prepare.clone()).await?)?;
        let mut corrupted = 0;
        for prior in journal.snapshot()?.operations.values().filter(|op| op.scoped_session_id == SCOPE && op.verified
            && ((op.kind == worker_boxlite::persistence::Kind::Source && op.snapshot_committed)
                || (op.kind == worker_boxlite::persistence::Kind::Target && op.phase == worker_boxlite::persistence::Phase::Canceled))) {
            let path = journal.artifact_path(SCOPE, &prior.operation_id, prior.generation)?;
            if !path.exists() { continue; }
            let file = std::fs::OpenOptions::new().read(true).write(true).open(path)?;
            let mut byte = [0]; file.read_exact_at(&mut byte, 0)?; byte[0] ^= 1;
            file.write_all_at(&byte, 0)?; file.sync_all()?; corrupted += 1;
        }
        if corrupted == 0 { return Err("corrupt-cache acceptance had no retained cache candidate".into()); }
        let gets_before_cold = store.get_requests.load(std::sync::atomic::Ordering::SeqCst);
        let cold_payload = Control { archive:Some(archive.clone()),
            download:Some(snapshot_store::grant(&store.url, "GET", archive.size_bytes, 0, None)), ..Default::default() };
        let final_ready = value(control_at(&dispatcher, "permanently-deleted-target", 4, Action::Restore, cold_payload).await?)?;
        if store.get_requests.load(std::sync::atomic::Ordering::SeqCst) != gets_before_cold+1 {
            return Err("corrupt caches did not fall back to exactly one complete shared GET".into());
        }
        println!("PASS: corrupted same-node caches are rejected and restore downloads the same committed reference before starting");
        value(control_at(&dispatcher, "permanently-deleted-target", 4, Action::Activate, Control::default()).await?)?;
        if manager.execute(exec(4, "cat /source-control-data")).await?.stdout != "preserved" { return Err("second restore lost snapshot bytes".into()); }
        let purge = Control { purge_session:true, restore_spec:Some(spec.clone()), ..Default::default() };
        let deleted = value(control_at(&dispatcher, "permanent-session-deletion", 5, Action::Cancel, purge.clone()).await?)?;
        let replay = value(control_at(&dispatcher, "permanent-session-deletion", 5, Action::Cancel, purge).await?)?;
        if deleted.state != "canceled" || replay.state != "canceled"
            || sdk.get_info(&final_ready.instance_id).await?.is_some() || manager.active_session_count().await != 0
            || manager.execute(exec(5, "after-permanent-delete")).await.is_ok()
            || journal.artifact_path(SCOPE, SECOND, 1)?.exists()
            || journal.artifact_path(SCOPE, "revoked-target", 3)?.exists()
            || journal.artifact_path(SCOPE, "permanently-deleted-target", 4)?.exists() {
            return Err("permanent deletion did not remove every root/cache and gate".into());
        }
        rejected(control_at(&dispatcher, "forbidden-future-restore", 6, Action::PrepareRestore, prepare).await?, "session_state_conflict")?;
        println!("PASS: permanent-delete frames remove a running target and all retained session archives; replay is idempotent and future recreation stays fenced");

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
