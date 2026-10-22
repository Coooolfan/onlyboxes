//! Pre-boot guardian grants against real BoxLite VMs. An owned SDK issuer is
//! killed while shim.pid is absent; pending and bound grants have distinct
//! shutdown evidence. This is a local fixture, not production supervision.
use boxlite::{BoxCommand, BoxOptions, BoxliteOptions, BoxliteRuntime, RootfsSpec};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    process::Command,
    sync::Arc,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
use worker_boxlite::persistence::{
    terminal_name, BootAuthorization, ExecutionLease, HostProcess, LeaseGuardClient, NativeRuntime,
    Phase, RuntimeHomeGuard,
};
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
const WORKER: &str = "native-boot-guard-worker";
const SCOPE: &str = "obx:fixture:boot-guard";
const OPERATION: &str = "source-rollback";
const IMAGE: &str =
    "python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258";
fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}
fn private(path: &Path) -> Result<()> {
    fs::create_dir(path)?;
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    Ok(())
}
struct Child(std::process::Child);
impl Drop for Child {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
#[derive(Serialize, Deserialize)]
struct Record {
    grant: BootAuthorization,
    writer: HostProcess,
    lease: Option<ExecutionLease>,
}
async fn sdk_child(root: &Path, mode: &str) -> Result<()> {
    let home = root.join("sdk");
    let owner = RuntimeHomeGuard::open(&home, root.join("journal"), WORKER)?;
    let journal = owner.journal();
    let sdk = Arc::new(BoxliteRuntime::new(BoxliteOptions {
        home_dir: home.clone(),
        ..Default::default()
    })?);
    let mut options = BoxOptions {
        cpus: Some(1),
        memory_mib: Some(256),
        auto_delete: Some(0),
        detach: true,
        rootfs: RootfsSpec::Image(IMAGE.into()),
        entrypoint: Some(vec!["sh".into(), "-c".into()]),
        cmd: Some(vec!["while :; do sleep 3600; done".into()]),
        ..Default::default()
    };
    options.advanced.security.resource_limits.max_processes = Some(128);
    let vm = sdk.create(options, Some(terminal_name(SCOPE))).await?;
    vm.start().await?;
    let result = vm
        .exec(BoxCommand::new("sh").args([
            "-c",
            "printf retained-before-restart > /boot-guard-data; sync",
        ]))
        .await?
        .wait()
        .await?;
    if result.exit_code != 0 {
        return Err("guest fixture write failed".into());
    }
    let id = vm.id().to_string();
    let native = NativeRuntime::new(sdk.clone(), &home)?;
    let instance = native.inspect(SCOPE, &id).await?;
    journal.probe(SCOPE, &id)?;
    let first_operation = if mode == "bound" {
        "running-source-rollback"
    } else {
        OPERATION
    };
    journal.claim(serde_json::from_value(serde_json::json!({
        "operation_id":first_operation,"scoped_session_id":SCOPE,"generation":1,
        "kind":"source","phase":"preparing","box_id":id,
        "restore_spec":instance.spec,"lease_expires_unix_ms":0,"staging_bytes":1
    }))?)?;
    let identity =
        LeaseGuardClient::discover(root.join("control/control.sock"), WORKER, None).await?;
    let client = LeaseGuardClient::bind(root.join("control/control.sock"), identity.clone());
    if mode == "bound" {
        let original = native.inspect_writer(SCOPE, &id).await?;
        client
            .adopt(ExecutionLease {
                box_id: id.clone(),
                scoped_session_id: SCOPE.into(),
                generation: 1,
                revision: 1,
                expires_unix_ms: now_ms() + 60_000,
                process: original.clone(),
                revoked: false,
            })
            .await?;
        let guarded = NativeRuntime::new(sdk.clone(), &home)?.with_execution_guard(client.clone());
        let reuse_expiry = now_ms() + 20_000;
        guarded
            .rollback_source(
                &journal,
                first_operation,
                reuse_expiry,
                Duration::from_secs(3),
            )
            .await?;
        guarded
            .rollback_source(
                &journal,
                first_operation,
                reuse_expiry,
                Duration::from_secs(3),
            )
            .await?;
        let reused = journal.snapshot()?.operations[first_operation].clone();
        if reused
            .boot_guard
            .as_ref()
            .is_none_or(|binding| binding.authorization.is_some())
            || reused.started_process.as_ref() != Some(&original)
            || reused.source_stop_confirmed
            || native.inspect_writer(SCOPE, &id).await? != original
            || native
                .rollback_source(
                    &journal,
                    first_operation,
                    reuse_expiry,
                    Duration::from_secs(3),
                )
                .await
                .is_ok()
        {
            return Err(
                "running-source rollback rebooted or lost its immutable companion binding".into(),
            );
        }
        journal.claim(serde_json::from_value(serde_json::json!({
            "operation_id":OPERATION,"scoped_session_id":SCOPE,"generation":1,
            "kind":"source","phase":"preparing","box_id":id,
            "restore_spec":instance.spec,"lease_expires_unix_ms":0,"staging_bytes":1
        }))?)?;
        println!("PASS: a running source rollback retains the adopted VM and exact guardian epoch without minting a boot grant or rebooting; replay remains guarded");
    }
    native.stop_source(&journal, OPERATION).await?;
    let expiry = now_ms() + 8000;
    let (grant, writer, lease) = if mode == "bound" {
        let mut stale = identity.clone();
        stale.epoch_id = uuid::Uuid::new_v4().to_string();
        let stale = NativeRuntime::new(sdk.clone(), &home)?.with_execution_guard(
            LeaseGuardClient::bind(root.join("control/control.sock"), stale),
        );
        if stale
            .rollback_source(&journal, OPERATION, expiry, Duration::from_secs(3))
            .await
            .is_ok()
            || journal.snapshot()?.operations[OPERATION].start_attempted
            || native.inspect(SCOPE, &id).await?.status != boxlite::BoxStatus::Stopped
        {
            return Err("unavailable guardian permitted an SDK boot or modified its intent".into());
        }
        let guarded = NativeRuntime::new(sdk.clone(), &home)?.with_execution_guard(client.clone());
        guarded
            .rollback_source(&journal, OPERATION, expiry, Duration::from_secs(3))
            .await?;
        let operation = journal.snapshot()?.operations[OPERATION].clone();
        let binding = operation
            .boot_guard
            .ok_or("runtime did not persist its guard binding")?;
        let grant = binding
            .authorization
            .ok_or("runtime did not persist its boot authorization")?;
        let writer = operation
            .started_process
            .ok_or("runtime did not retain its native writer")?;
        if operation.phase != Phase::RolledBack || binding.guard != identity {
            return Err(
                "runtime acknowledged rollback without durable readiness and guard binding".into(),
            );
        }
        // Reconstruct the native adapter and replay without another SDK start.
        NativeRuntime::new(sdk.clone(), &home)?
            .with_execution_guard(client.clone())
            .rollback_source(&journal, OPERATION, expiry, Duration::from_secs(3))
            .await?;
        if native
            .rollback_source(&journal, OPERATION, expiry, Duration::from_secs(3))
            .await
            .is_ok()
            || stale
                .rollback_source(&journal, OPERATION, expiry, Duration::from_secs(3))
                .await
                .is_ok()
            || native.inspect_writer(SCOPE, &id).await? != writer
        {
            return Err(
                "guarded rollback permitted downgrade, epoch replacement or a new writer".into(),
            );
        }
        guarded.verify_execution_guard(&journal, OPERATION).await?;
        if native
            .verify_execution_guard(&journal, OPERATION)
            .await
            .is_ok()
            || stale
                .verify_execution_guard(&journal, OPERATION)
                .await
                .is_ok()
        {
            return Err("readiness or activation accepted a missing or replaced guardian".into());
        }
        let lease = client
            .status(&id)
            .await?
            .ok_or("runtime did not bind an execution lease")?;
        let renewed = client
            .renew(
                lease.revision,
                ExecutionLease {
                    revision: lease.revision + 1,
                    expires_unix_ms: lease.expires_unix_ms + 3000,
                    ..lease
                },
            )
            .await?;
        println!("PASS: the real rollback adapter requires its guardian before SDK start, durably binds the writer before readiness, replays after reconstruction and rejects missing or replaced guard epochs");
        (grant, writer, Some(renewed))
    } else {
        let grant = BootAuthorization {
            operation_id: OPERATION.into(),
            box_id: id.clone(),
            scoped_session_id: SCOPE.into(),
            generation: 1,
            prior_revision: None,
            expires_unix_ms: expiry,
            origin: HostProcess::current()?,
            executable: native.prepare_boot_executable(&journal, OPERATION).await?,
        };
        // Fault fixture: durably hold a boot intent but omit binding after start.
        journal.update(OPERATION, |op| {
            op.phase = Phase::RollingBack;
            op.start_attempted = true;
            op.boot_executable = Some(grant.executable.clone());
            op.lease_expires_unix_ms = expiry;
            op.boot_guard = Some(worker_boxlite::persistence::GuardedBoot {
                guard: identity,
                authorization: Some(grant.clone()),
            });
            Ok(())
        })?;
        client.prepare_boot(grant.clone()).await?;
        let vm = sdk.get(&id).await?.ok_or("stopped source disappeared")?;
        vm.start().await?;
        let writer = native.inspect_writer(SCOPE, &id).await?;
        (grant, writer, None)
    };
    fs::write(root.join("boot-intent.json"), serde_json::to_vec(&grant)?)?;
    fs::File::open(root.join("boot-intent.json"))?.sync_all()?;
    fs::File::open(root)?.sync_all()?;
    // Reconstruct the SDK's publication gap without altering executable bytes.
    fs::rename(
        home.join("boxes").join(&id).join("shim.pid"),
        root.join("withheld-shim.pid"),
    )?;
    let record = Record {
        grant,
        writer,
        lease,
    };
    fs::write(root.join("ready.pending"), serde_json::to_vec(&record)?)?;
    fs::rename(root.join("ready.pending"), root.join("ready.json"))?;
    // Parent SIGKILLs this owned issuer. Do not finish or bind pending boots.
    std::future::pending::<()>().await;
    Ok(())
}
async fn scenario(root: &Path, mode: &str, helper: &Path) -> Result<()> {
    private(root)?;
    private(&root.join("sdk"))?;
    private(&root.join("control"))?;
    let mut guard = Child(
        Command::new(helper)
            .arg("--state-dir")
            .arg(root.join("leases"))
            .arg("--socket-dir")
            .arg(root.join("control"))
            .arg("--worker-id")
            .arg(WORKER)
            .spawn()?,
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    let identity = loop {
        if guard.0.try_wait()?.is_some() || Instant::now() > deadline {
            return Err("guardian startup failed".into());
        }
        if let Ok(identity) =
            LeaseGuardClient::discover(root.join("control/control.sock"), WORKER, None).await
        {
            break identity;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    };
    let client = LeaseGuardClient::bind(root.join("control/control.sock"), identity);
    let mut issuer = Child(
        Command::new(std::env::current_exe()?)
            .arg("issuer")
            .arg(mode)
            .arg(root)
            .spawn()?,
    );
    let deadline = Instant::now() + Duration::from_secs(120);
    while !root.join("ready.json").exists() {
        if issuer.0.try_wait()?.is_some() || Instant::now() > deadline {
            return Err("native issuer startup failed".into());
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let record: Record = serde_json::from_slice(&fs::read(root.join("ready.json"))?)?;
    issuer.0.kill()?;
    issuer.0.wait()?;
    if client
        .boot_status(&record.grant.box_id)
        .await
        .map_err(|error| format!("read boot receipt after issuer death: {error}"))?
        != Some(record.grant.clone())
    {
        return Err("durable boot authority was lost".into());
    }
    if root
        .join("sdk/boxes")
        .join(&record.grant.box_id)
        .join("shim.pid")
        .exists()
    {
        return Err("fixture still has a published PID".into());
    }
    let expires = record
        .lease
        .as_ref()
        .map_or(record.grant.expires_unix_ms, |lease| lease.expires_unix_ms);
    if mode == "bound" {
        while now_ms() <= record.grant.expires_unix_ms + 50 {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        if record.writer.writers_exited()? {
            return Err("old boot deadline stopped a renewed VM".into());
        }
    }
    let deadline = Instant::now()
        + Duration::from_millis(expires.saturating_sub(now_ms()).max(0) as u64 + 3000);
    loop {
        // A kernel query can be transiently unavailable while a VM exits.
        // Keep the original evidence and retry; only Ok(true) proves stop.
        if record.writer.writers_exited() == Ok(true) {
            break;
        }
        if Instant::now() > deadline {
            return Err("could not prove native VM exit after guardian expiry".into());
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    if now_ms() + 25 < expires {
        return Err("native writer stopped before lease expiry".into());
    }
    if !root
        .join("sdk/boxes")
        .join(&record.grant.box_id)
        .join("disks/disk.qcow2")
        .is_file()
    {
        return Err("lease expiry removed the root disk".into());
    }
    // SAFETY: this parent owns the unreaped guard and therefore reserves its PID.
    if unsafe { libc::kill(guard.0.id() as i32, libc::SIGTERM) } != 0 {
        return Err("shutdown signal failed".into());
    }
    if mode == "bound" {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(status) = guard.0.try_wait()? {
                if !status.success() {
                    return Err("bound guardian shutdown failed".into());
                }
                break;
            }
            if Instant::now() > deadline {
                return Err("bound guardian did not drain".into());
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        println!("PASS: a pre-boot native grant binds into CAS renewal; after issuer SIGKILL and PID-file loss, the VM survives the old boot deadline, stops at renewed expiry, retains disk and permits confirmed guardian shutdown");
    } else {
        tokio::time::sleep(Duration::from_millis(300)).await;
        if guard.0.try_wait()?.is_some() {
            return Err("unknown native start outcome falsely confirmed shutdown".into());
        }
        println!("PASS: an unbound pre-boot grant discovers and stops the real VM after issuer SIGKILL without shim.pid, retains disk and keeps the unresolved native start outcome fenced during shutdown");
    }
    Ok(())
}
#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = std::env::args_os().collect();
    if args.len() == 4 && args[1] == "issuer" {
        return sdk_child(Path::new(&args[3]), args[2].to_str().ok_or("invalid mode")?).await;
    }
    let helper = std::env::var_os("BOXLITE_LEASE_GUARD_BINARY")
        .map(PathBuf::from)
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
        return Err("build --bin boxlite-lease-guard before running the fixture".into());
    }
    let root =
        PathBuf::from("/tmp").join(format!("obx-bg-{}", &uuid::Uuid::new_v4().to_string()[..8]));
    private(&root)?;
    println!(
        "native pre-boot guardian acceptance; retained root={}",
        root.display()
    );
    scenario(&root.join("bound"), "bound", &helper).await?;
    scenario(&root.join("pending"), "pending", &helper).await?;
    Ok(())
}
