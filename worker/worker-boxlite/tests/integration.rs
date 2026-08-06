#![cfg(feature = "integration")]

//! Runtime smoke tests for worker-boxlite's Boxlite backend.
//!
//! These tests require a working Boxlite runtime (macOS Hypervisor.framework or Linux KVM).
//! Run with: `cargo test --features integration`

use boxlite::{BoxCommand, BoxOptions, BoxliteOptions, BoxliteRuntime, RootfsSpec};
use std::sync::{Arc, OnceLock};
use tokio_stream::StreamExt;

struct SharedRuntime {
    runtime: Arc<BoxliteRuntime>,
}

fn runtime() -> Arc<BoxliteRuntime> {
    static RT: OnceLock<SharedRuntime> = OnceLock::new();
    RT.get_or_init(|| {
        let mut options = BoxliteOptions::default();
        options.home_dir = std::path::PathBuf::from(format!(
            "/tmp/obx-it-{}-{:08x}",
            std::process::id(),
            rand::random::<u32>()
        ));

        SharedRuntime {
            runtime: Arc::new(
                BoxliteRuntime::new(options)
                    .expect("Boxlite runtime required for integration tests"),
            ),
        }
    })
    .runtime
    .clone()
}

fn random_name(prefix: &str) -> String {
    format!("{prefix}-test-{:08x}", rand::random::<u32>())
}

#[tokio::test]
async fn test_python_exec_hello_world() {
    let rt = runtime();
    let name = random_name("pyexec");

    let litebox = rt
        .create(
            BoxOptions {
                cpus: Some(1),
                memory_mib: Some(512),
                rootfs: RootfsSpec::Image("python:slim".into()),
                auto_remove: true,
                ..Default::default()
            },
            Some(name.clone()),
        )
        .await
        .expect("create VM");

    litebox.start().await.expect("start VM");

    let mut execution = litebox
        .exec(BoxCommand::new("python3").args(["-c", "print('hello from boxlite')"]))
        .await
        .expect("exec");

    let mut stdout = String::new();
    if let Some(mut stream) = execution.stdout() {
        while let Some(chunk) = stream.next().await {
            stdout.push_str(&chunk);
        }
    }
    let result = execution.wait().await.expect("wait");
    assert_eq!(result.exit_code, 0);
    assert!(stdout.contains("hello from boxlite"));

    let _ = rt.remove(&name, true).await;
}

#[tokio::test]
async fn test_terminal_session_create_and_reuse() {
    let rt = runtime();
    let name = random_name("term");

    let litebox = rt
        .create(
            BoxOptions {
                cpus: Some(1),
                memory_mib: Some(512),
                rootfs: RootfsSpec::Image("python:slim".into()),
                auto_remove: false,
                ..Default::default()
            },
            Some(name.clone()),
        )
        .await
        .expect("create VM");

    litebox.start().await.expect("start VM");

    let exec1 = litebox
        .exec(BoxCommand::new("sh").args(["-lc", "echo hello > /tmp/test.txt"]))
        .await
        .expect("exec1");
    let result1 = exec1.wait().await.expect("wait1");
    assert_eq!(result1.exit_code, 0);

    let mut exec2 = litebox
        .exec(BoxCommand::new("sh").args(["-lc", "cat /tmp/test.txt"]))
        .await
        .expect("exec2");

    let mut stdout = String::new();
    if let Some(mut stream) = exec2.stdout() {
        while let Some(chunk) = stream.next().await {
            stdout.push_str(&chunk);
        }
    }
    let result2 = exec2.wait().await.expect("wait2");
    assert_eq!(result2.exit_code, 0);
    assert!(stdout.contains("hello"));

    let _ = rt.remove(&name, true).await;
}

#[tokio::test]
async fn test_vm_stop_and_cleanup() {
    let rt = runtime();
    let name = random_name("cleanup");

    let litebox = rt
        .create(
            BoxOptions {
                cpus: Some(1),
                memory_mib: Some(512),
                rootfs: RootfsSpec::Image("alpine:latest".into()),
                auto_remove: false,
                ..Default::default()
            },
            Some(name.clone()),
        )
        .await
        .expect("create VM");

    litebox.start().await.expect("start VM");
    assert!(rt.exists(&name).await.expect("exists check"));

    rt.remove(&name, true).await.expect("remove");
    assert!(!rt.exists(&name).await.expect("exists check after remove"));
}
