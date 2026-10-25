//! Real SDK reopening after an owned child is killed; pre-SDK refusals preserve
//! orphan data and independently scheduled lease expiry after issuer loss.
//! The guardian is an owned fixture process, not a deployed supervised service.
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
use worker_boxlite::persistence::{
    terminal_name, ExecutionLease, LeaseGuardClient, RuntimeHomeGuard,
};
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
    executable: worker_boxlite::persistence::BootExecutable,
}
#[derive(Serialize, Deserialize)]
struct LeaseReady {
    store_id: String,
    initial_expiry: i64,
    lease: ExecutionLease,
}
struct Guardian(std::process::Child);
impl Drop for Guardian {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
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
                    .args(["-c", "printf preserved > /home-guard-data; sync; (while :; do printf . >> /home-guard-background; sleep 0.1; done) >/dev/null 2>&1 &"]),
            )
            .await?
            .wait()
            .await?;
        if execution.exit_code != 0 {
            return Err("test data write failed".into());
        }
        let info = native.info().await?;
        let id = info.id.to_string();
        // This live root has no journal/probe entry yet. A Console reconnect
        // with an empty inventory cannot turn its name into delete authority.
        if boxlite_runtime::cleanup_orphan_terminal_session_boxes(
            &cfg,
            &std::collections::HashSet::new(),
        )
        .await
            != 0
            || sdk.get(&id).await?.is_none()
            || native.info().await?.pid != info.pid
        {
            return Err("ordinary orphan cleanup removed a protected unregistered root".into());
        }
        println!("PASS: empty Console inventory cannot delete an unregistered live root in the protected home");
        journal.probe(SCOPE, &id)?;
        let pid = boxlite::util::PidRecord::decode(&std::fs::read(
            home.join("boxes").join(&id).join("shim.pid"),
        )?)?;
        let record = Record {
            executable: worker_boxlite::persistence::NativeRuntime::new(
                Arc::new(sdk.clone()),
                &home,
            )?
            .original_boot_executable(SCOPE, &id)
            .await?,
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
                    .args(["-c", "test \"$(cat /home-guard-data)\" = preserved && before=$(wc -c < /home-guard-background) && sleep 0.2 && after=$(wc -c < /home-guard-background) && test \"$after\" -gt \"$before\""]),
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
    let helper = std::env::var_os("BOXLITE_LEASE_GUARD_BINARY")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| {
            std::env::current_exe()
                .unwrap()
                .parent()
                .unwrap()
                .parent()
                .unwrap()
                .join("boxlite-lease-guard")
        });
    if !helper.is_file() {
        return Err("build the boxlite-lease-guard companion binary before this fixture".into());
    }
    let control = std::path::PathBuf::from("/tmp")
        .join(format!("obx-ng-{}", &uuid::Uuid::new_v4().to_string()[..8]));
    private(&control)?;
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
    let mut guardian: Option<Guardian> = None;
    let result:Result<()>=async {
        while !root.join("ready.json").exists() {
            if worker.try_wait()?.is_some() { return Err("owned SDK child exited before readiness".into()); }
            if started.elapsed()>Duration::from_secs(120) { return Err("owned SDK child did not become ready".into()); }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        guardian=Some(Guardian(std::process::Command::new(&helper).arg("--state-dir").arg(root.join("execution-leases")).arg("--socket-dir").arg(&control).arg("--worker-id").arg(WORKER).spawn()?));
        let guard_deadline=std::time::Instant::now()+Duration::from_secs(10);
        let identity=loop {
            if guardian.as_mut().unwrap().0.try_wait()?.is_some() || std::time::Instant::now()>=guard_deadline {return Err("independent guardian did not become ready".into());}
            if let Ok(identity)=LeaseGuardClient::discover(control.join("control.sock"),WORKER,None).await{break identity;}
            tokio::time::sleep(Duration::from_millis(10)).await;
        };
        let client=LeaseGuardClient::bind(control.join("control.sock"),identity.clone());
        let record:Record=serde_json::from_slice(&std::fs::read(root.join("ready.json"))?)?;
        let initial=client.adopt(ExecutionLease{box_id:record.id,scoped_session_id:SCOPE.into(),generation:1,revision:1,expires_unix_ms:now_ms()+2000,process:record.writer,revoked:false}).await?;
        let lease=client.renew(1,ExecutionLease{revision:2,expires_unix_ms:initial.expires_unix_ms+4000,..initial.clone()}).await?;
        let receipt=LeaseReady{store_id:identity.store_id,initial_expiry:initial.expires_unix_ms,lease};
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
        let pid_file=root.join("sdk/boxes").join(&record.id).join("shim.pid");
        let saved=root.join("saved-shim.pid");
        std::fs::rename(&pid_file,&saved)?;
        let found=record.executable.discover()?;
        std::fs::rename(&saved,&pid_file)?;
        if !found.contains(&record.writer){return Err("kernel discovery lost the native VM when its PID file was unavailable".into());}
        println!("PASS: a durable per-box executable identifies the same real VM through kernel process evidence while shim.pid is absent");
        while now_ms()<=receipt.initial_expiry+50 {tokio::time::sleep(Duration::from_millis(10)).await;}
        if record.writer.writers_exited()? {return Err("stale initial deadline stopped a renewed writer".into());}
        let remaining=receipt.lease.expires_unix_ms.saturating_sub(now_ms()).max(0) as u64;
        let deadline=std::time::Instant::now()+Duration::from_millis(remaining+2000);
        while !record.writer.writers_exited()? {
            if std::time::Instant::now()>=deadline {return Err("native writer did not stop independently of the dead SDK issuer".into());}
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        if now_ms()+25<receipt.lease.expires_unix_ms {return Err("writer stopped before the confirmed renewed lease".into());}
        if !root.join("sdk/boxes").join(&record.id).join("disks/disk.qcow2").is_file() {return Err("exact writer stop removed the retained guest disk".into());}
        if guardian.as_mut().unwrap().0.try_wait()?.is_some() {return Err("expiry depended on the dead issuer instead of the independent guardian".into());}
        println!("PASS: an independent guardian confirms durable adoption/renewal, preserves the live background writer past the original deadline, then stops the pinned VM at the renewed lease after issuer SIGKILL and retains its guest disk");
        let pid=guardian.as_ref().unwrap().0.id();
        // SAFETY: the owned, unreaped helper reserves its PID until this parent waits.
        if unsafe{libc::kill(pid as i32,libc::SIGTERM)}!=0{return Err("guardian shutdown signal failed".into());}
        let closed=std::time::Instant::now()+Duration::from_secs(5);
        loop {
            if let Some(status)=guardian.as_mut().unwrap().0.try_wait()? {if !status.success(){return Err("guardian shutdown failed".into());}break;}
            if std::time::Instant::now()>=closed{return Err("guardian did not drain after expiry".into());}
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        guardian.take();
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
    drop(guardian);
    let _ = std::fs::remove_dir_all(&control);
    if result.is_ok() && cleanup.is_ok() {
        std::fs::remove_dir_all(&root)?;
    } else {
        eprintln!("retained guarded-home test evidence at {}", root.display());
    }
    result?;
    cleanup
}
