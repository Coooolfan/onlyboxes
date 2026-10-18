//! Native cold-home export/import acceptance for the pinned BoxLite SDK.
//!
//! Run `cargo run --example boxlite_snapshot_smoke`. BOXLITE_SNAPSHOT_SMOKE_IMAGE
//! may select another immutable image containing Python and /bin/sh. All runtime
//! homes are private temporary directories owned by this invocation.

use std::error::Error;
use std::path::Path;
use std::process::Command;
use std::sync::Arc;
use std::time::Instant;

use boxlite::{
    BoxArchive, BoxCommand, BoxOptions, BoxStatus, BoxliteOptions, BoxliteRuntime, LiteBox,
    RootfsSpec,
};
use sha2::{Digest, Sha256};
use tokio_stream::StreamExt;
use worker_boxlite::persistence::{
    prepare_import_archive, terminal_name, Archive, ArchiveLimits, Journal, Kind, NativeRuntime,
    Operation, Phase, RestoreSpec,
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
            backend
                .stop_source(&journal, "snapshot-source-operation")
                .await?;
            backend
                .stop_source(&journal, "snapshot-source-operation")
                .await?;
            println!(
                "PASS: read-only native inspection, journal identity and idempotent source stop"
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
        } else {
            let started = Instant::now();
            let (spec, descriptor): (RestoreSpec, Archive) =
                serde_json::from_slice(&std::fs::read(root.join("snapshot.meta.json"))?)?;
            let staged = root.join("target-import.boxlite");
            let mut input = std::fs::File::open(&archive)?;
            let mut output = std::fs::OpenOptions::new()
                .read(true)
                .write(true)
                .create_new(true)
                .open(&staged)?;
            prepare_import_archive(
                &mut input,
                &mut output,
                &spec,
                "obx:smoke:session",
                &descriptor,
                ArchiveLimits {
                    max_bytes: 256 << 20,
                    max_expanded_bytes: 64 << 30,
                },
            )?;
            drop(output);
            let journal = Arc::new(Journal::open(root.join("target-journal"), "smoke-target")?);
            journal.claim(Operation {
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
                lease_expires_unix_ms: 0,
                staging_bytes: 65 << 30,
                snapshot_committed: false,
                verified: true,
                remote_verified: false,
                cache_until_unix_ms: 0,
                lease_ttl_sec: 300,
            })?;
            journal.update("restore-operation", |op| {
                op.phase = Phase::Importing;
                Ok(())
            })?;
            let litebox = runtime
                .import_box(
                    BoxArchive::new(&staged),
                    Some(terminal_name("obx:smoke:session")),
                )
                .await?;
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
            let backend = NativeRuntime::new(runtime.clone(), root.join("target-home"))?;
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
            println!(
                "PASS: an imported instance can be suspended again using its recorded ownership"
            );
            runtime.remove(litebox.id().as_str(), true).await?;
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
    let result = (|| -> Result<()> {
        let executable = std::env::current_exe()?;
        if !Command::new(&executable)
            .arg("source")
            .arg(&root)
            .arg(&image)
            .status()?
            .success()
        {
            return Err("source process failed".into());
        }
        // No source DB, image cache, base disks or box directories remain.
        std::fs::remove_dir_all(root.join("source-home"))?;
        std::fs::remove_dir_all(root.join("source-journal"))?;
        std::fs::remove_dir_all(root.join("recovery-journal"))?;
        std::fs::remove_dir_all(root.join("wrong-spec-journal"))?;
        let mut archive = std::fs::File::open(root.join("snapshot.boxlite"))?;
        let mut digest = Sha256::new();
        let bytes = std::io::copy(&mut archive, &mut digest)?;
        println!("archive_bytes={bytes} sha256={:x}", digest.finalize());
        if !Command::new(&executable)
            .arg("target")
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
