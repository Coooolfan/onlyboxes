//! Real companion RPC and owned kernel writer; the execution backend is a fake
//! so refusal tests prove no guest command, resource read or root creation ran.
use super::*;
use worker_boxlite::persistence::{
    ExecutionLease, ExecutionLeaseLedger, GuardedBoot, HostProcess, LeaseGuardClient,
    LeaseGuardServer,
};

struct Rig {
    f: Fixture,
    client: LeaseGuardClient,
    child: std::process::Child,
    control: PathBuf,
    shutdown: CancellationToken,
    server: Option<tokio::task::JoinHandle<Result<(), worker_boxlite::persistence::Error>>>,
}
impl Rig {
    async fn new() -> Self {
        let f = Fixture::new(1).await;
        let control =
            PathBuf::from("/tmp").join(format!("obx-mg-{}", &Uuid::new_v4().to_string()[..8]));
        let ledger =
            Arc::new(ExecutionLeaseLedger::open(f.root.join("leases"), "worker-a").unwrap());
        let server = LeaseGuardServer::bind(ledger, &control).await.unwrap();
        let client =
            LeaseGuardClient::bind(control.join("control.sock"), server.identity().clone());
        let shutdown = CancellationToken::new();
        let server = tokio::spawn(server.run(shutdown.clone()));
        let child = std::process::Command::new("/bin/sleep")
            .arg("30")
            .spawn()
            .unwrap();
        let writer = HostProcess::inspect_writer(child.id(), "restored-box").unwrap();
        let lease = client
            .adopt(ExecutionLease {
                box_id: "restored-box".into(),
                scoped_session_id: SCOPE.into(),
                generation: 1,
                revision: 1,
                expires_unix_ms: future(5),
                process: writer.clone(),
                revoked: false,
            })
            .await
            .unwrap();
        f.manager
            .install_execution_guard(Some(client.clone()))
            .await
            .unwrap();
        let adoption = f
            .journal
            .begin_initial_adoption(worker_boxlite::persistence::InitialAdoption {
                guard: client.identity().clone(),
                lease: lease.clone(),
                confirmed: false,
            })
            .unwrap();
        f.journal.confirm_initial_adoption(&adoption).unwrap();
        f.manager
            .install_snapshot_journal(f.journal.clone())
            .await
            .unwrap();
        connect(&f.manager, CONNECTION, &[]).await;
        f.manager
            .recover_one_session(
                SCOPE,
                system_time_from_unix_millis(lease.expires_unix_ms).unwrap(),
                1,
            )
            .await
            .unwrap();
        f.journal.claim(serde_json::from_value(serde_json::json!({
            "operation_id":OP,"scoped_session_id":SCOPE,"generation":1,
            "kind":"source","phase":"preparing","box_id":"restored-box","restore_spec":spec(),
        })).unwrap()).unwrap();
        f.journal
            .update(OP, |op| {
                op.phase = Phase::RollingBack;
                op.start_attempted = true;
                op.started_process = Some(writer);
                op.lease_expires_unix_ms = lease.expires_unix_ms;
                op.boot_guard = Some(GuardedBoot {
                    guard: client.identity().clone(),
                    authorization: None,
                });
                Ok(())
            })
            .unwrap();
        f.journal
            .update(OP, |op| {
                op.phase = Phase::RolledBack;
                Ok(())
            })
            .unwrap();
        f.manager.sync_snapshot_operation(OP).await.unwrap();
        f.manager.bind_snapshot_instance(OP).await.unwrap();
        f.manager
            .activate_snapshot_instance(CONNECTION, SCOPE, OP, 1)
            .await
            .unwrap();
        Self {
            f,
            client,
            child,
            control,
            shutdown,
            server: Some(server),
        }
    }
    async fn lose_service(&mut self) {
        let server = self.server.take().unwrap();
        server.abort();
        let _ = server.await;
    }
    async fn close(mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        self.shutdown.cancel();
        if let Some(server) = self.server.take() {
            server.await.unwrap().unwrap();
        }
        self.f.manager.close_preserving().await;
    }
}
impl Drop for Rig {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        self.shutdown.cancel();
        if let Some(server) = &self.server {
            server.abort();
        }
        let _ = std::fs::remove_dir_all(&self.control);
    }
}

#[tokio::test]
async fn execute_and_explicit_renew_acknowledge_guard_before_local_extension() {
    let r = Rig::new().await;
    let initial = r.client.check_running("restored-box").await.unwrap();
    let result =
        r.f.manager
            .execute(request("guarded-exec", 1))
            .await
            .unwrap();
    let guarded = r.client.check_running("restored-box").await.unwrap();
    let saved = r.f.journal.snapshot().unwrap().operations[OP].lease_expires_unix_ms;
    assert!(guarded.revision > initial.revision);
    assert!(
        guarded.expires_unix_ms >= saved && guarded.expires_unix_ms >= result.lease_expires_unix_ms
    );
    assert_eq!(r.f.backend.observed_leases.lock().await.as_slice(), [saved]);
    let renewed =
        r.f.manager
            .renew_lease(TerminalLeaseRenewRequest {
                session_id: SCOPE.into(),
                generation: 1,
                connection_id: Some(CONNECTION.into()),
                lease_ttl_sec: 400,
            })
            .await
            .unwrap();
    let guarded = r.client.check_running("restored-box").await.unwrap();
    assert!(guarded.expires_unix_ms >= renewed.lease_expires_unix_ms);
    let memory =
        r.f.manager
            .sessions
            .lock()
            .await
            .get(SCOPE)
            .unwrap()
            .lease_expires_at;
    assert!(system_time_from_unix_millis(guarded.expires_unix_ms).unwrap() >= memory);
    let short =
        r.f.manager
            .renew_lease(TerminalLeaseRenewRequest {
                session_id: SCOPE.into(),
                generation: 1,
                connection_id: Some(CONNECTION.into()),
                lease_ttl_sec: 1,
            })
            .await
            .unwrap();
    assert_eq!(short.lease_expires_unix_ms, renewed.lease_expires_unix_ms);
    assert_eq!(
        r.client.check_running("restored-box").await.unwrap(),
        guarded
    );
    let permit =
        r.f.manager
            .claim_proxy(SCOPE, 8080, 1, SystemTime::now())
            .await
            .unwrap();
    assert!(
        r.f.manager
            .proxy_binding_active(SCOPE, "restored-box", 1, SystemTime::now())
            .await
    );
    drop(permit);
    r.close().await;
}

#[tokio::test]
async fn loss_of_guard_fences_every_request_without_extending_memory_or_recreating_root() {
    for action in [
        "execute",
        "shorter-renew",
        "resource",
        "proxy",
        "proxy-active",
        "recover",
        "activate",
    ] {
        let mut r = Rig::new().await;
        let permit =
            r.f.manager
                .claim_proxy(SCOPE, 8080, 1, SystemTime::now())
                .await
                .unwrap();
        let before =
            r.f.manager
                .sessions
                .lock()
                .await
                .get(SCOPE)
                .unwrap()
                .lease_expires_at;
        r.lose_service().await;
        let refused = match action {
            "execute" => {
                r.f.manager
                    .execute(request("must-not-run", 1))
                    .await
                    .is_err()
            }
            "shorter-renew" => {
                r.f.manager
                    .renew_lease(TerminalLeaseRenewRequest {
                        session_id: SCOPE.into(),
                        generation: 1,
                        connection_id: Some(CONNECTION.into()),
                        lease_ttl_sec: 1,
                    })
                    .await
                    .is_err()
            }
            "resource" => {
                r.f.manager
                    .resolve_resource(TerminalResourceRequest {
                        session_id: SCOPE.into(),
                        generation: 1,
                        connection_id: Some(CONNECTION.into()),
                        file_path: "/data".into(),
                        action: "read".into(),
                        signed_url: String::new(),
                        headers: HashMap::new(),
                        deadline_unix_ms: 0,
                    })
                    .await
                    .is_err()
            }
            "proxy" => {
                r.f.manager
                    .claim_proxy(SCOPE, 8080, 1, SystemTime::now())
                    .await
                    .is_err()
            }
            "proxy-active" => {
                !r.f.manager
                    .proxy_binding_active(SCOPE, "restored-box", 1, SystemTime::now())
                    .await
            }
            "recover" => {
                r.f.manager
                    .recover_one_session(SCOPE, SystemTime::now() + Duration::from_secs(300), 1)
                    .await
                    .is_err()
            }
            "activate" => {
                r.f.manager
                    .activate_snapshot_instance(CONNECTION, SCOPE, OP, 1)
                    .await
                    .is_err()
            }
            _ => unreachable!(),
        };
        assert!(refused, "{action}");
        let state = r.f.manager.sessions.lock().await;
        assert!(state.failed, "{action}");
        assert_eq!(
            state.get(SCOPE).unwrap().lease_expires_at,
            before,
            "{action}"
        );
        assert!(permit.cancel.is_cancelled(), "{action}");
        drop(state);
        drop(permit);
        assert!(r.f.backend.commands.lock().await.is_empty(), "{action}");
        assert!(r.f.backend.removed.lock().await.is_empty(), "{action}");
        assert_eq!(r.f.backend.created.load(Ordering::SeqCst), 0, "{action}");
        r.close().await;
    }
}

#[tokio::test]
async fn guarded_journal_cannot_reopen_without_the_same_companion() {
    let r = Rig::new().await;
    let m = manager(r.f.backend.clone(), 1);
    assert!(m
        .install_snapshot_journal(r.f.journal.clone())
        .await
        .is_err());
    assert!(m.sessions.lock().await.failed);
    assert!(r.f.manager.install_execution_guard(None).await.is_err());
    assert!(r.f.manager.sessions.lock().await.failed);
    m.close_preserving().await;
    r.close().await;
}

#[tokio::test]
async fn guarded_active_root_survives_local_expiry_and_pending_adoption_blocks_execution() {
    let r = Rig::new().await;
    {
        let mut state = r.f.manager.sessions.lock().await;
        state.get_mut(SCOPE).unwrap().lease_expires_at = SystemTime::UNIX_EPOCH;
        assert!(state.preserve(SCOPE));
    }
    r.f.manager.cleanup_expired_sessions().await;
    assert!(r.f.backend.removed.lock().await.is_empty());
    assert_eq!(r.f.manager.active_session_count().await, 1);
    {
        let mut state = r.f.manager.sessions.lock().await;
        state.get_mut(SCOPE).unwrap().lease_expires_at = SystemTime::now() + Duration::from_secs(5);
        state.pending_adoptions.insert(SCOPE.into());
    }
    for generation in [0, 1] {
        assert!(r
            .f
            .manager
            .execute(request("pending-adoption", generation))
            .await
            .is_err());
        assert!(r
            .f
            .manager
            .claim_proxy(SCOPE, 8080, generation, SystemTime::now())
            .await
            .is_err());
    }
    assert!(r.f.backend.commands.lock().await.is_empty());
    assert_eq!(r.f.backend.created.load(Ordering::SeqCst), 0);
    assert!(r.f.backend.removed.lock().await.is_empty());
    r.close().await;
}

#[tokio::test]
async fn guarded_creation_failure_retains_capacity_and_never_uses_legacy_creation() {
    let r = Rig::new().await;
    let m = manager(r.f.backend.clone(), 2);
    m.install_execution_guard(Some(r.client.clone()))
        .await
        .unwrap();
    m.install_snapshot_journal(r.f.journal.clone())
        .await
        .unwrap();
    connect(&m, CONNECTION, &[]).await;
    let scope = "obx:owner:guarded-create";
    let mut req = request("must-not-run", 0);
    req.session_id = scope.into();
    assert!(m.execute(req.clone()).await.is_err());
    assert!(m.sessions.lock().await.pending_adoptions.contains(scope));
    assert_eq!(m.active_session_count().await, 2);
    assert!(m.execute(req).await.is_err());
    m.cleanup_expired_sessions().await;
    assert_eq!(m.active_session_count().await, 2);
    assert_eq!(r.f.backend.created.load(Ordering::SeqCst), 0);
    assert!(r.f.backend.commands.lock().await.is_empty());
    assert!(r.f.backend.removed.lock().await.is_empty());
    m.close_preserving().await;
    r.close().await;
}

#[tokio::test]
async fn revoked_stopped_initial_root_is_held_without_transferring_execution() {
    let mut r = Rig::new().await;
    let f = Fixture::new(2).await;
    let lease = r.client.status("restored-box").await.unwrap().unwrap();
    // Durable metadata fixture: manager startup never verifies/copies a shim
    // or calls native boot. The current companion RPC and writer are real.
    let mut origin = lease.process.clone();
    origin.writer_cgroup = None;
    origin.signal_version = None;
    let mut old_guard = r.client.identity().clone();
    old_guard.epoch_id = Uuid::new_v4().to_string();
    old_guard.process = origin.clone();
    let executable = serde_json::from_value(serde_json::json!({
        "home": f.root, "box_id":"restored-box",
        "directories":vec![serde_json::json!({"device":1,"inode":1});4],
        "image":{"device":1,"inode":1,"size":1,
            "modified":0,"modified_ns":0,"changed":0,"changed_ns":0},
        "sha256":"a".repeat(64)
    }))
    .unwrap();
    f.journal
        .begin_initial_boot(worker_boxlite::persistence::InitialBoot {
            guard: old_guard.clone(),
            authorization: worker_boxlite::persistence::BootAuthorization {
                operation_id: "initial-test".into(),
                box_id: lease.box_id.clone(),
                scoped_session_id: SCOPE.into(),
                generation: 1,
                prior_revision: None,
                expires_unix_ms: lease.expires_unix_ms,
                origin,
                executable,
            },
            managed: false,
        })
        .unwrap();
    let adoption = f
        .journal
        .begin_initial_adoption(worker_boxlite::persistence::InitialAdoption {
            guard: old_guard,
            lease,
            confirmed: false,
        })
        .unwrap();
    f.journal.confirm_initial_adoption(&adoption).unwrap();
    let saved = f.journal.snapshot().unwrap();
    assert!(!stopped_initial_hold(&saved, SCOPE, "restored-box", &r.client).await);
    r.child.kill().unwrap();
    r.child.wait().unwrap();
    // A dead writer alone is insufficient: wait for the real service to
    // acknowledge revocation of its original short grant.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(8);
    while !r
        .client
        .status("restored-box")
        .await
        .unwrap()
        .unwrap()
        .revoked
    {
        assert!(tokio::time::Instant::now() < deadline);
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let complete_writer_proof = adoption.lease.process.writers_exited().unwrap_or(false);
    assert_eq!(
        stopped_initial_hold(&saved, SCOPE, "restored-box", &r.client).await,
        complete_writer_proof
    );
    let mut same_service = saved.clone();
    same_service.initial_boots.get_mut(SCOPE).unwrap().guard = r.client.identity().clone();
    same_service.adoptions.get_mut(SCOPE).unwrap().guard = r.client.identity().clone();
    assert_eq!(
        stopped_initial_hold(&same_service, SCOPE, "restored-box", &r.client).await,
        complete_writer_proof
    );
    let mut foreign_store = r.client.identity().clone();
    foreign_store.store_id = Uuid::new_v4().to_string();
    let foreign = LeaseGuardClient::bind(r.control.join("control.sock"), foreign_store);
    assert!(!stopped_initial_hold(&saved, SCOPE, "restored-box", &foreign).await);
    for case in [
        "managed",
        "unconfirmed",
        "missing-boot",
        "live-origin",
        "live-old-service",
        "wrong-writer",
        "lifecycle",
    ] {
        let mut invalid = saved.clone();
        match case {
            "managed" => invalid.initial_boots.get_mut(SCOPE).unwrap().managed = true,
            "unconfirmed" => invalid.adoptions.get_mut(SCOPE).unwrap().confirmed = false,
            "missing-boot" => {
                invalid.initial_boots.clear();
            }
            "live-origin" => {
                invalid
                    .initial_boots
                    .get_mut(SCOPE)
                    .unwrap()
                    .authorization
                    .origin = HostProcess::current().unwrap()
            }
            "live-old-service" => {
                let process = HostProcess::current().unwrap();
                invalid.adoptions.get_mut(SCOPE).unwrap().guard.process = process.clone();
                invalid.initial_boots.get_mut(SCOPE).unwrap().guard.process = process;
            }
            "wrong-writer" => {
                invalid.adoptions.get_mut(SCOPE).unwrap().lease.process =
                    HostProcess::current().unwrap()
            }
            "lifecycle" => {
                invalid.current.insert(SCOPE.into(), OP.into());
            }
            _ => unreachable!(),
        }
        assert!(
            !stopped_initial_hold(&invalid, SCOPE, "restored-box", &r.client).await,
            "{case}"
        );
    }
    f.manager
        .install_execution_guard(Some(r.client.clone()))
        .await
        .unwrap();
    let result = f.manager.install_snapshot_journal(f.journal.clone()).await;
    if complete_writer_proof {
        result.unwrap();
        connect(&f.manager, CONNECTION, &[]).await;
        let state = f.manager.sessions.lock().await;
        assert!(!state.failed);
        assert!(state.pending_adoptions.contains(SCOPE));
        assert_eq!(
            state.initial_roots.get(SCOPE).map(String::as_str),
            Some("restored-box")
        );
        assert!(state.preserve(SCOPE));
        drop(state);
        assert_eq!(f.manager.active_session_count().await, 1);
        for generation in [0, 1] {
            assert!(f
                .manager
                .execute(request("held-initial", generation))
                .await
                .is_err());
            assert!(f
                .manager
                .claim_proxy(SCOPE, 8080, generation, SystemTime::now())
                .await
                .is_err());
        }
        assert!(f
            .manager
            .renew_lease(TerminalLeaseRenewRequest {
                session_id: SCOPE.into(),
                generation: 0,
                connection_id: Some(CONNECTION.into()),
                lease_ttl_sec: 60,
            })
            .await
            .is_err());
        assert!(f
            .manager
            .resolve_resource(TerminalResourceRequest {
                session_id: SCOPE.into(),
                generation: 0,
                connection_id: Some(CONNECTION.into()),
                file_path: "/data".into(),
                action: "read".into(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .is_err());
        assert!(f
            .manager
            .recover_one_session(SCOPE, SystemTime::now() + Duration::from_secs(60), 0)
            .await
            .is_err());
        f.manager.cleanup_expired_sessions().await;
        assert_eq!(f.manager.active_session_count().await, 1);
        assert!(f.backend.commands.lock().await.is_empty());
        assert!(f.backend.removed.lock().await.is_empty());
        assert!(f.backend.pinned.lock().await.is_empty());
        assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
        let after = f.journal.snapshot().unwrap();
        assert_eq!(after.initial_boots, saved.initial_boots);
        assert_eq!(after.adoptions, saved.adoptions);
        assert_eq!(after.probes, saved.probes);
    } else {
        assert!(result.is_err());
        assert!(f.manager.sessions.lock().await.failed);
    }
    r.lose_service().await;
    assert!(!stopped_initial_hold(&saved, SCOPE, "restored-box", &r.client).await);
    f.manager.close_preserving().await;
    r.close().await;
}

#[tokio::test]
async fn orderly_shutdown_gate_requires_healthy_journal_and_exact_guard_identity() {
    let r = Rig::new().await;
    assert!(
        r.f.manager
            .healthy_guarded_shutdown(r.client.identity())
            .await
    );
    let mut changed = r.client.identity().clone();
    changed.epoch_id = Uuid::new_v4().to_string();
    assert!(!r.f.manager.healthy_guarded_shutdown(&changed).await);
    r.f.manager.close_preserving().await;
    assert!(
        r.f.manager
            .healthy_guarded_shutdown(r.client.identity())
            .await
    );
    assert!(r.client.check_running("restored-box").await.is_ok());
    r.f.manager.fail_persistence().await;
    assert!(
        !r.f.manager
            .healthy_guarded_shutdown(r.client.identity())
            .await
    );
    r.f.journal.close().unwrap();
    assert!(
        !r.f.manager
            .healthy_guarded_shutdown(r.client.identity())
            .await
    );
    r.close().await;
}

#[tokio::test]
async fn stopped_managed_boot_is_retained_without_recovery_or_execution() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let mut r = Rig::new().await;
    let f = Fixture::new(2).await;
    let lease = r.client.status("restored-box").await.unwrap().unwrap();
    let mut origin = lease.process.clone();
    origin.writer_cgroup = None;
    origin.signal_version = None;
    // A metadata-only boot transport fixture. It cannot start a native VM;
    // real SDK boot receipts are covered by the retained-home acceptance.
    let executable = serde_json::from_value(serde_json::json!({
        "home":f.root, "box_id":"restored-box",
        "directories":vec![serde_json::json!({"device":1,"inode":1});4],
        "image":{"device":1,"inode":1,"size":1,"modified":0,"modified_ns":0,"changed":0,"changed_ns":0},
        "sha256":"a".repeat(64)
    })).unwrap();
    let authorization = worker_boxlite::persistence::BootAuthorization {
        operation_id: OP.into(),
        box_id: "restored-box".into(),
        scoped_session_id: SCOPE.into(),
        generation: 1,
        prior_revision: None,
        expires_unix_ms: lease.expires_unix_ms,
        origin,
        executable,
    };
    let identity = r.client.identity().clone();
    let mode = Arc::new(AtomicU32::new(0));
    let endpoint = r.control.join("held.sock");
    let listener = tokio::net::UnixListener::bind(&endpoint).unwrap();
    std::fs::set_permissions(&endpoint, std::fs::Permissions::from_mode(0o600)).unwrap();
    let client = LeaseGuardClient::bind(endpoint, identity.clone());
    let shutdown = CancellationToken::new();
    let stopping = shutdown.clone();
    let mock_mode = mode.clone();
    let auth = authorization.clone();
    let stored = lease.clone();
    let service = tokio::spawn(async move {
        loop {
            let (mut stream, _) = tokio::select! {
                _ = stopping.cancelled() => break,
                accepted = listener.accept() => accepted.unwrap(),
            };
            let size = stream.read_u32().await.unwrap();
            assert!(size < 16384);
            let mut bytes = vec![0; size as usize];
            stream.read_exact(&mut bytes).await.unwrap();
            let request: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            assert_eq!(
                request["expected"],
                serde_json::to_value(&identity).unwrap()
            );
            let mode = mock_mode.load(Ordering::SeqCst);
            let reply = match request["command"]["action"].as_str().unwrap() {
                "check_running" => serde_json::json!({"type":"rejected","value":"unavailable"}),
                "boot_status" => {
                    serde_json::json!({"type":"boot_status","value":if mode == 1 {None} else {Some(auth.clone())}})
                }
                "status" => {
                    let mut lease = stored.clone();
                    if mode == 2 {
                        lease.expires_unix_ms -= 1;
                    }
                    if mode == 3 {
                        lease.process = HostProcess::current().unwrap();
                    }
                    if mode == 4 {
                        lease.generation += 1;
                    }
                    if mode == 5 {
                        lease.revoked = true;
                    }
                    serde_json::json!({"type":"status","value":if mode == 6 {None} else {Some(lease)}})
                }
                _ => panic!("retention must not send a mutating companion request"),
            };
            let response = serde_json::to_vec(&serde_json::json!({"version":2,"request_id":request["request_id"],"identity":identity,"reply":reply})).unwrap();
            stream.write_u32(response.len() as u32).await.unwrap();
            stream.write_all(&response).await.unwrap();
        }
    });
    f.journal
        .claim(
            serde_json::from_value(serde_json::json!({
                "operation_id":OP,"scoped_session_id":SCOPE,"generation":1,
                "kind":"source","phase":"preparing","box_id":"restored-box","restore_spec":spec()
            }))
            .unwrap(),
        )
        .unwrap();
    f.journal
        .update(OP, |op| {
            op.phase = Phase::RollingBack;
            op.start_attempted = true;
            op.started_process = Some(lease.process.clone());
            op.lease_expires_unix_ms = lease.expires_unix_ms;
            op.boot_executable = Some(authorization.executable.clone());
            op.boot_guard = Some(GuardedBoot {
                guard: client.identity().clone(),
                authorization: Some(authorization.clone()),
            });
            Ok(())
        })
        .unwrap();
    f.journal
        .update(OP, |op| {
            op.phase = Phase::RolledBack;
            Ok(())
        })
        .unwrap();
    let saved = f.journal.snapshot().unwrap();
    let op = &saved.operations[OP];
    assert!(!stopped_managed_hold(op, "worker-a", &client).await);
    r.child.kill().unwrap();
    r.child.wait().unwrap();
    let proof = lease.process.writers_exited().unwrap_or(false);
    for value in 0..=6 {
        mode.store(value, Ordering::SeqCst);
        assert_eq!(
            stopped_managed_hold(op, "worker-a", &client).await,
            proof && matches!(value, 0 | 5),
            "mode={value}"
        );
    }
    mode.store(0, Ordering::SeqCst);
    for missing in [
        "attempt",
        "writer",
        "authorization",
        "executable",
        "live-origin",
        "epoch",
        "phase",
    ] {
        let mut invalid = op.clone();
        match missing {
            "attempt" => invalid.start_attempted = false,
            "writer" => invalid.started_process = None,
            "authorization" => invalid.boot_guard.as_mut().unwrap().authorization = None,
            "executable" => invalid.boot_executable = None,
            "live-origin" => {
                invalid
                    .boot_guard
                    .as_mut()
                    .unwrap()
                    .authorization
                    .as_mut()
                    .unwrap()
                    .origin = HostProcess::current().unwrap()
            }
            "epoch" => {
                invalid.boot_guard.as_mut().unwrap().guard.epoch_id = Uuid::new_v4().to_string()
            }
            "phase" => invalid.phase = Phase::RollingBack,
            _ => unreachable!(),
        }
        assert!(
            !stopped_managed_hold(&invalid, "worker-a", &client).await,
            "{missing}"
        );
    }
    f.manager
        .install_execution_guard(Some(client.clone()))
        .await
        .unwrap();
    let result = f.manager.install_snapshot_journal(f.journal.clone()).await;
    if proof {
        result.unwrap();
        connect(&f.manager, CONNECTION, &[]).await;
        let state = f.manager.sessions.lock().await;
        assert!(!state.failed);
        assert!(state.pending_adoptions.contains(SCOPE));
        assert!(!state.fences[SCOPE].active);
        assert!(state.fences[SCOPE].held);
        assert!(state
            .protected_names
            .contains(&terminal_session_resource_name(SCOPE)));
        drop(state);
        assert_eq!(f.manager.active_session_count().await, 1);
        for generation in [0, 1] {
            assert!(f
                .manager
                .execute(request("held-managed", generation))
                .await
                .is_err());
            assert!(f
                .manager
                .claim_proxy(SCOPE, 8080, generation, SystemTime::now())
                .await
                .is_err());
        }
        assert!(f
            .manager
            .renew_lease(TerminalLeaseRenewRequest {
                session_id: SCOPE.into(),
                generation: 1,
                connection_id: Some(CONNECTION.into()),
                lease_ttl_sec: 60,
            })
            .await
            .is_err());
        assert!(f
            .manager
            .resolve_resource(TerminalResourceRequest {
                session_id: SCOPE.into(),
                generation: 1,
                connection_id: Some(CONNECTION.into()),
                file_path: "/data".into(),
                action: "read".into(),
                signed_url: String::new(),
                headers: HashMap::new(),
                deadline_unix_ms: 0,
            })
            .await
            .is_err());
        assert!(f
            .manager
            .recover_one_session(SCOPE, SystemTime::now() + Duration::from_secs(60), 1)
            .await
            .is_err());
        f.manager.cleanup_expired_sessions().await;
        assert_eq!(f.manager.active_session_count().await, 1);
        assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
        assert!(f.backend.commands.lock().await.is_empty());
        assert!(f.backend.pinned.lock().await.is_empty());
        assert!(f.backend.removed.lock().await.is_empty());
        let after = f.journal.snapshot().unwrap();
        assert_eq!(
            serde_json::to_value(&after).unwrap(),
            serde_json::to_value(&saved).unwrap()
        );
    } else {
        assert!(result.is_err());
        assert!(f.manager.sessions.lock().await.failed);
    }
    shutdown.cancel();
    service.await.unwrap();
    assert!(!stopped_managed_hold(op, "worker-a", &client).await);
    f.manager.close_preserving().await;
    r.close().await;
}
