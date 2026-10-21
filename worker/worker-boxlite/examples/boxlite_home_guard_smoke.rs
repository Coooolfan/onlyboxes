//! Real SDK reopening after an owned child is killed; pre-SDK refusals preserve
//! orphan data and exact native stop after issuer loss. This does not validate
//! an independently supervised lease service or expiry scheduling.
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
use serde::{Deserialize, Serialize};
use std::{os::unix::fs::PermissionsExt, path::Path, sync::Arc, time::Duration};
use worker_boxlite::persistence::{terminal_name, RuntimeHomeGuard};
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
const WORKER: &str = "home-guard-fixture-worker";
const SCOPE: &str = "obx:fixture:guarded-home";
const DELETE: &str = "fixture-permanent-delete";
#[derive(Serialize, Deserialize)]
struct Record {
    id: String,
    pid: u32,
    start_time: Option<u64>,
    writer: worker_boxlite::persistence::HostProcess,
}
fn private(path: &Path) -> Result<()> {
    std::fs::create_dir(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}
async fn child(mode: &str, root: &Path) -> Result<()> {
    let home = root.join("sdk");
    let state = root.join("state");
    if mode == "reject-unguarded" {
        let mut cfg = config::Config::load();
        cfg.boxlite_home = home.to_str().ok_or("invalid test home")?.into();
        if boxlite_runtime::runtime(&cfg).is_ok() {
            return Err("marked home bypassed guarded startup".into());
        }
        return Ok(());
    }
    let owner = RuntimeHomeGuard::open(&home, &state, WORKER);
    if mode == "reject-orphan" {
        if owner.is_ok() {
            return Err("orphan home was accepted before SDK initialization".into());
        }
        if std::fs::read(home.join("boxes/unrecorded-root/disk.qcow2"))?
            != b"unique-owned-test-data"
        {
            return Err("preflight refusal changed orphan data".into());
        }
        return Ok(());
    }
    let owner = Arc::new(owner?);
    let journal = owner.journal();
    let mut cfg = config::Config::load();
    cfg.worker_id = WORKER.into();
    cfg.worker_secret = "fixture".into();
    cfg.boxlite_home = home.to_str().ok_or("invalid test path")?.into();
    if mode == "recover-cleanup" {
        let record: Record = serde_json::from_slice(&std::fs::read(root.join("ready.json"))?)?;
        if home.join("boxes").join(&record.id).exists()
            || journal.snapshot()?.operations[DELETE].phase
                != worker_boxlite::persistence::Phase::Deleting
        {
            return Err("startup did not reclaim authorized residue before SDK open, or prematurely completed the operation".into());
        }
    }
    let sdk = boxlite_runtime::initialize_guarded(&cfg, owner.clone())?;
    if boxlite_runtime::initialize_guarded(&cfg, owner).is_ok() {
        return Err("late guarded initialization was accepted".into());
    }
    if matches!(mode, "cleanup" | "cleanup-residue" | "recover-cleanup") {
        let record: Record = serde_json::from_slice(&std::fs::read(root.join("ready.json"))?)?;
        let native = worker_boxlite::persistence::NativeRuntime::new(Arc::new(sdk.clone()), &home)?;
        if !journal.snapshot()?.operations.contains_key(DELETE) {
            let inspected = native.inspect(SCOPE, &record.id).await?;
            journal.claim(serde_json::from_value(serde_json::json!({
                "operation_id":DELETE,"scoped_session_id":SCOPE,"generation":2,"kind":"delete","phase":"deleting",
                "restore_spec":inspected.spec,"box_id":record.id,
                "cleanup_box_ids":[record.id],"cleanup_names":{&record.id:terminal_name(SCOPE)},
                "cleanup_processes":{&record.id:record.writer},
            }))?)?;
        }
        let before = journal.snapshot()?.operations[DELETE].name_fences.clone();
        native.purge_session(journal.clone(), DELETE).await?;
        if mode == "recover-cleanup" && before != journal.snapshot()?.operations[DELETE].name_fences
        {
            return Err("cleanup replay replaced a recorded native name fence".into());
        }
        if mode == "cleanup-residue" {
            boxlite_runtime::shutdown().await;
            journal.close()?;
            // Fault model: retain real cleanup IDs/exit proofs from this VM,
            // reconstruct the database-first removal window in durable state,
            // and leave only the physical residue that SDK removal can miss.
            let path = state.join("journal.json");
            let mut value: serde_json::Value = serde_json::from_slice(&std::fs::read(&path)?)?;
            value["operations"][DELETE]["phase"] = "deleting".into();
            use std::io::Write;
            use std::os::unix::fs::OpenOptionsExt;
            let pending = state.join("fixture-recovery.pending");
            let mut file = std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(&pending)?;
            file.write_all(&serde_json::to_vec(&value)?)?;
            file.sync_all()?;
            drop(file);
            std::fs::rename(pending, &path)?;
            std::fs::File::open(&state)?.sync_all()?;
            let residue = home.join("boxes").join(&record.id);
            private(&residue)?;
            std::fs::write(residue.join("disk.qcow2"), b"physical removal residue")?;
            std::fs::File::open(home.join("boxes"))?.sync_all()?;
        }
        return Ok(());
    }
    if mode == "create" {
        let mut options = boxlite::BoxOptions {
            cpus: Some(1),
            memory_mib: Some(256),
            auto_delete: Some(0),
            detach: true,
            rootfs: boxlite::RootfsSpec::Image(
                "python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258"
                    .into(),
            ),
            entrypoint: Some(vec!["sh".into(), "-c".into()]),
            cmd: Some(vec!["while true; do sleep 3600; done".into()]),
            ..Default::default()
        };
        options.advanced.security.resource_limits.max_processes = Some(128);
        let native = sdk.create(options, Some(terminal_name(SCOPE))).await?;
        native.start().await?;
        let execution = native
            .exec(
                boxlite::BoxCommand::new("sh")
                    .args(["-c", "printf preserved > /home-guard-data; sync"]),
            )
            .await?
            .wait()
            .await?;
        if execution.exit_code != 0 {
            return Err("test data write failed".into());
        }
        let info = native.info().await?;
        let id = info.id.to_string();
        journal.probe(SCOPE, &id)?;
        let pid = boxlite::util::PidRecord::decode(&std::fs::read(
            home.join("boxes").join(&id).join("shim.pid"),
        )?)?;
        let record = Record {
            writer: worker_boxlite::persistence::NativeRuntime::new(Arc::new(sdk.clone()), &home)?
                .inspect_writer(SCOPE, &id)
                .await?,
            id,
            pid: info.pid.ok_or("running PID missing")?,
            start_time: pid.start_time,
        };
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        let mut output = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(root.join("ready.pending"))?;
        output.write_all(&serde_json::to_vec(&record)?)?;
        output.sync_all()?;
        drop(output);
        std::fs::rename(root.join("ready.pending"), root.join("ready.json"))?;
        std::fs::File::open(root)?.sync_all()?;
        // Parent kills this exact owned child after it sees the record.
        std::future::pending::<()>().await;
    } else {
        let record: Record = serde_json::from_slice(&std::fs::read(root.join("ready.json"))?)?;
        let native = sdk
            .get(&record.id)
            .await?
            .ok_or("pinned root missing after restart")?;
        let info = native.info().await?;
        let pid = boxlite::util::PidRecord::decode(&std::fs::read(
            home.join("boxes").join(&record.id).join("shim.pid"),
        )?)?;
        if info.status != boxlite::BoxStatus::Running
            || info.pid != Some(record.pid)
            || pid.start_time != record.start_time
        {
            return Err("guarded restart replaced or rebooted the original VM".into());
        }
        let read = native
            .exec(
                boxlite::BoxCommand::new("sh")
                    .args(["-c", "test \"$(cat /home-guard-data)\" = preserved"]),
            )
            .await?
            .wait()
            .await?;
        if read.exit_code != 0 {
            return Err("guarded restart lost test data".into());
        }
        if mode == "cleanup" {
            native.stop().await?;
            sdk.remove(&record.id, false).await?;
        }
        // SDK runtime is held statically; exit leaves detached boxes untouched.
    }
    Ok(())
}
fn run(exe: &Path, mode: &str, root: &Path) -> Result<()> {
    let status = std::process::Command::new(exe)
        .arg(mode)
        .arg(root)
        .status()?;
    if !status.success() {
        return Err(format!("guard smoke phase failed: {mode}").into());
    }
    Ok(())
}
#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = std::env::args_os().collect();
    if args.len() == 3 {
        return child(args[1].to_str().ok_or("invalid mode")?, Path::new(&args[2])).await;
    }
    let root = std::env::temp_dir().join(format!(
        "onlyboxes-home-guard-native-{}",
        uuid::Uuid::new_v4()
    ));
    private(&root)?;
    private(&root.join("sdk"))?;
    let exe = std::env::current_exe()?;
    let mut worker = std::process::Command::new(&exe)
        .arg("create")
        .arg(&root)
        .spawn()?;
    let started = std::time::Instant::now();
    let result:Result<()>=async {
        while !root.join("ready.json").exists() {
            if worker.try_wait()?.is_some() { return Err("owned SDK child exited before readiness".into()); }
            if started.elapsed()>Duration::from_secs(120) { return Err("owned SDK child did not become ready".into()); }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        worker.kill()?; worker.wait()?;
        run(&exe,"reject-unguarded",&root)?;
        run(&exe,"reopen",&root)?;
        println!("PASS: a fresh process opens the paired journal/home before SDK recovery and reuses the same VM PID/start-time and filesystem after Worker SIGKILL");
        let orphan=root.join("sdk/boxes/unrecorded-root"); private(&orphan)?;
        std::fs::write(orphan.join("disk.qcow2"),b"unique-owned-test-data")?;
        run(&exe,"reject-orphan",&root)?;
        println!("PASS: startup census rejects an orphan before SDK cleanup and preserves its bytes");
        std::fs::remove_dir_all(&orphan)?;
        let record:Record=serde_json::from_slice(&std::fs::read(root.join("ready.json"))?)?;
        record.writer.request_writer_stop()?;
        let deadline=std::time::Instant::now()+Duration::from_secs(10);
        while !record.writer.writers_exited()? {
            if std::time::Instant::now()>=deadline {return Err("native writer did not stop independently of the dead SDK issuer".into());}
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        if !root.join("sdk/boxes").join(&record.id).join("disks/disk.qcow2").is_file() {return Err("exact writer stop removed the retained guest disk".into());}
        println!("PASS: a process without an SDK runtime stops the pinned native writer after issuer SIGKILL and retains the guest disk");
        run(&exe,"cleanup-residue",&root)?;
        run(&exe,"recover-cleanup",&root)?;
        println!("PASS: new-process startup removes only authorized DB-first residue using real VM exit proofs, then cleanup replay preserves native fence IDs");
        Ok(())
    }.await;
    // Always join our worker and try normal SDK cleanup of the exact recorded
    // instance. Never signal a PID discovered from an arbitrary home file.
    if worker.try_wait()?.is_none() {
        let _ = worker.kill();
        let _ = worker.wait();
    }
    let orphan = root.join("sdk/boxes/unrecorded-root");
    if orphan.exists() {
        std::fs::remove_dir_all(orphan)?;
    }
    let cleanup = if root.join("ready.json").exists() {
        run(&exe, "cleanup", &root)
    } else {
        Ok(())
    };
    if result.is_ok() && cleanup.is_ok() {
        std::fs::remove_dir_all(&root)?;
    } else {
        eprintln!("retained guarded-home test evidence at {}", root.display());
    }
    result?;
    cleanup
}
