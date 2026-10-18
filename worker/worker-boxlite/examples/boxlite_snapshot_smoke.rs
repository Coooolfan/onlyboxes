//! Native cold-home export/import acceptance for the pinned BoxLite SDK.
//!
//! Run `cargo run --example boxlite_snapshot_smoke`. BOXLITE_SNAPSHOT_SMOKE_IMAGE
//! may select another immutable image containing Python and /bin/sh. All runtime
//! homes are private temporary directories owned by this invocation.

use std::error::Error;
use std::path::Path;
use std::process::Command;
use std::time::Instant;

use boxlite::{
    BoxArchive, BoxCommand, BoxOptions, BoxStatus, BoxliteOptions, BoxliteRuntime, ExportOptions,
    LiteBox, RootfsSpec,
};
use sha2::{Digest, Sha256};
use tokio_stream::StreamExt;

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
os.sync()
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

fn runtime(home: &Path) -> Result<BoxliteRuntime> {
    Ok(BoxliteRuntime::new(BoxliteOptions {
        home_dir: home.to_path_buf(),
        ..Default::default()
    })?)
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
                    Some("snapshot-source".to_owned()),
                )
                .await?;
            litebox.start().await?;
            println!("{}", python(&litebox, WRITE).await?);
            litebox.stop().await?;
            if litebox.info().await?.status != BoxStatus::Stopped {
                return Err("source did not stop all writers".into());
            }
            let started = Instant::now();
            litebox.export(ExportOptions::default(), &archive).await?;
            println!("export_ms={}", started.elapsed().as_millis());
            std::fs::write(root.join("source-id"), litebox.id().as_str())?;
        } else {
            let started = Instant::now();
            let litebox = runtime
                .import_box(
                    BoxArchive::new(&archive),
                    Some("snapshot-target".to_owned()),
                )
                .await?;
            if litebox.info().await?.status != BoxStatus::Stopped {
                return Err("import implicitly started the target".into());
            }
            if std::fs::read_to_string(root.join("source-id"))? == litebox.id().as_str() {
                return Err("import reused the source identity".into());
            }
            println!("import_ms={}", started.elapsed().as_millis());
            litebox.start().await?;
            println!("{}", python(&litebox, VERIFY).await?);
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
