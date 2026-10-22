//! Exercise the actual Worker manager, backend and native snapshot primitives.
//! Local coordinator records are test fixtures, not Console transaction proof.
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
use runner::terminal_session_manager::{TerminalExecRequest, TerminalSessionManager};
use std::os::unix::fs::PermissionsExt;
use std::{
    collections::BTreeMap,
    path::Path,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use worker_boxlite::persistence::{
    ArchiveLimits, Journal, Kind, NativeRuntime, Operation, Phase, Transfer, UploadPart,
};

type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
const SCOPE: &str = "obx:smoke:manager";
const SOURCE: &str = "source-operation";
const ROLLBACK: &str = "rollback-operation";
const TARGET: &str = "restore-operation";
const CONNECTION: &str = "coordinator-epoch-1";
const IMAGE: &str =
    "python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258";
fn now_ms() -> Result<i64> {
    Ok(SystemTime::now().duration_since(UNIX_EPOCH)?.as_millis() as i64)
}
fn request(generation: i64, command: &str) -> TerminalExecRequest {
    TerminalExecRequest {
        session_id: SCOPE.into(),
        generation,
        connection_id: Some(CONNECTION.into()),
        command: command.into(),
        create_if_missing: true,
        lease_ttl_sec: Some(300),
        deadline_unix_ms: 0,
    }
}
async fn connect(manager: &TerminalSessionManager, epoch: &str) -> Result<()> {
    manager
        .protect_persistence_recovery(epoch, &[], &[])
        .await?;
    manager.finish_persistence_recovery(epoch).await?;
    Ok(())
}
fn private_directory(path: &Path) -> Result<()> {
    std::fs::create_dir(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}
#[tokio::main]
async fn main() -> Result<()> {
    let root = std::env::temp_dir().join(format!(
        "onlyboxes-manager-snapshot-{}",
        uuid::Uuid::new_v4()
    ));
    private_directory(&root)?;
    let home = root.join("sdk-home");
    private_directory(&home)?;
    let mut cfg = config::Config::load();
    cfg.worker_id = "smoke-worker".into();
    cfg.worker_secret = "smoke-only".into();
    cfg.boxlite_home = home.to_str().ok_or("non-UTF8 home")?.into();
    cfg.terminal_exec_image = IMAGE.into();
    cfg.terminal_max_active_sessions = 1;
    cfg.proxy_enabled = false;
    cfg.proxy_sandbox_ports = vec![8080];
    let journal = Arc::new(Journal::open(root.join("journal"), "smoke-worker")?);
    let manager = TerminalSessionManager::from_config(&cfg);
    manager.install_snapshot_journal(journal.clone()).await?;
    connect(&manager, CONNECTION).await?;
    let store = snapshot_store::Store::new(&root.join("object-service")).await?;
    let limits = ArchiveLimits {
        max_bytes: 256 << 20,
        max_expanded_bytes: 64 << 30,
    };
    println!(
        "manager/native admission acceptance; root={}",
        root.display()
    );
    let result: Result<()> = async {
        manager.execute(request(0,"python -c \"from pathlib import Path; Path('/onlyboxes-manager-data').write_text('preserved')\"")).await?;
        let runtime = Arc::new(boxlite_runtime::runtime(&cfg)?.clone());
        let native = NativeRuntime::new(runtime.clone(), &home)?;
        let source = runtime
            .get(&worker_boxlite::persistence::terminal_name(SCOPE))
            .await?
            .ok_or("source missing")?;
        let source_id = source.id().as_str().to_owned();
        let inspected = native.inspect(SCOPE, &source_id).await?;
        journal.probe(SCOPE, &source_id)?;
        manager.install_snapshot_journal(journal.clone()).await?;
        connect(&manager, CONNECTION).await?;
        manager.hold_snapshot_source(SCOPE, ROLLBACK, 1).await?;
        journal.claim(Operation {
            operation_id: ROLLBACK.into(),
            scoped_session_id: SCOPE.into(),
            generation: 1,
            kind: Kind::Source,
            phase: Phase::Preparing,
            restore_spec: Some(inspected.spec.clone()),
            archive: None,
            artifact: String::new(),
            box_id: source_id.clone(),
            tombstone_box_id: String::new(),
            name_fences: Default::default(),
            cleanup_processes: Default::default(),
            cleanup_box_ids: Default::default(),
            cleanup_names: Default::default(),
        lease_retirements: Default::default(),
            source_process: None,
            source_stop_confirmed: false,
            start_attempted: false,
            started_process: None,
            boot_executable: None,
        boot_guard: None,
            lease_expires_unix_ms: now_ms()? + 300_000,
            staging_bytes: 65 << 30,
            snapshot_committed: false,
            verified: false,
            remote_verified: false,
            cache_until_unix_ms: 0,
            lease_ttl_sec: 0,
        })?;
        manager.sync_snapshot_operation(ROLLBACK).await?;
        manager.drain_snapshot_source(SCOPE, ROLLBACK, 1).await?;
        // Roll back after native stop: the original host process must be proven
        // dead before restarting the SAME disk/ID. Console activation is separate.
        native.stop_source(&journal, ROLLBACK).await?;
        let rollback = native.rollback_source(&journal, ROLLBACK, now_ms()? + 300_000, Duration::from_secs(30)).await?;
        if rollback.box_id != source_id {
            return Err("rollback replaced original root".into());
        }
        native.rollback_source(&journal, ROLLBACK, now_ms()? + 300_000, Duration::from_secs(30)).await?;
        manager.sync_snapshot_operation(ROLLBACK).await?;
        manager.bind_snapshot_instance(ROLLBACK).await?;
        if manager.execute(request(1, "must-remain-gated")).await.is_ok() {
            return Err("rollback opened admission before activation".into());
        }
        manager.activate_snapshot_instance(CONNECTION, SCOPE, ROLLBACK, 1).await?;
        if manager.execute(request(1, "cat /onlyboxes-manager-data")).await?.stdout != "preserved" {
            return Err("rollback changed source filesystem".into());
        }
        manager.hold_snapshot_source(SCOPE, SOURCE, 1).await?;
        let mut next = journal.snapshot()?.operations[ROLLBACK].clone();
        next.operation_id = SOURCE.into();
        next.phase = Phase::Preparing;
        next.source_process = None;
        next.source_stop_confirmed = false;
        next.start_attempted = false;
        next.started_process = None;
        next.cleanup_box_ids.clear();
        next.cleanup_names.clear();
        next.cleanup_processes.clear();
        next.name_fences.clear();
        journal.claim(next)?;
        manager.sync_snapshot_operation(SOURCE).await?;
        if manager.execute(request(1, "must-not-run")).await.is_ok() {
            return Err("source gate admitted execution".into());
        }
        manager.drain_snapshot_source(SCOPE, SOURCE, 1).await?;
        let archive = native
            .export_source(journal.clone(), SOURCE, limits)
            .await?;
        if native.finalize_source(&journal, SOURCE).await.is_ok()
            || runtime.get_info(&source_id).await?.is_none()
        { return Err("uncommitted source was reclaimed".into()); }
        let transfer = Transfer::new(4, Duration::from_secs(30))?;
        let file = std::fs::File::open(journal.artifact_path(SCOPE, SOURCE, 1)?)?;
        let pinned = transfer
            .source(
                file,
                archive.clone(),
                tokio_util::sync::CancellationToken::new(),
            )
            .await?;
        let mut parts = vec![];
        let mut offset = 0;
        while offset < archive.size_bytes {
            let size = (archive.size_bytes - offset).min(8 << 20);
            let number = parts.len() as i32 + 1;
            parts.push(UploadPart {
                part_number: number,
                grant: snapshot_store::grant(&store.url, "PUT", size, offset, Some(number)),
            });
            offset += size;
        }
        let mut receipts = vec![];
        for batch in parts.chunks(16) {
            receipts.extend(
                transfer
                    .upload(&pinned, batch, tokio_util::sync::CancellationToken::new())
                    .await?,
            );
        }
        store.complete(&receipts, &archive)?;
        transfer
            .verify_remote(
                &snapshot_store::grant(&store.url, "GET", archive.size_bytes, 0, None),
                &archive,
                tokio_util::sync::CancellationToken::new(),
            )
            .await?;
        drop(pinned);
        // This fixture coordinator models commit only after actual readback. It
        // does not exercise the production Console manifest/database transaction.
        journal.update(SOURCE, |op| {
            op.remote_verified = true;
            op.snapshot_committed = true;
            op.phase = Phase::Committed;
            Ok(())
        })?;
        native.finalize_source(&journal, SOURCE).await?;
        native.finalize_source(&journal, SOURCE).await?;
        boxlite_runtime::remove_box(&cfg, &source_id).await;
        if journal.snapshot()?.operations[SOURCE].name_fences.len() != 1 {
            return Err("finalized source has no native name reservation".into());
        }
        if runtime.create(boxlite::BoxOptions::default(), Some(worker_boxlite::persistence::terminal_name(SCOPE))).await.is_ok() {
            return Err("late create replaced finalized source name".into());
        }

        manager.sync_snapshot_operation(SOURCE).await?;
        manager.detach_snapshot_instance(SCOPE, SOURCE, 1).await?;
        if runtime.get(&source_id).await?.is_some() {
            return Err("source cleanup did not remove selected ID".into());
        }
        println!(
            "PASS: manager source gate, host exit proof, upload/readback and native source cleanup"
        );
        manager.reserve_snapshot_target(SCOPE, TARGET, 2).await?;
        journal.claim(Operation {
            operation_id: TARGET.into(),
            scoped_session_id: SCOPE.into(),
            generation: 2,
            kind: Kind::Target,
            phase: Phase::Prepared,
            restore_spec: Some(inspected.spec),
            archive: Some(archive.clone()),
            artifact: worker_boxlite::persistence::artifact_name(SCOPE, TARGET, 2),
            box_id: String::new(),
            tombstone_box_id: String::new(),
            name_fences: Default::default(),
            cleanup_processes: Default::default(),
            cleanup_box_ids: Default::default(),
            cleanup_names: Default::default(),
        lease_retirements: Default::default(),
            source_process: None,
            source_stop_confirmed: false,
            start_attempted: false,
            started_process: None,
            boot_executable: None,
        boot_guard: None,
            lease_expires_unix_ms: 0,
            staging_bytes: 65 << 30,
            snapshot_committed: false,
            verified: false,
            remote_verified: false,
            cache_until_unix_ms: 0,
            lease_ttl_sec: 300,
        })?;
        drop(
            transfer
                .download(
                    &snapshot_store::grant(&store.url, "GET", archive.size_bytes, 0, None),
                    &archive,
                    journal.artifact_path(SCOPE, TARGET, 2)?,
                    tokio_util::sync::CancellationToken::new(),
                )
                .await?,
        );
        let imported = native
            .import_target(journal.clone(), TARGET, limits)
            .await?;
        // Expired authority must not record an intent or start a writer.
        if native.start_target(&journal, TARGET, now_ms()? - 1, Duration::from_secs(30)).await.is_ok()
            || journal.snapshot()?.operations[TARGET].start_attempted
        {
            return Err("expired lease started target".into());
        }
        // Simulate a crash after durable intent and before SDK acknowledgement.
        // This independent journal is a test fixture for the same stopped ID.
        let uncertain = Journal::open(root.join("uncertain-boot-fixture"), "smoke-worker")?;
        let mut pending = journal.snapshot()?.operations[TARGET].clone();
        pending.phase = Phase::Prepared;
        pending.verified = false;
        uncertain.claim(pending)?;
        uncertain.update(TARGET, |op| {
            op.phase = Phase::Importing;
            op.verified = true;
            op.start_attempted = true;
            op.lease_expires_unix_ms = now_ms().map_err(|_| worker_boxlite::persistence::Error::Unavailable)? + 300_000;
            Ok(())
        })?;
        uncertain.close()?;
        let uncertain = Journal::open(root.join("uncertain-boot-fixture"), "smoke-worker")?;
        if native.start_target(&uncertain, TARGET, now_ms()? + 300_000, Duration::from_secs(30)).await.is_ok()
            || native.inspect_target(&journal, TARGET).await?.status != boxlite::BoxStatus::Stopped
        {
            return Err("uncertain boot replay started a second writer".into());
        }
        if native.cancel_target(&uncertain, TARGET).await.is_ok()
            || runtime.get_info(&imported.box_id).await?.is_none()
        { return Err("uncertain boot was removed without process evidence".into()); }
        uncertain.close()?;
        // A separate inactive target exercises Ready cancellation and a lost
        // database-first deletion ACK, without assigning it any public owner.
        let cancel_id = "cancel-restore-operation";
        let cancel_root = root.join("cancel-target-fixture");
        let cancel_journal = Arc::new(Journal::open(&cancel_root, "smoke-worker")?);
        let mut cancel_op = journal.snapshot()?.operations[TARGET].clone();
        cancel_op.operation_id = cancel_id.into();
        cancel_op.phase = Phase::Prepared;
        cancel_op.box_id.clear();
        cancel_op.verified = false;
        cancel_op.artifact = worker_boxlite::persistence::artifact_name(SCOPE, cancel_id, 2);
        cancel_journal.claim(cancel_op)?;
        std::fs::copy(journal.artifact_path(SCOPE, TARGET, 2)?, cancel_journal.artifact_path(SCOPE, cancel_id, 2)?)?;
        let canceled = native.import_target(cancel_journal.clone(), cancel_id, limits).await?;
        native.start_target(&cancel_journal, cancel_id, now_ms()? + 300_000, Duration::from_secs(30)).await?;
        native.cancel_target(&cancel_journal, cancel_id).await?;
        let canceled_state = cancel_journal.snapshot()?.operations[cancel_id].clone();
        if canceled_state.phase != Phase::Canceled || canceled_state.cleanup_processes.is_empty()
            || canceled_state.name_fences.len() != 1 || runtime.get(&canceled.box_id).await?.is_some()
        { return Err("canceled target retained a native writer or lost cleanup evidence".into()); }
        // Model SDK's database-first removal with leftover physical contents.
        let residue = home.join("boxes").join(&canceled.box_id);
        private_directory(&residue)?;
        std::fs::write(residue.join("orphaned-disk"), b"owned cleanup residue")?;
        cancel_journal.close()?;
        let cancel_journal = Journal::open(&cancel_root, "smoke-worker")?;
        native.cancel_target(&cancel_journal, cancel_id).await?;
        if residue.exists() { return Err("cleanup replay ignored native directory residue".into()); }
        let canceled_name = worker_boxlite::persistence::target_name(SCOPE, cancel_id, 2);
        if runtime.import_box(boxlite::BoxArchive::new(journal.artifact_path(SCOPE, TARGET, 2)?), Some(canceled_name)).await.is_ok()
            || cancel_journal.snapshot()?.operations[cancel_id].name_fences != canceled_state.name_fences
        { return Err("late import replaced canceled target name".into()); }
        cancel_journal.close()?;
        native.start_target(&journal, TARGET, now_ms()? + 300_000, Duration::from_secs(30)).await?;
        // Lost readiness ACK replay must keep the same running host process.
        let first_process = journal.snapshot()?.operations[TARGET].started_process.clone();
        native.start_target(&journal, TARGET, now_ms()? + 300_000, Duration::from_secs(30)).await?;
        if journal.snapshot()?.operations[TARGET].started_process != first_process {
            return Err("readiness replay replaced running process".into());
        }
        let target = runtime.get(&imported.box_id).await?.ok_or("import missing")?;
        boxlite_runtime::recover_pinned_terminal_session_box(&cfg, &imported.box_id)
            .await?
            .ok_or("target not running")?;
        let probe = boxlite_runtime::exec_terminal_shell(
            &cfg,
            &imported.box_id,
            "cat /onlyboxes-manager-data",
            now_ms()? + 10_000,
        )
        .await?;
        if probe.stdout != "preserved" {
            return Err("restored filesystem content changed".into());
        }
        manager.sync_snapshot_operation(TARGET).await?;
        manager.bind_snapshot_instance(TARGET).await?;
        if manager.active_session_count().await != 1
            || manager.execute(request(2, "early")).await.is_ok()
            || manager
                .claim_proxy(SCOPE, 8080, 2, SystemTime::now())
                .await
                .is_ok()
        {
            return Err("ready target opened admission or double-counted capacity".into());
        }
        journal.update(TARGET, |op| {
            op.phase = Phase::Active;
            Ok(())
        })?;
        manager.sync_snapshot_operation(TARGET).await?;
        if manager.execute(request(2, "before-activate")).await.is_ok() {
            return Err("journal active flag bypassed activation".into());
        }
        manager
            .activate_snapshot_instance(CONNECTION, SCOPE, TARGET, 2)
            .await?;
        let read = manager
            .execute(request(2, "cat /onlyboxes-manager-data"))
            .await?;
        if read.stdout != "preserved" || read.created {
            return Err("manager did not bind the imported root".into());
        }
        for generation in [0, 1] {
            if manager.execute(request(generation, "stale")).await.is_ok() {
                return Err("stale generation executed".into());
            }
        }
        if native.cancel_target(&journal, TARGET).await.is_ok()
            || runtime.get_info(&imported.box_id).await?.is_none()
        { return Err("late cancellation removed activated target".into()); }
        println!("PASS: restored root remains inactive until activation, then executes through its selected ID");
        manager.close_preserving().await;
        let recovered = TerminalSessionManager::from_config(&cfg);
        recovered.install_snapshot_journal(journal.clone()).await?;
        recovered
            .protect_persistence_recovery("coordinator-epoch-2", &[], &[])
            .await?;
        let candidate = proto::registryv1::TerminalSessionRecoveryCandidate {
            session_id: SCOPE.into(),
            generation: 2,
            lease_expires_unix_ms: journal.snapshot()?.operations[TARGET].lease_expires_unix_ms,
        };
        let report = recovered
            .recover_sessions(std::slice::from_ref(&candidate))
            .await;
        if report[0].status
            != proto::registryv1::terminal_session_recovery_result::Status::Recovered as i32
        {
            return Err("pinned running root was not recovered".into());
        }
        recovered
            .finish_persistence_recovery("coordinator-epoch-2")
            .await?;
        let mut current = request(2, "cat /onlyboxes-manager-data");
        current.connection_id = Some("coordinator-epoch-2".into());
        if recovered.execute(current).await?.stdout != "preserved" {
            return Err("manager reconstruction changed root".into());
        }
        recovered.close_preserving().await;
        target.stop().await?;
        if native.start_target(&journal, TARGET, now_ms()? + 300_000, Duration::from_secs(30)).await.is_ok()
            || target.info().await?.status != boxlite::BoxStatus::Stopped
        {
            return Err("ready/active retry rebooted a stopped managed disk".into());
        }
        let stopped = TerminalSessionManager::from_config(&cfg);
        stopped.install_snapshot_journal(journal.clone()).await?;
        stopped
            .protect_persistence_recovery("coordinator-epoch-3", &[], &[])
            .await?;
        let report = stopped.recover_sessions(&[candidate]).await;
        if report[0].status
            != proto::registryv1::terminal_session_recovery_result::Status::Invalid as i32
            || target.info().await?.status != boxlite::BoxStatus::Stopped
        {
            return Err("ordinary recovery eagerly started a lifecycle-owned root".into());
        }
        stopped.close_preserving().await;
        let other_scope = "obx:smoke:unrelated";
        let mut unrelated_op = journal.snapshot()?.operations[TARGET].clone();
        unrelated_op.operation_id = "unrelated-cache-operation".into();
        unrelated_op.scoped_session_id = other_scope.into();
        unrelated_op.phase = Phase::Prepared;
        unrelated_op.box_id.clear();
        unrelated_op.verified = false;
        unrelated_op.start_attempted = false;
        unrelated_op.started_process = None;
        unrelated_op.lease_expires_unix_ms = 0;
        unrelated_op.artifact = worker_boxlite::persistence::artifact_name(other_scope, &unrelated_op.operation_id, 2);
        journal.claim(unrelated_op)?;
        let other_cache = journal.artifact_path(other_scope, "unrelated-cache-operation", 2)?;
        std::fs::write(&other_cache, b"unrelated cache must remain")?;
        std::fs::set_permissions(&other_cache, std::fs::Permissions::from_mode(0o600))?;
        let unrelated_native = runtime.create(boxlite::BoxOptions::default(), Some(worker_boxlite::persistence::terminal_name(other_scope))).await?;
        let delete: Operation = serde_json::from_value(serde_json::json!({
            "operation_id": "delete-session-operation", "scoped_session_id": SCOPE,
            "generation": 3, "kind": "delete", "phase": "deleting",
            "restore_spec": journal.snapshot()?.operations[TARGET].restore_spec,
            "box_id": imported.box_id
        }))?;
        journal.claim(delete)?;
        // Model an accepted SDK start completing after the delete intent. The
        // caller joins it while gates stay closed, then purge must stop its new
        // lifecycle as well as validate every older recorded process proof.
        if !journal.snapshot()?.operations[TARGET].started_process.as_ref().ok_or("missing previous process")?.has_exited()? {
            return Err("prior target process still owns the disk".into());
        }
        let late = runtime.get(&imported.box_id).await?.ok_or("target missing before late start")?;
        late.start().await?;
        native.purge_session(journal.clone(), "delete-session-operation").await?;
        let deleted = journal.snapshot()?.operations["delete-session-operation"].clone();
        if deleted.phase != Phase::Deleted || deleted.name_fences.len() != 2
            || deleted.tombstone_box_id != journal.snapshot()?.operations[SOURCE].name_fences[&worker_boxlite::persistence::terminal_name(SCOPE)]
            || runtime.get_info(&imported.box_id).await?.is_some()
            || journal.artifact_path(SCOPE, SOURCE, 1)?.exists() || journal.artifact_path(SCOPE, TARGET, 2)?.exists()
            || std::fs::read(&other_cache)? != b"unrelated cache must remain"
            || runtime.get_info(unrelated_native.id().as_str()).await?.is_none()
        { return Err("permanent deletion lost name fences or crossed scope boundaries".into()); }
        native.purge_session(journal.clone(), "delete-session-operation").await?;
        if runtime.create(boxlite::BoxOptions::default(), Some(worker_boxlite::persistence::terminal_name(SCOPE))).await.is_ok() {
            return Err("deleted session name was recreated".into());
        }
        // Losing an acknowledged marker must not silently replace its ID.
        runtime.remove(&deleted.tombstone_box_id, false).await?;
        if native.purge_session(journal.clone(), "delete-session-operation").await.is_ok()
            || runtime.get_info(&worker_boxlite::persistence::terminal_name(SCOPE)).await?.is_some()
        { return Err("missing permanent name fence was silently replaced".into()); }
        runtime.remove(unrelated_native.id().as_str(), false).await?;
        boxlite_runtime::remove_box(&cfg, &imported.box_id).await;
        println!("PASS: permanent deletion reuses old markers, removes all recorded roots/caches and preserves unrelated scopes");
        println!("PASS: reconstructed manager recovers exact running ID and never eagerly starts stopped managed roots");
        Ok(())
    }.await;
    manager.close_preserving().await;
    boxlite_runtime::shutdown().await;
    if result.is_ok() {
        journal.close()?;
        std::fs::remove_dir_all(&root)?;
    } else {
        eprintln!("retained acceptance artifacts at {}", root.display());
    }
    result
}
