#![cfg(feature = "integration")]

//! Runtime smoke tests for worker-boxlite's Boxlite backend.
//!
//! These tests require a working Boxlite runtime (macOS Hypervisor.framework or Linux KVM).
//! Run with: `cargo test --features integration`

use boxlite::{
    BoxCommand, BoxOptions, BoxStatus, BoxliteOptions, BoxliteRuntime, LiteBox, RootfsSpec,
};
use std::io::{BufRead, BufReader};
use std::net::SocketAddr;
use std::process::{Command, Stdio};
use std::sync::{Arc, OnceLock};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
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

async fn http_get_via_tunnel(litebox: &LiteBox, port: u16, expected_body: &[u8]) -> Vec<u8> {
    let guest_ip = boxlite::net::constants::GUEST_IP
        .parse()
        .expect("parse BoxLite guest IP");
    let target = SocketAddr::new(guest_ip, port);
    tokio::time::timeout(std::time::Duration::from_secs(10), async {
        loop {
            match litebox.network().tunnel(target).await {
                Ok(tunnel) => {
                    let mut connection = tunnel.connect().expect("connect prepared tunnel");
                    connection
                        .write_all(
                            b"GET / HTTP/1.1\r\nHost: preview.test\r\nConnection: close\r\n\r\n",
                        )
                        .await
                        .expect("write tunnel request");
                    let mut response = Vec::new();
                    match connection.read_to_end(&mut response).await {
                        Ok(_)
                            if response
                                .windows(expected_body.len())
                                .any(|part| part == expected_body) =>
                        {
                            break response;
                        }
                        _ => tokio::time::sleep(std::time::Duration::from_millis(100)).await,
                    }
                }
                Err(_) => tokio::time::sleep(std::time::Duration::from_millis(100)).await,
            }
        }
    })
    .await
    .expect("guest HTTP server reachable through tunnel")
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
                auto_delete: Some(0),
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
                auto_delete: Some(0),
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
async fn test_terminal_service_is_reachable_through_box_tunnel() {
    let rt = runtime();
    let name = random_name("tunnel");
    let litebox = rt
        .create(
            BoxOptions {
                cpus: Some(1),
                memory_mib: Some(512),
                rootfs: RootfsSpec::Image("python:slim".into()),
                auto_delete: Some(0),
                detach: true,
                ..Default::default()
            },
            Some(name.clone()),
        )
        .await
        .expect("create tunnel VM");
    litebox.start().await.expect("start tunnel VM");

    let server = litebox
        .exec(BoxCommand::new("sh").args([
            "-lc",
            "printf tunnel-ok > /tmp/index.html && python3 -m http.server 8080 --directory /tmp >/tmp/http.log 2>&1 &",
        ]))
        .await
        .expect("start guest HTTP server");
    assert_eq!(
        server
            .wait()
            .await
            .expect("wait for server start")
            .exit_code,
        0
    );

    let response = http_get_via_tunnel(&litebox, 8080, b"tunnel-ok").await;

    assert!(response.starts_with(b"HTTP/1.0 200 OK") || response.starts_with(b"HTTP/1.1 200 OK"));
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
                auto_delete: Some(0),
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

#[tokio::test]
async fn test_detached_terminal_survives_runtime_restart_and_keeps_files() {
    let home = std::path::PathBuf::from(format!(
        "/tmp/obx-recovery-it-{}-{:08x}",
        std::process::id(),
        rand::random::<u32>()
    ));
    let name = random_name("onlyboxes-terminal-v1-recovery");
    let mut options = BoxliteOptions::default();
    options.home_dir = home.clone();
    let first_runtime = Arc::new(BoxliteRuntime::new(options.clone()).expect("first runtime"));
    let litebox = first_runtime
        .get_or_create(
            BoxOptions {
                cpus: Some(1),
                memory_mib: Some(512),
                rootfs: RootfsSpec::Image("python:slim".into()),
                auto_delete: Some(0),
                detach: true,
                entrypoint: Some(vec!["sh".into(), "-lc".into()]),
                cmd: Some(vec!["while true; do sleep 3600; done".into()]),
                ..Default::default()
            },
            Some(name.clone()),
        )
        .await
        .expect("create detached terminal")
        .0;
    litebox.start().await.expect("start detached terminal");
    let write = litebox
        .exec(BoxCommand::new("sh").args([
            "-lc",
            "printf recovery-ok > /tmp/onlyboxes-recovery.txt; printf recovery-tunnel-ok > /tmp/index.html; python3 -m http.server 8080 --directory /tmp >/tmp/http.log 2>&1 &",
        ]))
        .await
        .expect("write persistent file");
    assert_eq!(write.wait().await.expect("wait for write").exit_code, 0);
    drop(litebox);
    first_runtime
        .shutdown(None)
        .await
        .expect("shutdown first runtime");
    drop(first_runtime);

    let second_runtime = Arc::new(BoxliteRuntime::new(options).expect("second runtime"));
    let recovered = second_runtime
        .get(&name)
        .await
        .expect("lookup by deterministic name")
        .expect("detached terminal survived runtime shutdown");
    match recovered
        .info()
        .await
        .expect("inspect recovered terminal")
        .status
    {
        BoxStatus::Running => {}
        BoxStatus::Configured | BoxStatus::Stopped => {
            recovered.start().await.expect("restart recovered terminal")
        }
        status => panic!("unexpected recovered status: {status:?}"),
    }
    let mut read = recovered
        .exec(BoxCommand::new("sh").args(["-lc", "cat /tmp/onlyboxes-recovery.txt"]))
        .await
        .expect("read persistent file");
    let mut stdout = String::new();
    if let Some(mut stream) = read.stdout() {
        while let Some(chunk) = stream.next().await {
            stdout.push_str(&chunk);
        }
    }
    assert_eq!(read.wait().await.expect("wait for read").exit_code, 0);
    assert_eq!(stdout, "recovery-ok");
    let response = http_get_via_tunnel(&recovered, 8080, b"recovery-tunnel-ok").await;
    assert!(response.starts_with(b"HTTP/1.0 200 OK") || response.starts_with(b"HTTP/1.1 200 OK"));
    second_runtime
        .remove(&name, true)
        .await
        .expect("remove recovered terminal");
}

#[tokio::test]
async fn boxlite_forced_termination_helper() {
    if std::env::var("BOXLITE_CRASH_HELPER").ok().as_deref() != Some("1") {
        return;
    }
    let home =
        std::path::PathBuf::from(std::env::var("BOXLITE_CRASH_HOME").expect("BOXLITE_CRASH_HOME"));
    let name = std::env::var("BOXLITE_CRASH_NAME").expect("BOXLITE_CRASH_NAME");
    let mut options = BoxliteOptions::default();
    options.home_dir = home;
    let runtime = BoxliteRuntime::new(options).expect("crash helper runtime");
    let litebox = runtime
        .get_or_create(detached_terminal_options(), Some(name))
        .await
        .expect("crash helper create")
        .0;
    litebox.start().await.expect("crash helper start");
    let write = litebox
        .exec(BoxCommand::new("sh").args([
            "-lc",
            "printf forced-recovery-ok > /tmp/onlyboxes-forced-recovery.txt",
        ]))
        .await
        .expect("crash helper write");
    assert_eq!(write.wait().await.expect("crash helper wait").exit_code, 0);
    println!("BOXLITE_CRASH_HELPER_READY");
    std::io::Write::flush(&mut std::io::stdout()).expect("flush helper readiness");
    std::future::pending::<()>().await;
}

#[tokio::test]
async fn test_detached_terminal_survives_forced_worker_termination() {
    let home = std::path::PathBuf::from(format!(
        "/tmp/obx-crash-it-{}-{:08x}",
        std::process::id(),
        rand::random::<u32>()
    ));
    let name = random_name("onlyboxes-terminal-v1-crash-recovery");
    let mut child = Command::new(std::env::current_exe().expect("current test executable"))
        .args([
            "--exact",
            "boxlite_forced_termination_helper",
            "--nocapture",
        ])
        .env("BOXLITE_CRASH_HELPER", "1")
        .env("BOXLITE_CRASH_HOME", &home)
        .env("BOXLITE_CRASH_NAME", &name)
        .stdout(Stdio::piped())
        .spawn()
        .expect("spawn crash helper");
    let stdout = child.stdout.take().expect("crash helper stdout");
    let mut ready = false;
    for line in BufReader::new(stdout).lines() {
        let line = line.expect("read crash helper output");
        if line.contains("BOXLITE_CRASH_HELPER_READY") {
            ready = true;
            break;
        }
    }
    assert!(
        ready,
        "crash helper exited before creating the detached Box"
    );
    child.kill().expect("force terminate crash helper");
    let status = child.wait().expect("wait for crash helper termination");
    assert!(!status.success(), "crash helper was not force terminated");

    let mut options = BoxliteOptions::default();
    options.home_dir = home;
    let runtime = BoxliteRuntime::new(options).expect("recovery runtime after forced termination");
    let recovered = runtime
        .get(&name)
        .await
        .expect("lookup after forced termination")
        .expect("detached Box survived forced worker termination");
    match recovered
        .info()
        .await
        .expect("inspect recovered Box")
        .status
    {
        BoxStatus::Running => {}
        BoxStatus::Configured | BoxStatus::Stopped => {
            recovered.start().await.expect("restart recovered Box")
        }
        status => panic!("unexpected recovered status: {status:?}"),
    }
    let mut read = recovered
        .exec(BoxCommand::new("sh").args(["-lc", "cat /tmp/onlyboxes-forced-recovery.txt"]))
        .await
        .expect("read after forced termination");
    let mut stdout = String::new();
    if let Some(mut stream) = read.stdout() {
        while let Some(chunk) = stream.next().await {
            stdout.push_str(&chunk);
        }
    }
    assert_eq!(read.wait().await.expect("wait for read").exit_code, 0);
    assert_eq!(stdout, "forced-recovery-ok");
    runtime
        .remove(&name, true)
        .await
        .expect("remove recovered Box");
}

fn detached_terminal_options() -> BoxOptions {
    BoxOptions {
        cpus: Some(1),
        memory_mib: Some(512),
        rootfs: RootfsSpec::Image("alpine:latest".into()),
        auto_delete: Some(0),
        detach: true,
        entrypoint: Some(vec!["sh".into(), "-lc".into()]),
        cmd: Some(vec!["while true; do sleep 3600; done".into()]),
        ..Default::default()
    }
}
