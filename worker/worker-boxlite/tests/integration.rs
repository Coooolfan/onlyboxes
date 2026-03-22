#![cfg(feature = "integration")]

//! Integration tests for worker-boxlite capabilities.
//!
//! These tests require a working Boxlite runtime (macOS Hypervisor.framework or Linux KVM).
//! Run with: `cargo test --features integration`
//!
//! The `BOXLITE_RUNTIME_DIR` environment variable must point to a directory containing
//! boxlite-guest, boxlite-shim, mke2fs, and debugfs binaries.
//!
//! All tests share a single BoxliteRuntime to avoid lock conflicts on `~/.boxlite`.

use boxlite::{BoxCommand, BoxOptions, BoxliteRuntime, RootfsSpec};
use futures::StreamExt;
use std::sync::{Arc, OnceLock};

/// Shared runtime singleton — BoxliteRuntime locks `~/.boxlite` exclusively,
/// so only one instance can exist per process.
fn runtime() -> Arc<BoxliteRuntime> {
    static RT: OnceLock<Arc<BoxliteRuntime>> = OnceLock::new();
    RT.get_or_init(|| {
        Arc::new(
            BoxliteRuntime::with_defaults()
                .expect("Boxlite runtime required for integration tests"),
        )
    })
    .clone()
}

fn random_name(prefix: &str) -> String {
    format!("{}-test-{:08x}", prefix, rand::random::<u32>())
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

    let mut exec = litebox
        .exec(BoxCommand::new("python3").args(["-c", "print('hello from boxlite')"]))
        .await
        .expect("exec");

    let mut stdout = String::new();
    if let Some(mut stream) = exec.stdout() {
        while let Some(chunk) = stream.next().await {
            stdout.push_str(&chunk);
        }
    }

    let result = exec.wait().await.expect("wait");
    assert_eq!(result.exit_code, 0);
    assert!(stdout.contains("hello from boxlite"));

    let _ = rt.remove(&name, true).await;
}

#[tokio::test]
async fn test_python_exec_nonzero_exit() {
    let rt = runtime();
    let name = random_name("pyexit");

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

    let mut exec = litebox
        .exec(BoxCommand::new("python3").args(["-c", "import sys; sys.exit(42)"]))
        .await
        .expect("exec");

    let result = exec.wait().await.expect("wait");
    assert_eq!(result.exit_code, 42);

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

    // First command: create a file
    let mut exec1 = litebox
        .exec(BoxCommand::new("sh").args(["-lc", "echo hello > /tmp/test.txt"]))
        .await
        .expect("exec1");
    let r1 = exec1.wait().await.expect("wait1");
    assert_eq!(r1.exit_code, 0);

    // Second command: read the file (proves state persistence)
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
    let r2 = exec2.wait().await.expect("wait2");
    assert_eq!(r2.exit_code, 0);
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

    // VM should exist
    assert!(rt.exists(&name).await.expect("exists check"));

    // Remove it
    rt.remove(&name, true).await.expect("remove");

    // VM should no longer exist
    assert!(!rt.exists(&name).await.expect("exists check after remove"));
}
