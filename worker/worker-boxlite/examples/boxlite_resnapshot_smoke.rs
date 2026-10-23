//! Native SDK export/import/export regression. Owns only fresh temporary homes.
//! Run with `cargo run --example boxlite_resnapshot_smoke`.
use boxlite::{BoxCommand, BoxOptions, BoxliteOptions, BoxliteRuntime, RootfsSpec};
use std::{error::Error, path::Path};
use tokio_stream::StreamExt;
type Result<T> = std::result::Result<T, Box<dyn Error>>;
async fn write(b: &boxlite::LiteBox, name: &str) -> Result<()> {
    let script = format!("from pathlib import Path; import os; p=Path('/root/{name}'); p.write_bytes(bytes(range(256))*4096+b'0123456789abcdef0123456789abcdef'); os.chmod(p,0o640); os.sync(); print('written')");
    let mut exec = b
        .exec(BoxCommand::new("python3").args(["-c", &script]))
        .await?;
    let stdout = exec.stdout();
    let drain = tokio::spawn(async move {
        if let Some(mut s) = stdout {
            while s.next().await.is_some() {}
        }
    });
    let status = exec.wait().await?;
    drain.await?;
    if status.exit_code != 0 {
        return Err("guest write failed".into());
    }
    Ok(())
}
fn runtime(home: &Path) -> Result<BoxliteRuntime> {
    Ok(BoxliteRuntime::new(BoxliteOptions {
        home_dir: home.to_owned(),
        ..Default::default()
    })?)
}
#[tokio::main]
async fn main() -> Result<()> {
    let root = std::env::temp_dir().join(format!("onlyboxes-resnapshot-{}", uuid::Uuid::new_v4()));
    std::fs::create_dir(&root)?;
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700))?;
    println!("evidence={}", root.display());
    let source = runtime(&root.join("source"))?;
    let outcome = async {
        let b = source.create(BoxOptions {
            cpus: Some(1), memory_mib: Some(512),
            rootfs: RootfsSpec::Image("python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258".into()),
            auto_delete: Some(0), detach: true,
            entrypoint: Some(vec!["sh".into(), "-lc".into()]),
            cmd: Some(vec!["while true; do sleep 3600; done".into()]),
            ..Default::default()
        }, None).await?;
        b.start().await?;
        write(&b, "resnapshot-first").await?;
        b.stop().await?;
        b.export(Default::default(), &root.join("first.boxlite")).await?;
        println!("PASS: source exported");
        let target = runtime(&root.join("target"))?;
        let result: Result<()> = async {
            let imported = target.import_box(boxlite::BoxArchive::new(root.join("first.boxlite")), None).await?;
            imported.start().await?;
            write(&imported, "resnapshot-second").await?;
            imported.stop().await?;
            imported.export(Default::default(), &root.join("second.boxlite")).await?;
            println!("PASS: native export/import/write/export");
            Ok(())
        }.await;
        target.shutdown(Some(10)).await?;
        result
    }.await;
    source.shutdown(Some(10)).await?;
    if outcome.is_ok() {
        std::fs::remove_dir_all(root)?;
    }
    outcome
}
