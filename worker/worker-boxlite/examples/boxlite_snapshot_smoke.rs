//! Native cold-home export/import acceptance for the pinned BoxLite SDK.
//!
//! Run `cargo run --example boxlite_snapshot_smoke`. BOXLITE_SNAPSHOT_SMOKE_IMAGE
//! may select another immutable image containing Python and /bin/sh. All runtime
//! homes are private temporary directories owned by this invocation.

#[path = "support/snapshot_store.rs"]
mod snapshot_store;

use std::error::Error;
use std::path::Path;
use std::process::Command;
use std::sync::Arc;
use std::time::Instant;

use boxlite::{
    BoxCommand, BoxOptions, BoxStatus, BoxliteOptions, BoxliteRuntime, LiteBox, RootfsSpec,
};
use sha2::{Digest, Sha256};
use tokio_stream::StreamExt;
use worker_boxlite::persistence::{
    terminal_name, Archive, ArchiveLimits, Journal, Kind, NativeRuntime, Operation, Phase,
    RestoreSpec,
};

type Result<T> = std::result::Result<T, Box<dyn Error>>;
const IMAGE: &str =
    "python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258";
const WRITE: &str = r#"
import json, os
p='/onlyboxes-persistence'
os.mkdir(p)
with open(p+'/data','w') as f: f.write('persistent-rootfs')
os.chmod(p+'/data',0o640)
os.chown(p+'/data',1234,2345)
os.link(p+'/data',p+'/hardlink')
os.symlink('data',p+'/symlink')
os.mkfifo(p+'/fifo',0o600)
os.setxattr(p+'/data','user.onlyboxes',b'native-archive')
with open(p+'/sparse','wb') as f:
 f.seek(8*1024*1024); f.write(b'end')
with open('/etc/onlyboxes-base-change','w') as f: f.write('modified')
os.unlink('/etc/debian_version')
with open('/dev/shm/onlyboxes-ephemeral','w') as f: f.write('memory-only')
print('source fixture ready')
"#;
const VERIFY: &str = r#"
import json, os, stat
p='/onlyboxes-persistence'
s=os.stat(p+'/data')
assert open(p+'/data').read()=='persistent-rootfs'
assert stat.S_IMODE(s.st_mode)==0o640
assert (s.st_uid,s.st_gid)==(1234,2345)
assert os.stat(p+'/hardlink').st_ino==s.st_ino
assert os.readlink(p+'/symlink')=='data'
assert stat.S_ISFIFO(os.stat(p+'/fifo').st_mode)
assert os.getxattr(p+'/data','user.onlyboxes')==b'native-archive'
with open(p+'/sparse','rb') as f:
 assert f.read(16)==bytes(16)
 f.seek(8*1024*1024); assert f.read()==b'end'
assert open('/etc/onlyboxes-base-change').read()=='modified'
assert not os.path.exists('/etc/debian_version')
assert not os.path.exists('/dev/shm/onlyboxes-ephemeral')
assert os.environ['ONLYBOXES_ARCHIVE_CONFIG']=='preserved'
print(json.dumps({'verified':['content','mode','uid_gid','hardlink','symlink','fifo','xattr','sparse','base_changes','deletion','tmpfs_excluded','env'],'sparse_blocks':os.stat(p+'/sparse').st_blocks}))
"#;

async fn python(litebox: &LiteBox, script: &str) -> Result<String> {
    let mut execution = litebox
        .exec(BoxCommand::new("python").args(["-c", script]))
        .await?;
    let stdout = execution.stdout();
    let stderr = execution.stderr();
    let output = tokio::spawn(async move {
        let mut data = String::new();
        if let Some(mut stream) = stdout {
            while let Some(chunk) = stream.next().await {
                data.push_str(&chunk);
            }
        }
        data
    });
    let errors = tokio::spawn(async move {
        let mut data = String::new();
        if let Some(mut stream) = stderr {
            while let Some(chunk) = stream.next().await {
                data.push_str(&chunk);
            }
        }
        data
    });
    let status = execution.wait().await?;
    let output = output.await?;
    let errors = errors.await?;
    if status.exit_code != 0 {
        return Err(format!(
            "guest verification failed: exit={} {errors}",
            status.exit_code
        )
        .into());
    }
    Ok(output)
}

fn runtime(home: &Path) -> Result<Arc<BoxliteRuntime>> {
    std::fs::create_dir_all(home)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(home, std::fs::Permissions::from_mode(0o700))?;
    }
    Ok(Arc::new(BoxliteRuntime::new(BoxliteOptions {
        home_dir: home.to_path_buf(),
        ..Default::default()
    })?))
}

async fn phase(phase: &str, root: &Path, image: &str) -> Result<()> {
    let endpoint = std::env::var("BOXLITE_SNAPSHOT_SMOKE_STORE_URL")?;
    if phase == "verify" {
        let (_, descriptor): (RestoreSpec, Archive) =
            serde_json::from_slice(&std::fs::read(root.join("snapshot.meta.json"))?)?;
        let transfer =
            worker_boxlite::persistence::Transfer::new(4, std::time::Duration::from_secs(30))?;
        transfer
            .verify_remote(
                &snapshot_store::grant(&endpoint, "GET", descriptor.size_bytes, 0, None),
                &descriptor,
                tokio_util::sync::CancellationToken::new(),
            )
            .await?;
        let journal = Journal::open(root.join("source-journal"), "smoke-worker")?;
        journal.update("snapshot-source-operation", |op| {
            op.remote_verified = true;
            Ok(())
        })?;
        println!("PASS: uploaded object read back and fully verified over HTTP");
        return Ok(());
    }
    let runtime = runtime(&root.join(format!("{phase}-home")))?;
    let archive = root.join("snapshot.boxlite");
    let result: Result<()> = async {
        if phase == "source" {
            let litebox = runtime
                .create(
                    BoxOptions {
                        cpus: Some(1),
                        memory_mib: Some(512),
                        rootfs: RootfsSpec::Image(image.to_owned()),
                        auto_delete: Some(0),
                        detach: true,
                        env: vec![(
                            "ONLYBOXES_ARCHIVE_CONFIG".to_owned(),
                            "preserved".to_owned(),
                        )],
                        entrypoint: Some(vec!["sh".to_owned(), "-lc".to_owned()]),
                        cmd: Some(vec!["while true; do sleep 3600; done".to_owned()]),
                        ..Default::default()
                    },
                    Some(terminal_name("obx:smoke:session")),
                )
                .await?;
            litebox.start().await?;
            println!("{}", python(&litebox, WRITE).await?);
            let backend = NativeRuntime::new(runtime.clone(), root.join("source-home"))?;
            let inspected = backend
                .inspect("obx:smoke:session", litebox.id().as_str())
                .await?;
            if backend
                .inspect("obx:smoke:other", litebox.id().as_str())
                .await
                .is_ok()
            {
                return Err("inspection accepted another scope".into());
            }
            let journal = Arc::new(Journal::open(root.join("source-journal"), "smoke-worker")?);
            if backend
                .stop_source(&journal, "missing-operation")
                .await
                .is_ok()
            {
                return Err("stop accepted an unclaimed operation".into());
            }
            if litebox.info().await?.status != BoxStatus::Running {
                return Err("read-only inspection stopped the source".into());
            }
            journal.probe("obx:smoke:session", litebox.id().as_str())?;
            journal.claim(Operation {
                operation_id: "snapshot-source-operation".into(),
                scoped_session_id: "obx:smoke:session".into(),
                generation: 1,
                kind: Kind::Source,
                phase: Phase::Preparing,
                restore_spec: Some(inspected.spec),
                archive: None,
                artifact: String::new(),
                box_id: litebox.id().as_str().into(),
                tombstone_box_id: String::new(),
                source_process: None,
                source_stop_confirmed: false,
                lease_expires_unix_ms: std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)?
                    .as_millis() as i64
                    + 300_000,
                staging_bytes: 65 << 30,
                snapshot_committed: false,
                verified: false,
                remote_verified: false,
                cache_until_unix_ms: 0,
                lease_ttl_sec: 0,
            })?;
            let wrong_journal = Journal::open(root.join("wrong-spec-journal"), "smoke-worker")?;
            let mut wrong = journal.snapshot()?.operations["snapshot-source-operation"].clone();
            wrong
                .restore_spec
                .as_mut()
                .ok_or("missing source specification")?
                .memory_mib += 1;
            wrong_journal.claim(wrong)?;
            if backend
                .stop_source(&wrong_journal, "snapshot-source-operation")
                .await
                .is_ok()
                || litebox.info().await?.status != BoxStatus::Running
            {
                return Err("a mismatched source specification stopped the VM".into());
            }
            wrong_journal.close()?;
            if backend
                .export_source(
                    journal.clone(),
                    "snapshot-source-operation",
                    ArchiveLimits {
                        max_bytes: 1 << 40,
                        max_expanded_bytes: 1 << 40,
                    },
                )
                .await
                .err()
                != Some(worker_boxlite::persistence::NativeError::Budget)
                || litebox.info().await?.status != BoxStatus::Running
            {
                return Err("insufficient staging admission stopped the VM".into());
            }
            let unproven = journal.snapshot()?.operations["snapshot-source-operation"].clone();
            backend
                .stop_source(&journal, "snapshot-source-operation")
                .await?;
            backend
                .stop_source(&journal, "snapshot-source-operation")
                .await?;
            let stopped_record=journal.snapshot()?.operations["snapshot-source-operation"].clone();
            if !stopped_record.source_stop_confirmed || !stopped_record.source_process.as_ref()
                .ok_or("source process evidence was not persisted")?.has_exited()? {
                return Err("source stopped without durable host-process exit evidence".into());
            }
            let unproven_journal=Journal::open(root.join("unproven-source-journal"),"smoke-worker")?;
            unproven_journal.claim(unproven)?;
            if !matches!(backend.stop_source(&unproven_journal,"snapshot-source-operation").await,
                Err(worker_boxlite::persistence::NativeError::Conflict)) {
                return Err("stopped SDK metadata without prior host identity was trusted".into());
            }
            let interrupted=Journal::open(root.join("interrupted-stop-journal"),"smoke-worker")?;
            let mut pending_stop=stopped_record.clone();
            pending_stop.source_stop_confirmed=false;
            interrupted.claim(pending_stop)?;
            backend.stop_source(&interrupted,"snapshot-source-operation").await?;
            let recovered_stop=interrupted.snapshot()?.operations["snapshot-source-operation"].clone();
            if !recovered_stop.source_stop_confirmed || recovered_stop.source_process!=stopped_record.source_process {
                return Err("stopped SDK instance could not recover the same host-process exit proof".into());
            }
            println!(
                "PASS: private host-process proof, stop-ack recovery and idempotent source stop"
            );
            if litebox.info().await?.status != BoxStatus::Stopped {
                return Err("source did not stop all writers".into());
            }
            if backend
                .export_source(
                    journal.clone(),
                    "snapshot-source-operation",
                    ArchiveLimits {
                        max_bytes: 1,
                        max_expanded_bytes: 64 << 30,
                    },
                )
                .await
                .err()
                != Some(worker_boxlite::persistence::NativeError::Budget)
                || !runtime.exists(litebox.id().as_str()).await?
                || journal.snapshot()?.operations["snapshot-source-operation"].phase
                    != Phase::Preparing
            {
                return Err("quota rejection discarded the source or published an archive".into());
            }
            let started = Instant::now();
            let limits = ArchiveLimits {
                max_bytes: 256 << 20,
                max_expanded_bytes: 64 << 30,
            };
            let descriptor = backend
                .export_source(journal.clone(), "snapshot-source-operation", limits)
                .await?;
            let repeated = backend
                .export_source(journal.clone(), "snapshot-source-operation", limits)
                .await?;
            if repeated != descriptor {
                return Err("frozen export replay changed the archive".into());
            }
            let artifact =
                journal.artifact_path("obx:smoke:session", "snapshot-source-operation", 1)?;
            std::fs::copy(artifact, &archive)?;
            let state = journal.snapshot()?;
            let spec = state.operations["snapshot-source-operation"]
                .restore_spec
                .as_ref()
                .ok_or("missing source specification")?;
            let data = serde_json::to_vec(&(spec, &descriptor))?;
            std::fs::write(root.join("snapshot.meta.json"), data)?;
            println!(
                "export_ms={} expanded_bytes={}",
                started.elapsed().as_millis(),
                descriptor.expanded_bytes
            );
            let recovery = Arc::new(Journal::open(
                root.join("recovery-journal"),
                "smoke-worker",
            )?);
            let mut pending = state.operations["snapshot-source-operation"].clone();
            pending.phase = Phase::Preparing;
            pending.archive = None;
            pending.artifact.clear();
            pending.verified = false;
            recovery.claim(pending)?;
            std::fs::copy(
                &archive,
                recovery.artifact_path("obx:smoke:session", "snapshot-source-operation", 1)?,
            )?;
            let recovered = backend
                .export_source(recovery.clone(), "snapshot-source-operation", limits)
                .await?;
            if recovered != descriptor
                || recovery.snapshot()?.operations["snapshot-source-operation"].phase
                    != Phase::Frozen
            {
                return Err(
                    "published archive acknowledgement recovery changed its identity".into(),
                );
            }
            println!(
                "PASS: published archive recovered without a journal completion acknowledgement"
            );
            std::fs::write(root.join("source-id"), litebox.id().as_str())?;
            let transfer =
                worker_boxlite::persistence::Transfer::new(4, std::time::Duration::from_secs(30))?;
            let source = transfer
                .source(
                    std::fs::File::open(&archive)?,
                    descriptor.clone(),
                    tokio_util::sync::CancellationToken::new(),
                )
                .await?;
            let mut parts = Vec::new();
            let mut offset = 0;
            let mut number = 1;
            while offset < descriptor.size_bytes {
                let size = (descriptor.size_bytes - offset).min(8 << 20);
                parts.push(worker_boxlite::persistence::UploadPart {
                    part_number: number,
                    grant: snapshot_store::grant(&endpoint, "PUT", size, offset, Some(number)),
                });
                number += 1;
                offset += size;
            }
            let mut receipts = Vec::new();
            for batch in parts.chunks(16) {
                receipts.extend(
                    transfer
                        .upload(&source, batch, tokio_util::sync::CancellationToken::new())
                        .await?,
                );
            }
            std::fs::write(
                root.join("uploaded-parts.json"),
                serde_json::to_vec(&receipts)?,
            )?;
            println!(
                "PASS: native archive uploaded in {} HTTP parts",
                receipts.len()
            );
        } else {
            let started = Instant::now();
            let (spec, descriptor): (RestoreSpec, Archive) =
                serde_json::from_slice(&std::fs::read(root.join("snapshot.meta.json"))?)?;
            let journal = Arc::new(Journal::open(root.join("target-journal"), "smoke-target")?);
            let initial = Operation {
                operation_id: "restore-operation".into(),
                scoped_session_id: "obx:smoke:session".into(),
                generation: 2,
                kind: Kind::Target,
                phase: Phase::Prepared,
                restore_spec: Some(spec.clone()),
                archive: Some(descriptor.clone()),
                artifact: worker_boxlite::persistence::artifact_name(
                    "obx:smoke:session",
                    "restore-operation",
                    2,
                ),
                box_id: String::new(),
                tombstone_box_id: String::new(),
                source_process: None,
                source_stop_confirmed: false,
                lease_expires_unix_ms: 0,
                staging_bytes: 65 << 30,
                snapshot_committed: false,
                verified: false,
                remote_verified: false,
                cache_until_unix_ms: 0,
                lease_ttl_sec: 300,
            };
            journal.claim(initial.clone())?;
            let transfer =
                worker_boxlite::persistence::Transfer::new(4, std::time::Duration::from_secs(30))?;
            let artifact = journal.artifact_path("obx:smoke:session", "restore-operation", 2)?;
            let downloaded = transfer
                .download(
                    &snapshot_store::grant(&endpoint, "GET", descriptor.size_bytes, 0, None),
                    &descriptor,
                    artifact.clone(),
                    tokio_util::sync::CancellationToken::new(),
                )
                .await?;
            drop(downloaded);
            let backend = NativeRuntime::new(runtime.clone(), root.join("target-home"))?;
            let limits = ArchiveLimits {
                max_bytes: 256 << 20,
                max_expanded_bytes: 64 << 30,
            };
            let legacy = runtime
                .create(
                    BoxOptions {
                        rootfs: RootfsSpec::Image(spec.image.clone()),
                        auto_delete: Some(0),
                        detach: true,
                        ..BoxOptions::default()
                    },
                    Some(terminal_name("obx:smoke:session")),
                )
                .await?;
            let budget = Arc::new(Journal::open(
                root.join("target-budget-journal"),
                "smoke-target",
            )?);
            let mut small = initial.clone();
            small.staging_bytes = 1;
            budget.claim(small)?;
            if !matches!(
                backend
                    .import_target(budget, "restore-operation", limits)
                    .await,
                Err(worker_boxlite::persistence::NativeError::Budget)
            ) || runtime.list_info().await?.len() != 1
            {
                return Err("target budget rejection changed native state".into());
            }
            let imported = backend
                .import_target(journal.clone(), "restore-operation", limits)
                .await?;
            if imported.box_id == legacy.id().as_str() {
                return Err("target import adopted the legacy stable session name".into());
            }
            let replay = backend
                .import_target(journal.clone(), "restore-operation", limits)
                .await?;
            if imported.box_id != replay.box_id || imported.status != BoxStatus::Stopped {
                return Err("native import retry changed identity or started an instance".into());
            }
            // Reconstruct the durable pre-ack state in an independent journal:
            // the SDK native import exists but its ID acknowledgement was lost.
            let recovery = Arc::new(Journal::open(
                root.join("target-recovery-journal"),
                "smoke-target",
            )?);
            recovery.claim(initial.clone())?;
            recovery.update("restore-operation", |op| {
                op.phase = Phase::Importing;
                op.verified = true;
                Ok(())
            })?;
            std::fs::copy(
                &artifact,
                recovery.artifact_path("obx:smoke:session", "restore-operation", 2)?,
            )?;
            let recovered = backend
                .import_target(recovery.clone(), "restore-operation", limits)
                .await?;
            if recovered.box_id != imported.box_id || runtime.list_info().await?.len() != 2 {
                return Err("lost import acknowledgement created or selected another instance".into());
            }
            println!("PASS: import retries and lost native ID acknowledgement select the same stopped operation instance");
            let rejected = Arc::new(Journal::open(
                root.join("target-rejected-journal"),
                "smoke-target",
            )?);
            rejected.claim(initial.clone())?;
            rejected.update("restore-operation", |op| {
                op.phase = Phase::Canceling;
                Ok(())
            })?;
            if !matches!(
                backend
                    .import_target(rejected.clone(), "restore-operation", limits)
                    .await,
                Err(worker_boxlite::persistence::NativeError::Conflict)
            ) {
                return Err("canceled target import was accepted".into());
            }
            let mut deletion = initial.clone();
            deletion.kind = Kind::Delete;
            deletion.phase = Phase::Deleting;
            deletion.operation_id = "target-delete".into();
            deletion.generation = 3;
            deletion.archive = None;
            deletion.artifact.clear();
            deletion.staging_bytes = 0;
            deletion.lease_ttl_sec = 0;
            rejected.claim(deletion)?;
            if !matches!(
                backend
                    .import_target(rejected, "restore-operation", limits)
                    .await,
                Err(worker_boxlite::persistence::NativeError::Conflict)
            ) {
                return Err("superseded target import was accepted".into());
            }
            let wrong = Arc::new(Journal::open(
                root.join("target-wrong-generation-journal"),
                "smoke-target",
            )?);
            let mut wrong_op = initial;
            wrong_op.generation = 3;
            wrong_op.artifact =
                worker_boxlite::persistence::artifact_name("obx:smoke:session", "restore-operation", 3);
            wrong_op.box_id = imported.box_id.clone();
            wrong.claim(wrong_op)?;
            wrong.update("restore-operation", |op| {
                op.phase = Phase::Importing;
                op.verified = true;
                Ok(())
            })?;
            if !matches!(
                backend
                    .import_target(wrong, "restore-operation", limits)
                    .await,
                Err(worker_boxlite::persistence::NativeError::Conflict)
            ) || runtime.list_info().await?.len() != 2
            {
                return Err("another generation adopted the existing target ID".into());
            }
            println!("PASS: budget, cancellation, supersession and wrong generation cannot adopt or create a target");

            let litebox = runtime
                .get(&imported.box_id)
                .await?
                .ok_or("selected import is missing")?;
            if litebox.info().await?.status != BoxStatus::Stopped {
                return Err("import implicitly started the target".into());
            }
            if std::fs::read_to_string(root.join("source-id"))? == litebox.id().as_str() {
                return Err("import reused the source identity".into());
            }
            println!("import_ms={}", started.elapsed().as_millis());
            journal.update("restore-operation", |op| {
                op.box_id = litebox.id().as_str().into();
                op.lease_expires_unix_ms = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map_err(|_| worker_boxlite::persistence::Error::Conflict)?
                    .as_millis() as i64
                    + 300_000;
                Ok(())
            })?;
            litebox.start().await?;
            let restored = backend
                .inspect_target(&journal, "restore-operation")
                .await?;
            if restored.spec != spec {
                return Err("restored native specification changed".into());
            }
            println!("{}", python(&litebox, VERIFY).await?);
            journal.update("restore-operation", |op| {
                op.phase = Phase::Ready;
                Ok(())
            })?;
            journal.update("restore-operation", |op| {
                op.phase = Phase::Active;
                Ok(())
            })?;
            let mut next = journal.snapshot()?.operations["restore-operation"].clone();
            next.operation_id = "resuspend-operation".into();
            next.kind = Kind::Source;
            next.phase = Phase::Preparing;
            next.archive = None;
            next.artifact.clear();
            next.verified = false;
            next.lease_ttl_sec = 0;
            journal.claim(next)?;
            let second = backend
                .export_source(
                    journal.clone(),
                    "resuspend-operation",
                    ArchiveLimits {
                        max_bytes: 256 << 20,
                        max_expanded_bytes: 64 << 30,
                    },
                )
                .await?;
            let mut second_file = std::fs::File::open(journal.artifact_path(
                "obx:smoke:session",
                "resuspend-operation",
                2,
            )?)?;
            worker_boxlite::persistence::verify_archive(
                &mut second_file,
                &spec,
                "obx:smoke:session",
                &second,
                ArchiveLimits {
                    max_bytes: 256 << 20,
                    max_expanded_bytes: 64 << 30,
                },
            )?;
            println!("PASS: an imported instance can be suspended again using its recorded ownership");
            runtime.remove(litebox.id().as_str(), true).await?;
            if !matches!(
                backend
                    .import_target(recovery, "restore-operation", limits)
                    .await,
                Err(worker_boxlite::persistence::NativeError::Missing)
            ) || runtime.list_info().await?.len() != 1
            {
                return Err("missing selected native ID was replaced by a new import".into());
            }
            runtime.remove(legacy.id().as_str(), true).await?;
            println!("PASS: missing selected native ID never falls back to another instance");
        }
        Ok(())
    }
    .await;
    let shutdown = runtime.shutdown(Some(10)).await;
    result?;
    shutdown?;
    Ok(())
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    if args.len() == 4 {
        return phase(&args[1], Path::new(&args[2]), &args[3]).await;
    }
    let image = std::env::var("BOXLITE_SNAPSHOT_SMOKE_IMAGE").unwrap_or_else(|_| IMAGE.to_owned());
    if !image.contains("@sha256:") {
        return Err("smoke image must use an immutable digest".into());
    }
    let root = std::env::temp_dir().join(format!(
        "onlyboxes-boxlite-snapshot-{}",
        uuid::Uuid::new_v4()
    ));
    std::fs::create_dir(&root)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700))?;
    }
    println!(
        "BoxLite 0.10.7 native archive acceptance; root={}",
        root.display()
    );
    let store = snapshot_store::Store::new(&root.join("object-store")).await?;
    let result = (|| -> Result<()> {
        let executable = std::env::current_exe()?;
        if !Command::new(&executable)
            .arg("source")
            .env("BOXLITE_SNAPSHOT_SMOKE_STORE_URL", &store.url)
            .arg(&root)
            .arg(&image)
            .status()?
            .success()
        {
            return Err("source process failed".into());
        }
        let (_, descriptor): (RestoreSpec, Archive) =
            serde_json::from_slice(&std::fs::read(root.join("snapshot.meta.json"))?)?;
        let parts: Vec<worker_boxlite::persistence::CompletedPart> =
            serde_json::from_slice(&std::fs::read(root.join("uploaded-parts.json"))?)?;
        store.complete(&parts, &descriptor)?;
        if !Command::new(&executable)
            .arg("verify")
            .arg(&root)
            .arg(&image)
            .env("BOXLITE_SNAPSHOT_SMOKE_STORE_URL", &store.url)
            .status()?
            .success()
        {
            return Err("remote verification failed".into());
        }
        // The target must use HTTP: remove all source runtime and archive state.
        std::fs::remove_dir_all(root.join("source-home"))?;
        std::fs::remove_dir_all(root.join("source-journal"))?;
        std::fs::remove_dir_all(root.join("recovery-journal"))?;
        std::fs::remove_dir_all(root.join("wrong-spec-journal"))?;
        std::fs::remove_dir_all(root.join("unproven-source-journal"))?;
        std::fs::remove_dir_all(root.join("interrupted-stop-journal"))?;
        std::fs::remove_file(root.join("snapshot.boxlite"))?;
        let mut archive = std::fs::File::open(&store.object)?;
        let mut digest = Sha256::new();
        let bytes = std::io::copy(&mut archive, &mut digest)?;
        println!("archive_bytes={bytes} sha256={:x}", digest.finalize());
        if !Command::new(&executable)
            .arg("target")
            .env("BOXLITE_SNAPSHOT_SMOKE_STORE_URL", &store.url)
            .arg(&root)
            .arg(&image)
            .status()?
            .success()
        {
            return Err("target process failed".into());
        }
        println!(
            "PASS: independent process and empty home restored archive after source home removal"
        );
        Ok(())
    })();
    if result.is_ok() {
        std::fs::remove_dir_all(&root)?;
    } else {
        eprintln!("retained failed acceptance artifacts at {}", root.display());
    }
    result
}
