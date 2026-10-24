//! Actual companion binary, with no Worker, VM or external service required.
use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};
use worker_boxlite::persistence::{LeaseGuardClient, LeaseGuardIdentity};

struct Fixture {
    root: PathBuf,
    child: Option<Child>,
}
impl Fixture {
    fn new() -> Self {
        Self {
            root: PathBuf::from("/tmp")
                .join(format!("obx-lp-{}", &uuid::Uuid::new_v4().to_string()[..8])),
            child: None,
        }
    }
    fn socket(&self) -> PathBuf {
        self.root.join("rpc/control.sock")
    }
    fn command(&self, store: Option<&str>) -> Command {
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_boxlite-lease-guard"));
        cmd.arg("--state-dir")
            .arg(self.root.join("state"))
            .arg("--socket-dir")
            .arg(self.root.join("rpc"))
            .arg("--worker-id")
            .arg("fixture-worker");
        if let Some(store) = store {
            cmd.arg("--store-id").arg(store);
        }
        cmd.stdout(Stdio::null()).stderr(Stdio::null());
        cmd
    }
    fn start(&mut self, store: Option<&str>) {
        assert!(self.child.is_none());
        self.child = Some(self.command(store).spawn().unwrap());
    }
    async fn ready(&mut self, store: Option<&str>) -> LeaseGuardIdentity {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            assert!(
                self.child.as_mut().unwrap().try_wait().unwrap().is_none(),
                "companion exited before readiness"
            );
            if let Ok(identity) =
                LeaseGuardClient::discover(self.socket(), "fixture-worker", store).await
            {
                return identity;
            }
            assert!(Instant::now() < deadline, "companion did not become ready");
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }
    async fn exited(&mut self) {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(status) = self.child.as_mut().unwrap().try_wait().unwrap() {
                assert!(status.success());
                self.child.take();
                return;
            }
            assert!(
                Instant::now() < deadline,
                "companion did not complete shutdown"
            );
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        if let Some(mut child) = self.child.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

#[tokio::test]
async fn real_companion_restarts_require_pinned_store_and_reject_old_clients() {
    let mut fixture = Fixture::new();
    fixture.start(None);
    let first = fixture.ready(None).await;
    assert_eq!(first.process.pid, fixture.child.as_ref().unwrap().id());
    let stale = LeaseGuardClient::bind(fixture.socket(), first.clone());
    assert_eq!(stale.status("missing").await.unwrap(), None);
    fixture.child.as_mut().unwrap().kill().unwrap();
    fixture.child.as_mut().unwrap().wait().unwrap();
    fixture.child.take();
    assert!(stale.status("missing").await.is_err());
    let status = fixture.command(None).status().unwrap();
    assert!(
        !status.success(),
        "existing ledger reopened without retained store identity"
    );
    fixture.start(Some(&first.store_id));
    let second = fixture.ready(Some(&first.store_id)).await;
    assert_eq!(first.store_id, second.store_id);
    assert_ne!(first.epoch_id, second.epoch_id);
    assert!(stale.status("missing").await.is_err());
    assert_eq!(
        LeaseGuardClient::bind(fixture.socket(), second)
            .status("missing")
            .await
            .unwrap(),
        None
    );
    let pid = fixture.child.as_ref().unwrap().id();
    // SAFETY: this exact owned, unreaped child still reserves its PID. Only
    // the fixture parent reaps it, after the requested graceful shutdown.
    assert_eq!(unsafe { libc::kill(pid as i32, libc::SIGTERM) }, 0);
    fixture.exited().await;
    assert!(!Path::new(&fixture.socket()).exists());
}

#[tokio::test]
async fn real_companion_diagnostics_distinguish_signal_stop_and_rejected_startup() {
    let mut fixture = Fixture::new();
    let child = fixture
        .command(None)
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    fixture.child = Some(child);
    fixture.ready(None).await;
    let mut diagnostics = fixture.child.as_mut().unwrap().stderr.take().unwrap();
    let pid = fixture.child.as_ref().unwrap().id();
    // SAFETY: this owned, unreaped child reserves its PID until exited() waits.
    assert_eq!(unsafe { libc::kill(pid as i32, libc::SIGTERM) }, 0);
    fixture.exited().await;
    let mut text = String::new();
    diagnostics.read_to_string(&mut text).unwrap();
    assert_eq!(text, "BoxLite lease guard stopped: SIGTERM\n");
    let output = fixture
        .command(None)
        .stderr(Stdio::piped())
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(1));
    assert_eq!(output.stderr, b"BoxLite lease guard is unavailable\n");
    let output = Command::new(env!("CARGO_BIN_EXE_boxlite-lease-guard"))
        .args(["--unknown", "private-argument-must-not-be-logged"])
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(1));
    assert_eq!(output.stderr, b"BoxLite lease guard is unavailable\n");
}
