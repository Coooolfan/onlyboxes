use super::*;

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let path = PathBuf::from("/tmp")
            .join(format!("obx-lg-{}", &uuid::Uuid::new_v4().to_string()[..8]));
        private_directory(&path).unwrap();
        Self(path)
    }
    fn ledger(&self) -> Arc<ExecutionLeaseLedger> {
        Arc::new(ExecutionLeaseLedger::open(self.0.join("state"), "worker").unwrap())
    }
    fn socket(&self) -> PathBuf {
        self.0.join("rpc/control.sock")
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

#[tokio::test]
async fn private_readiness_and_stale_epochs_do_not_create_missing_ownership() {
    let root = Root::new();
    let ledger = root.ledger();
    let server = LeaseGuardServer::bind(ledger.clone(), root.0.join("rpc"))
        .await
        .unwrap();
    let identity = server.identity().clone();
    let shutdown = CancellationToken::new();
    let task = tokio::spawn(server.run(shutdown.clone()));
    let discovered = LeaseGuardClient::discover(root.socket(), "worker", Some(ledger.store_id()))
        .await
        .unwrap();
    assert_eq!(discovered, identity);
    assert!(
        LeaseGuardClient::discover(root.socket(), "other-worker", Some(ledger.store_id()))
            .await
            .is_err()
    );
    let client = LeaseGuardClient::bind(root.socket(), identity.clone());
    assert_eq!(client.status("missing").await, Ok(None));
    let mut stale = identity;
    stale.epoch_id = uuid::Uuid::new_v4().to_string();
    assert_eq!(
        LeaseGuardClient::bind(root.socket(), stale)
            .status("missing")
            .await,
        Err(Error::Unavailable)
    );
    shutdown.cancel();
    task.await.unwrap().unwrap();
    assert!(!root.socket().exists());
}

#[tokio::test]
async fn another_listener_or_nonprivate_endpoint_cannot_be_overwritten() {
    let root = Root::new();
    let ledger = root.ledger();
    let server = LeaseGuardServer::bind(ledger.clone(), root.0.join("rpc"))
        .await
        .unwrap();
    assert!(LeaseGuardServer::bind(ledger.clone(), root.0.join("rpc"))
        .await
        .is_err());
    let before = fs::symlink_metadata(root.socket()).unwrap().ino();
    assert_eq!(fs::symlink_metadata(root.socket()).unwrap().ino(), before);
    drop(server);
    let ledger =
        Arc::new(ExecutionLeaseLedger::open(root.0.join("other-state"), "worker").unwrap());
    let path = root.0.join("bad");
    private_directory(&path).unwrap();
    fs::write(path.join("control.sock"), b"unrelated data").unwrap();
    assert!(LeaseGuardServer::bind(ledger, path.clone()).await.is_err());
    assert_eq!(
        fs::read(path.join("control.sock")).unwrap(),
        b"unrelated data"
    );
}

#[tokio::test]
async fn partial_and_oversized_frames_do_not_hold_control_slots_indefinitely() {
    let root = Root::new();
    let ledger = root.ledger();
    let server = LeaseGuardServer::bind(ledger.clone(), root.0.join("rpc"))
        .await
        .unwrap();
    let shutdown = CancellationToken::new();
    let task = tokio::spawn(server.run(shutdown.clone()));
    let mut slow = Vec::new();
    for _ in 0..MAX_HANDLERS {
        let mut stream = UnixStream::connect(root.socket()).await.unwrap();
        stream.write_all(&[0]).await.unwrap();
        slow.push(stream);
    }
    tokio::time::sleep(IO_TIMEOUT + Duration::from_millis(100)).await;
    let id = LeaseGuardClient::discover(root.socket(), "worker", Some(ledger.store_id()))
        .await
        .unwrap();
    let mut oversized = UnixStream::connect(root.socket()).await.unwrap();
    oversized.write_u32((MAX_FRAME + 1) as u32).await.unwrap();
    let mut byte = [0];
    assert_eq!(oversized.read(&mut byte).await.unwrap(), 0);
    let client = LeaseGuardClient::bind(root.socket(), id);
    assert_eq!(client.status("missing").await, Ok(None));
    shutdown.cancel();
    task.await.unwrap().unwrap();
}

#[tokio::test]
async fn socket_cleanup_preserves_a_replacement_endpoint() {
    let root = Root::new();
    let ledger = root.ledger();
    let server = LeaseGuardServer::bind(ledger, root.0.join("rpc"))
        .await
        .unwrap();
    fs::remove_file(root.socket()).unwrap();
    fs::write(root.socket(), b"replacement").unwrap();
    drop(server);
    assert_eq!(fs::read(root.socket()).unwrap(), b"replacement");
}

#[tokio::test]
async fn malformed_replies_cannot_confirm_readiness() {
    for fault in ["nonce", "store", "epoch", "pid", "version"] {
        let root = Root::new();
        private_directory(&root.0.join("rpc")).unwrap();
        let listener = UnixListener::bind(root.socket()).unwrap();
        fs::set_permissions(root.socket(), fs::Permissions::from_mode(0o600)).unwrap();
        let store = uuid::Uuid::new_v4().to_string();
        let expected = store.clone();
        let task = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let request: Request = read_frame(&mut stream).await.unwrap();
            let mut response = Response {
                version: VERSION,
                request_id: request.request_id,
                identity: LeaseGuardIdentity {
                    worker_id: "worker".into(),
                    store_id: store,
                    epoch_id: uuid::Uuid::new_v4().to_string(),
                    process: HostProcess::capture(std::process::id()).unwrap(),
                },
                reply: Reply::Ready,
            };
            match fault {
                "nonce" => response.request_id = uuid::Uuid::new_v4().to_string(),
                "store" => response.identity.store_id = uuid::Uuid::new_v4().to_string(),
                "epoch" => response.identity.epoch_id = "invalid".into(),
                "pid" => response.identity.process.pid += 1,
                _ => response.version += 1,
            }
            write_frame(&mut stream, &response).await.unwrap();
        });
        assert!(
            LeaseGuardClient::discover(root.socket(), "worker", Some(&expected))
                .await
                .is_err(),
            "{fault}"
        );
        task.await.unwrap();
    }
}

#[cfg(target_os = "macos")]
mod native {
    use super::*;
    struct Child(std::process::Child);
    impl Child {
        fn new() -> Self {
            Self(
                std::process::Command::new("sleep")
                    .arg("30")
                    .spawn()
                    .unwrap(),
            )
        }
        fn lease(&self, ttl: i64) -> ExecutionLease {
            let mut process = HostProcess::capture(self.0.id()).unwrap();
            process.bind_writer_group("owned_box").unwrap();
            ExecutionLease {
                box_id: "owned_box".into(),
                scoped_session_id: "obx:owner:session".into(),
                generation: 1,
                revision: 1,
                expires_unix_ms: std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_millis() as i64
                    + ttl,
                process,
                revoked: false,
            }
        }
        async fn exited(&mut self) {
            let end = std::time::Instant::now() + Duration::from_secs(3);
            while self.0.try_wait().unwrap().is_none() {
                assert!(std::time::Instant::now() < end, "writer did not stop");
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        }
    }
    impl Drop for Child {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    #[tokio::test]
    async fn expiry_survives_slow_connections_and_a_lost_adoption_reply() {
        let root = Root::new();
        let ledger = root.ledger();
        let mut child = Child::new();
        let lease = child.lease(500);
        let server = LeaseGuardServer::bind(ledger.clone(), root.0.join("rpc"))
            .await
            .unwrap();
        let id = server.identity().clone();
        let shutdown = CancellationToken::new();
        let task = tokio::spawn(server.run(shutdown.clone()));
        let mut stream = UnixStream::connect(root.socket()).await.unwrap();
        write_frame(
            &mut stream,
            &Request {
                version: VERSION,
                request_id: uuid::Uuid::new_v4().to_string(),
                worker_id: "worker".into(),
                store_id: Some(id.store_id.clone()),
                expected: Some(id.clone()),
                command: Command::Adopt {
                    lease: lease.clone(),
                },
            },
        )
        .await
        .unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(1);
        while ledger.status(&lease.box_id).unwrap().is_none() {
            assert!(std::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        // Discard the reply only after the mutation is accepted. Disconnect
        // before acceptance may legitimately reject the request at peer checks.
        drop(stream);
        let client = LeaseGuardClient::bind(root.socket(), id);
        assert_eq!(client.adopt(lease.clone()).await, Ok(lease.clone()));
        let mut stalled = Vec::new();
        for _ in 0..MAX_HANDLERS {
            let mut stream = UnixStream::connect(root.socket()).await.unwrap();
            stream.write_all(&[0]).await.unwrap();
            stalled.push(stream);
        }
        child.exited().await;
        assert!(ledger.status(&lease.box_id).unwrap().unwrap().revoked);
        drop(stalled);
        shutdown.cancel();
        task.await.unwrap().unwrap();
    }

    #[tokio::test]
    async fn shutdown_revokes_and_confirms_native_exit_before_returning() {
        let root = Root::new();
        let ledger = root.ledger();
        let mut child = Child::new();
        let lease = child.lease(10_000);
        let server = LeaseGuardServer::bind(ledger, root.0.join("rpc"))
            .await
            .unwrap();
        let id = server.identity().clone();
        let shutdown = CancellationToken::new();
        let task = tokio::spawn(server.run(shutdown.clone()));
        let client = LeaseGuardClient::bind(root.socket(), id);
        client.adopt(lease.clone()).await.unwrap();
        shutdown.cancel();
        child.exited().await;
        task.await.unwrap().unwrap();
        assert!(lease.process.writers_exited().unwrap());
    }

    #[tokio::test]
    async fn endpoint_failure_still_stops_loaded_writers() {
        let root = Root::new();
        let ledger = root.ledger();
        let mut child = Child::new();
        ledger.adopt(child.lease(10_000)).unwrap();
        let directory = root.0.join("bad");
        private_directory(&directory).unwrap();
        fs::write(directory.join("control.sock"), b"unrelated").unwrap();
        let task = tokio::spawn(LeaseGuardServer::serve(
            ledger,
            directory.clone(),
            CancellationToken::new(),
        ));
        child.exited().await;
        assert_eq!(task.await.unwrap(), Err(Error::Unavailable));
        assert_eq!(
            fs::read(directory.join("control.sock")).unwrap(),
            b"unrelated"
        );
    }

    #[tokio::test]
    async fn dropping_the_service_future_keeps_native_enforcement_alive_until_exit() {
        let root = Root::new();
        let ledger = root.ledger();
        let mut child = Child::new();
        let lease = child.lease(10_000);
        let server = LeaseGuardServer::bind(ledger, root.0.join("rpc"))
            .await
            .unwrap();
        let client = LeaseGuardClient::bind(root.socket(), server.identity().clone());
        let task = tokio::spawn(server.run(CancellationToken::new()));
        client.adopt(lease.clone()).await.unwrap();
        task.abort();
        assert!(task.await.unwrap_err().is_cancelled());
        child.exited().await;
        assert!(lease.process.writers_exited().unwrap());
    }

    #[tokio::test]
    async fn shutdown_waits_for_accepted_mutations_while_stopping_writers() {
        let root = Root::new();
        let ledger = root.ledger();
        let mut child = Child::new();
        let lease = child.lease(10_000);
        let server = LeaseGuardServer::bind(ledger.clone(), root.0.join("rpc"))
            .await
            .unwrap();
        let client = LeaseGuardClient::bind(root.socket(), server.identity().clone());
        let shutdown = CancellationToken::new();
        let active = Arc::new(AtomicUsize::new(0));
        let enforcement = Enforcer::start(ledger.clone(), active.clone(), shutdown.clone());
        let task = tokio::spawn(server.run_owned(shutdown.clone(), active.clone(), enforcement));
        client.adopt(lease.clone()).await.unwrap();
        let (ready_tx, ready_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let held = ledger.clone();
        let holder = std::thread::spawn(move || held.hold_mutations_for_test(ready_tx, release_rx));
        ready_rx.recv_timeout(Duration::from_secs(1)).unwrap();
        let pending = tokio::spawn(async move {
            client
                .renew(
                    1,
                    ExecutionLease {
                        revision: 2,
                        expires_unix_ms: lease.expires_unix_ms + 1000,
                        ..lease
                    },
                )
                .await
        });
        let deadline = std::time::Instant::now() + Duration::from_secs(1);
        while active.load(Ordering::SeqCst) == 0 {
            assert!(std::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        shutdown.cancel();
        child.exited().await;
        assert!(
            !task.is_finished(),
            "shutdown discarded an accepted ledger mutation"
        );
        release_tx.send(()).unwrap();
        holder.join().unwrap();
        assert_eq!(pending.await.unwrap(), Err(Error::Unavailable));
        task.await.unwrap().unwrap();
        let state: serde_json::Value =
            serde_json::from_slice(&fs::read(root.0.join("state/leases.json")).unwrap()).unwrap();
        assert_eq!(state["leases"]["owned_box"]["revision"], 1);
    }
}
