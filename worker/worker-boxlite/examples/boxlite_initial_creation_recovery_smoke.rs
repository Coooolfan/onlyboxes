//! Real SDK configured creates, owned issuer SIGKILL and pre-SDK recovery.
//! Prefix actors stop before boot; these are not Console/Worker crash tests.
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
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    fs,
    io::{Read, Write},
    os::unix::{
        fs::{OpenOptionsExt, PermissionsExt},
        process::ExitStatusExt,
    },
    path::{Path, PathBuf},
    sync::Arc,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
use worker_boxlite::persistence::{
    terminal_name, BootAuthorization, HostProcess, InitialCreation, Journal, LeaseGuardClient,
    LeaseGuardIdentity, NativeRuntime, RuntimeHomeGuard,
};
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
const WORKER: &str = "initial-creation-recovery-fixture";
const SCOPE: &str = "obx:fixture:unbooted-initial";
#[derive(Serialize, Deserialize)]
struct Control {
    socket: PathBuf,
    identity: LeaseGuardIdentity,
}
#[derive(Serialize, Deserialize)]
struct Ready {
    id: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct NodePrefix {
    worker_id: String,
    scoped_session_id: String,
    home: PathBuf,
    journal: PathBuf,
    lease_state: PathBuf,
    socket: PathBuf,
    guard: LeaseGuardIdentity,
    ready: PathBuf,
}

async fn node_prefix(path: &Path) -> Result<()> {
    use std::os::unix::fs::MetadataExt;
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_file()
        || meta.uid() != unsafe { libc::geteuid() }
        || meta.mode() & 0o077 != 0
        || meta.nlink() != 1
        || meta.len() > 65536
    {
        return Err("node prefix requires bounded private owned input".into());
    }
    let file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)?;
    let spec: NodePrefix = serde_json::from_reader(file.take(65537))?;
    if spec.worker_id != spec.guard.worker_id
        || [
            &spec.home,
            &spec.journal,
            &spec.lease_state,
            &spec.socket,
            &spec.ready,
        ]
        .iter()
        .any(|path| !path.is_absolute())
        || fs::symlink_metadata(&spec.ready).is_ok()
    {
        return Err("invalid node prefix binding or existing readiness evidence".into());
    }
    let client = LeaseGuardClient::bind(&spec.socket, spec.guard.clone());
    let owner = Arc::new(
        RuntimeHomeGuard::open_with_execution_guard(
            &spec.home,
            &spec.journal,
            &spec.worker_id,
            &spec.lease_state,
            &client,
        )
        .await?,
    );
    let journal = owner.journal();
    let mut cfg = config::Config::load();
    cfg.worker_id = spec.worker_id;
    cfg.boxlite_home = spec.home.to_str().ok_or("invalid node home")?.into();
    cfg.persistence.enabled = true;
    let sdk = boxlite_runtime::initialize_guarded(&cfg, owner)?;
    let native =
        NativeRuntime::new(Arc::new(sdk.clone()), &spec.home)?.with_execution_guard(client.clone());
    let options = initial_options()?;
    let intent = InitialCreation {
        operation_id: format!("initial-{}", uuid::Uuid::new_v4()),
        scoped_session_id: spec.scoped_session_id.clone(),
        guard: spec.guard,
        origin: HostProcess::current()?,
        expires_unix_ms: (SystemTime::now().duration_since(UNIX_EPOCH)?.as_millis() + 600_000)
            as i64,
        options_sha256: InitialCreation::options_digest(&options)?,
        box_id: String::new(),
        abandoned: false,
        cleanup_complete: false,
    };
    journal.begin_initial_creation(intent.clone())?;
    let configured = sdk
        .create(options, Some(terminal_name(&spec.scoped_session_id)))
        .await?;
    let info = configured.info().await?;
    if info.status != boxlite::BoxStatus::Configured || info.pid.is_some() {
        return Err("node prefix unexpectedly started an instance".into());
    }
    let id = configured.id().to_string();
    journal.select_initial_creation(&intent, &id)?;
    native.prepare_initial_creation_executable(&journal, &spec.scoped_session_id)?;
    write_private(&spec.ready, &Ready { id })?;
    // The acceptance harness owns and SIGKILLs this exact process after the
    // SDK/preparation prefix. No journal boot, guard grant or SDK start occurs.
    std::future::pending::<()>().await;
    unreachable!()
}
fn private(path: &Path) -> Result<()> {
    fs::create_dir_all(path)?;
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    Ok(())
}
fn write_private(path: &Path, value: &impl Serialize) -> Result<()> {
    let temporary = path.with_extension("pending");
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&temporary)?;
    file.write_all(&serde_json::to_vec(value)?)?;
    file.sync_all()?;
    drop(file);
    fs::rename(temporary, path)?;
    fs::File::open(path.parent().ok_or("missing fixture parent")?)?.sync_all()?;
    Ok(())
}
fn client(root: &Path) -> Result<LeaseGuardClient> {
    let path = if root.join("guard-current.json").exists() {
        root.join("guard-current.json")
    } else {
        root.join("guard.json")
    };
    let control: Control = serde_json::from_slice(&fs::read(path)?)?;
    Ok(LeaseGuardClient::bind(control.socket, control.identity))
}
fn config(root: &Path) -> config::Config {
    let mut cfg = config::Config::load();
    cfg.worker_id = WORKER.into();
    cfg.worker_secret = "fixture-only".into();
    cfg.boxlite_home = root.join("sdk").to_str().unwrap().into();
    cfg.persistence.enabled = true;
    cfg
}
fn fingerprint(root: &Path) -> Result<BTreeMap<String, Vec<u8>>> {
    let mut files = BTreeMap::new();
    for name in [
        "state/journal.json",
        "sdk/db/boxlite.db",
        "sdk/db/boxlite.db-wal",
    ] {
        let path = root.join(name);
        if path.exists() {
            if path.metadata()?.len() > 1 << 20 {
                return Err("fixture metadata exceeds bound".into());
            }
            files.insert(name.into(), fs::read(path)?);
        }
    }
    Ok(files)
}
fn initial_options() -> Result<boxlite::BoxOptions> {
    let mut options = boxlite::BoxOptions {
        cpus: Some(1),
        memory_mib: Some(256),
        disk_size_gb: Some(4),
        auto_delete: Some(0),
        detach: true,
        rootfs: boxlite::RootfsSpec::Image(
            "python@sha256:739ba32ae445e8d58f3d90feb85f83bebc8346f8dd280fa1eb5848f4ff1ed163".into(),
        ),
        ..Default::default()
    };
    options.sanitize()?;
    Ok(options)
}
async fn child(mode: &str, root: &Path) -> Result<()> {
    let client = client(root)?;
    let home = root.join("sdk");
    let state = root.join("state");
    if mode == "reject" {
        let before = fingerprint(root)?;
        if RuntimeHomeGuard::open_with_execution_guard(
            &home,
            &state,
            WORKER,
            &root.join("execution-leases"),
            &client,
        )
        .await
        .is_ok()
        {
            return Err("unsafe pre-boot creation was accepted".into());
        }
        if before != fingerprint(root)? {
            return Err("refused recovery changed journal/database/WAL bytes".into());
        }
        return Ok(());
    }
    let owner = Arc::new(
        RuntimeHomeGuard::open_with_execution_guard(
            &home,
            &state,
            WORKER,
            &root.join("execution-leases"),
            &client,
        )
        .await?,
    );
    let journal = owner.journal();
    let cfg = config(root);
    let sdk = boxlite_runtime::initialize_guarded(&cfg, owner)?;
    let native =
        NativeRuntime::new(Arc::new(sdk.clone()), &home)?.with_execution_guard(client.clone());
    if let Some(case) = mode.strip_prefix("create-") {
        let mut options = initial_options()?;
        let intent = InitialCreation {
            operation_id: format!("initial-{}", uuid::Uuid::new_v4()),
            scoped_session_id: SCOPE.into(),
            guard: client.identity().clone(),
            origin: HostProcess::current()?,
            expires_unix_ms: (SystemTime::now().duration_since(UNIX_EPOCH)?.as_millis() + 600_000)
                as i64,
            options_sha256: InitialCreation::options_digest(&options)?,
            box_id: String::new(),
            abandoned: false,
            cleanup_complete: false,
        };
        journal.begin_initial_creation(intent.clone())?;
        let mut id = String::new();
        if !matches!(case, "intent-only" | "late-empty" | "completed-late") {
            if case == "bad-options" {
                options.memory_mib = Some(257);
            }
            let configured = sdk.create(options, Some(terminal_name(SCOPE))).await?;
            let info = configured.info().await?;
            if info.status != boxlite::BoxStatus::Configured || info.pid.is_some() {
                return Err("SDK prefix unexpectedly started an instance".into());
            }
            id = configured.id().as_str().into();
            if !matches!(case, "unselected" | "bad-options") {
                journal.select_initial_creation(&intent, &id)?;
            }
            if matches!(
                case,
                "prepared" | "alias" | "delete-replay" | "boot-history"
            ) {
                let executable = native.prepare_initial_creation_executable(&journal, SCOPE)?;
                if case == "alias" {
                    fs::hard_link(
                        home.join("boxes").join(&id).join("bin/boxlite-shim"),
                        home.join("boxes")
                            .join(&id)
                            .join(format!("bin/shim-{}.pending", uuid::Uuid::new_v4())),
                    )?;
                }
                if case == "boot-history" {
                    client
                        .prepare_boot(BootAuthorization {
                            operation_id: intent.operation_id.clone(),
                            box_id: id.clone(),
                            scoped_session_id: SCOPE.into(),
                            generation: 1,
                            prior_revision: None,
                            expires_unix_ms: intent.expires_unix_ms,
                            origin: intent.origin.clone(),
                            executable,
                        })
                        .await?;
                }
            }
            if case == "partial" {
                let bin = home.join("boxes").join(&id).join("bin");
                private(bin.parent().unwrap())?;
                private(&bin)?;
                let mut prefix = Vec::new();
                fs::File::open(boxlite::util::find_binary("boxlite-shim")?)?
                    .take(4096)
                    .read_to_end(&mut prefix)?;
                let mut out = fs::OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .mode(0o600)
                    .open(bin.join(format!("shim-{}.pending", uuid::Uuid::new_v4())))?;
                out.write_all(&prefix)?;
                out.sync_all()?;
            }
            if case == "disk" {
                let disks = home.join("boxes").join(&id).join("disks");
                private(disks.parent().unwrap())?;
                private(&disks)?;
                fs::write(disks.join("disk.qcow2"), b"unique-fixture-data-to-preserve")?;
            }
            if case == "mixed-unknown" {
                sdk.create(
                    initial_options()?,
                    Some(terminal_name("obx:fixture:unregistered")),
                )
                .await?;
            }
        }
        write_private(&root.join("ready.json"), &Ready { id })?;
        std::future::pending::<()>().await;
        unreachable!();
    }
    if matches!(mode, "late-after-empty-seal" | "late-after-completion") {
        if mode == "late-after-completion" {
            native.cleanup_abandoned_initial_creations(&journal).await?;
        }
        let record = journal.snapshot()?.initial_creations[SCOPE].clone();
        if !record.abandoned
            || !record.box_id.is_empty()
            || record.cleanup_complete != (mode == "late-after-completion")
        {
            return Err("late-row fixture did not reach its sealed empty boundary".into());
        }
        // Deliberately bypass NativeRuntime's deny fence to model a foreign
        // or unexpected late SDK row. Recovery must preserve it and fail.
        let late = sdk
            .create(initial_options()?, Some(terminal_name(SCOPE)))
            .await?;
        write_private(
            &root.join("late.json"),
            &Ready {
                id: late.id().to_string(),
            },
        )?;
        std::future::pending::<()>().await;
        unreachable!();
    }
    if mode == "remove-before-ack" {
        let record = journal.snapshot()?.initial_creations[SCOPE].clone();
        if !record.abandoned || record.cleanup_complete || record.box_id.is_empty() {
            return Err("missing durable abandonment before SDK removal".into());
        }
        sdk.remove(&record.box_id, false).await?;
        // Reconstruct DB-first residue of the already authorized unbooted
        // cleanup; this is not a random interruption inside SDK remove.
        let bin = home.join("boxes").join(&record.box_id).join("bin");
        private(bin.parent().unwrap())?;
        private(&bin)?;
        fs::copy(
            boxlite::util::find_binary("boxlite-shim")?,
            bin.join("boxlite-shim"),
        )?;
        fs::set_permissions(bin.join("boxlite-shim"), fs::Permissions::from_mode(0o500))?;
        write_private(&root.join("removed.json"), &Ready { id: record.box_id })?;
        std::future::pending::<()>().await;
        unreachable!();
    }
    if mode != "recover" {
        return Err("unknown fixture phase".into());
    }
    native.cleanup_abandoned_initial_creations(&journal).await?;
    let record = journal.snapshot()?.initial_creations[SCOPE].clone();
    if !record.abandoned
        || !record.cleanup_complete
        || sdk.get_info(&terminal_name(SCOPE)).await?.is_some()
        || (!record.box_id.is_empty() && home.join("boxes").join(&record.box_id).exists())
        || journal.snapshot()?.initial_boots.contains_key(SCOPE)
    {
        return Err("unbooted creation cleanup lacks terminal proof".into());
    }
    if !record.box_id.is_empty()
        && (client.status(&record.box_id).await?.is_some()
            || client.boot_status(&record.box_id).await?.is_some()
            || client.retirement_status(&record.box_id).await?.is_some())
    {
        return Err("cleanup fabricated or borrowed execution ownership".into());
    }
    if native
        .create_initial_session(
            &journal,
            SCOPE,
            boxlite::BoxOptions::default(),
            record.expires_unix_ms,
        )
        .await
        .is_ok()
    {
        return Err("abandoned scope recreated a blank instance".into());
    }
    sdk.shutdown(Some(10)).await?;
    Ok(())
}
async fn kill_at(exe: &Path, mode: &str, root: &Path, ready: &str) -> Result<()> {
    let mut child = std::process::Command::new(exe)
        .arg(mode)
        .arg(root)
        .spawn()?;
    let deadline = Instant::now() + Duration::from_secs(40);
    let result = async {
        loop {
            if root.join(ready).exists() {
                break;
            }
            if child.try_wait()?.is_some() {
                return Err("SDK prefix actor exited before its boundary".into());
            }
            if Instant::now() >= deadline {
                return Err("SDK prefix boundary was not reached".into());
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        child.kill()?;
        let status = child.wait()?;
        if status.signal() != Some(libc::SIGKILL) {
            return Err("owned SDK actor did not exit from SIGKILL".into());
        }
        Ok(())
    }
    .await;
    if child.try_wait()?.is_none() {
        let _ = child.kill();
        let _ = child.wait();
    }
    result
}
fn run(exe: &Path, mode: &str, root: &Path) -> Result<()> {
    if !std::process::Command::new(exe)
        .arg(mode)
        .arg(root)
        .status()?
        .success()
    {
        return Err("SDK recovery fixture phase failed".into());
    }
    Ok(())
}
#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = std::env::args_os().collect();
    if args.len() == 3 && args[1] == "node-prefix" {
        return node_prefix(Path::new(&args[2])).await;
    }
    if args.len() == 3 {
        return child(
            args[1].to_str().ok_or("invalid phase")?,
            Path::new(&args[2]),
        )
        .await;
    }
    let exe = std::env::current_exe()?;
    for case in [
        "intent-only",
        "unselected",
        "selected",
        "prepared",
        "partial",
        "alias",
        "delete-replay",
        "disk",
        "bad-options",
        "boot-history",
        "guard-epoch",
        "late-empty",
        "completed-late",
        "mixed-unknown",
    ] {
        let root = std::env::temp_dir().join(format!(
            "onlyboxes-create-recovery-{}",
            uuid::Uuid::new_v4()
        ));
        private(&root)?;
        let mut guard = lease_guard::Guard::start(&root, WORKER).await?;
        write_private(
            &root.join("guard.json"),
            &Control {
                socket: guard.socket_directory().join("control.sock"),
                identity: guard.client.identity().clone(),
            },
        )?;
        let result:Result<()>=async {
            kill_at(&exe,&format!("create-{case}"),&root,"ready.json").await?;
            let record:Ready=serde_json::from_slice(&fs::read(root.join("ready.json"))?)?;
            let mut before=fingerprint(&root)?;
            let negative=matches!(case,"disk"|"bad-options"|"boot-history"|"guard-epoch"|"late-empty"|"completed-late"|"mixed-unknown");
            if matches!(case,"late-empty"|"completed-late") {
                kill_at(&exe,if case=="late-empty" {"late-after-empty-seal"} else {"late-after-completion"},&root,"late.json").await?;
                before=fingerprint(&root)?;
            }
            if case=="guard-epoch" {
                let store=guard.client.identity().store_id.clone();guard.shutdown().await?;
                guard=lease_guard::Guard::reopen(&root,WORKER,&store).await?;
                write_private(&root.join("guard-current.json"),&Control {socket:guard.socket_directory().join("control.sock"),identity:guard.client.identity().clone()})?;
            }
            if negative {
                run(&exe,"reject",&root)?;
                if before!=fingerprint(&root)? { return Err("unsafe refusal changed durable creation evidence".into()); }
                if case=="disk" && fs::read(root.join("sdk/boxes").join(&record.id).join("disks/disk.qcow2"))?!=b"unique-fixture-data-to-preserve" { return Err("unknown writable data was removed".into()); }
            } else {
                if case=="delete-replay" { kill_at(&exe,"remove-before-ack",&root,"removed.json").await?;if !root.join("sdk/boxes").join(&record.id).join("bin/boxlite-shim").is_file() {return Err("DB-first residue window missed".into());} }
                run(&exe,"recover",&root)?;run(&exe,"recover",&root)?;
            }
            println!("PASS: initial creation {case}: {}",if negative {"refused without SDK initialization or data/journal mutation"} else {"issuer SIGKILL, same-guard recovery, terminal cleanup and replay; no boot or blank recreation"});
            Ok(())
        }.await;
        // Every actor is owned/joined, and no prefix ever calls SDK start.
        // The pending-boot contradiction deliberately keeps guard history.
        if case == "boot-history" {
            guard.stop_abruptly()?;
        } else {
            guard.shutdown().await?;
        }
        drop(guard);
        if result.is_ok() {
            fs::remove_dir_all(&root)?;
        } else {
            eprintln!(
                "retained initial creation fixture evidence at {}",
                root.display()
            );
        }
        result?;
    }
    Ok(())
}
