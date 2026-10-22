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
        f.journal.probe(SCOPE, "restored-box").unwrap();
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
