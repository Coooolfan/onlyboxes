use super::*;
use std::collections::BTreeMap;
use std::os::unix::fs::PermissionsExt;
use worker_boxlite::persistence::{
    Archive, Operation, RestoreSpec, ARCHIVE_FORMAT, RUNTIME_VERSION,
};

const SCOPE: &str = "obx:owner:session";
const OP: &str = "restore-operation";
const CONNECTION: &str = "connection-1";

#[derive(Default)]
struct Backend {
    created: AtomicU32,
    commands: Mutex<Vec<String>>,
    removed: Mutex<Vec<String>>,
    named: Mutex<Vec<String>>,
    pinned: Mutex<Vec<String>>,
    orphan_names: Mutex<Vec<HashSet<String>>>,
    started: Notify,
    release: Notify,
    missing: AtomicBool,
    alias: AtomicBool,
    journal: Option<Arc<Journal>>,
    observed_leases: Mutex<Vec<i64>>,
    cleanup_block: AtomicBool,
    cleanup_started: Notify,
    cleanup_release: Notify,
}
#[async_trait]
impl TerminalBackend for Backend {
    async fn create_session_box(
        &self,
        _shutdown: CancellationToken,
        _deadline: i64,
    ) -> Result<String, TerminalOperationError> {
        Ok(format!(
            "box-{}",
            self.created.fetch_add(1, Ordering::SeqCst) + 1
        ))
    }
    async fn recover_session_box(
        &self,
        scope: &str,
    ) -> Result<Option<String>, TerminalOperationError> {
        self.named.lock().await.push(scope.into());
        Ok(Some("legacy-box".into()))
    }
    async fn recover_pinned_session_box(
        &self,
        id: &str,
    ) -> Result<Option<String>, TerminalOperationError> {
        self.pinned.lock().await.push(id.into());
        if self.missing.load(Ordering::SeqCst) {
            Ok(None)
        } else if self.alias.load(Ordering::SeqCst) {
            Ok(Some("wrong-id".into()))
        } else {
            Ok(Some(id.into()))
        }
    }
    async fn cleanup_orphan_session_boxes(&self, names: &HashSet<String>) -> usize {
        self.orphan_names.lock().await.push(names.clone());
        0
    }
    async fn exec_shell_command(
        &self,
        id: &str,
        command: &str,
        _deadline: i64,
    ) -> Result<CollectedExecOutput, BoxliteCommandError> {
        self.commands.lock().await.push(format!("{id}:{command}"));
        if let Some(journal) = &self.journal {
            if let Some(op) = journal.snapshot().unwrap().operations.get(OP) {
                self.observed_leases
                    .lock()
                    .await
                    .push(op.lease_expires_unix_ms);
            }
        }
        if command == "block" || command == "deadline" {
            self.started.notify_one();
            self.release.notified().await;
        }
        if command == "deadline" {
            return Err(BoxliteCommandError::DeadlineExceeded);
        }
        Ok(CollectedExecOutput {
            stdout: id.into(),
            stderr: String::new(),
            exit_code: 0,
        })
    }
    async fn exec_resource_probe(
        &self,
        _id: &str,
        _action: &str,
        _path: &str,
        _max: usize,
        _deadline: i64,
    ) -> Result<CollectedExecOutput, BoxliteCommandError> {
        panic!("a gated resource request reached the backend")
    }
    async fn copy_out_file(
        &self,
        _id: &str,
        _source: &str,
        _target: &Path,
        _deadline: i64,
    ) -> Result<(), BoxliteCommandError> {
        panic!("unexpected copy")
    }
    async fn remove_box(&self, id: &str) {
        if self.cleanup_block.load(Ordering::SeqCst) {
            self.cleanup_started.notify_one();
            self.cleanup_release.notified().await;
        }
        self.removed.lock().await.push(id.into());
    }
    fn proxy_ports(&self) -> Vec<u16> {
        vec![8080]
    }
}
struct Fixture {
    root: PathBuf,
    journal: Arc<Journal>,
    backend: Arc<Backend>,
    manager: Arc<TerminalSessionManager>,
}
impl Fixture {
    async fn new(capacity: u32) -> Self {
        let root =
            std::env::temp_dir().join(format!("onlyboxes-boxlite-admission-{}", Uuid::new_v4()));
        std::fs::create_dir(&root).unwrap();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        let journal = Arc::new(Journal::open(root.join("journal"), "worker-a").unwrap());
        let backend = Arc::new(Backend {
            journal: Some(journal.clone()),
            ..Default::default()
        });
        let manager = manager(backend.clone(), capacity);
        manager
            .install_snapshot_journal(journal.clone())
            .await
            .unwrap();
        connect(&manager, CONNECTION, &[]).await;
        Self {
            root,
            journal,
            backend,
            manager,
        }
    }
    async fn ready(&self) {
        self.stage_ready().await;
        self.manager.bind_snapshot_instance(OP).await.unwrap();
    }
    async fn stage_ready(&self) {
        self.manager
            .reserve_snapshot_target(SCOPE, OP, 2)
            .await
            .unwrap();
        self.journal.claim(target()).unwrap();
        self.journal
            .update(OP, |o| {
                o.phase = Phase::Importing;
                o.verified = true;
                Ok(())
            })
            .unwrap();
        self.journal
            .update(OP, |o| {
                o.box_id = "restored-box".into();
                o.lease_expires_unix_ms = future(10);
                o.phase = Phase::Ready;
                Ok(())
            })
            .unwrap();
        self.manager.sync_snapshot_operation(OP).await.unwrap();
    }
    async fn active(&self) {
        self.ready().await;
        self.journal
            .update(OP, |o| {
                o.phase = Phase::Active;
                Ok(())
            })
            .unwrap();
        self.manager.sync_snapshot_operation(OP).await.unwrap();
        self.manager
            .activate_snapshot_instance(CONNECTION, SCOPE, OP, 2)
            .await
            .unwrap();
    }
}

#[tokio::test]
async fn ready_binding_refreshes_exact_id_and_rejects_missing_or_alias() {
    for outcome in ["running", "missing", "alias"] {
        let f = Fixture::new(1).await;
        f.stage_ready().await;
        f.backend
            .missing
            .store(outcome == "missing", Ordering::SeqCst);
        f.backend.alias.store(outcome == "alias", Ordering::SeqCst);
        assert_eq!(
            f.manager.bind_snapshot_instance(OP).await.is_ok(),
            outcome == "running"
        );
        assert_eq!(*f.backend.pinned.lock().await, vec!["restored-box"]);
        assert!(f.backend.named.lock().await.is_empty());
        assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
        assert_eq!(f.manager.active_session_count().await, 1);
        assert!(f.manager.execute(request("closed", 2)).await.is_err());
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = self.journal.close();
        let _ = std::fs::remove_dir_all(&self.root);
    }
}
fn manager(backend: Arc<Backend>, capacity: u32) -> Arc<TerminalSessionManager> {
    TerminalSessionManager::new(
        TerminalSessionManagerConfig {
            lease_min_sec: 1,
            lease_max_sec: 600,
            lease_default_sec: 60,
            output_limit_bytes: 1024,
            export_max_bytes: 0,
            session_max_inflight: 4,
            max_active_sessions: capacity,
        },
        backend,
    )
}
fn future(seconds: u64) -> i64 {
    to_unix_millis(SystemTime::now() + Duration::from_secs(seconds))
}
fn spec() -> RestoreSpec {
    RestoreSpec {
        runtime: "boxlite".into(),
        runtime_version: RUNTIME_VERSION.into(),
        platform: worker_boxlite::persistence::guest_platform()
            .unwrap()
            .into(),
        format: ARCHIVE_FORMAT.into(),
        image: format!("image@sha256:{}", "a".repeat(64)),
        memory_mib: 256,
        cpus: 1.0,
        max_processes: 128,
        user: String::new(),
        working_dir: String::new(),
        env: BTreeMap::new(),
        entrypoint: vec![],
        command: vec![],
    }
}
fn target() -> Operation {
    Operation {
        operation_id: OP.into(),
        scoped_session_id: SCOPE.into(),
        generation: 2,
        kind: Kind::Target,
        phase: Phase::Prepared,
        restore_spec: Some(spec()),
        archive: Some(Archive {
            snapshot_id: "source-snapshot".into(),
            size_bytes: 1,
            sha256: "b".repeat(64),
            expanded_bytes: 1024,
            format: ARCHIVE_FORMAT.into(),
        }),
        artifact: worker_boxlite::persistence::artifact_name(SCOPE, OP, 2),
        box_id: String::new(),
        tombstone_box_id: String::new(),
        source_process: None,
        source_stop_confirmed: false,
        start_attempted: false,
        started_process: None,
        lease_expires_unix_ms: 0,
        staging_bytes: 4096,
        snapshot_committed: false,
        verified: false,
        remote_verified: false,
        cache_until_unix_ms: 0,
        lease_ttl_sec: 300,
    }
}
fn request(command: &str, generation: i64) -> TerminalExecRequest {
    TerminalExecRequest {
        session_id: SCOPE.into(),
        generation,
        connection_id: Some(CONNECTION.into()),
        command: command.into(),
        create_if_missing: true,
        lease_ttl_sec: Some(300),
        deadline_unix_ms: 0,
    }
}
async fn connect(
    m: &TerminalSessionManager,
    connection: &str,
    hints: &[SandboxPersistenceRecoveryCandidate],
) {
    m.protect_persistence_recovery(connection, &[], hints)
        .await
        .unwrap();
    m.finish_persistence_recovery(connection).await.unwrap();
}
fn code(error: TerminalOperationError) -> String {
    match error {
        TerminalOperationError::Terminal(e) => e.code,
        other => panic!("unexpected error: {other}"),
    }
}
fn hint(operation: &str, generation: i64) -> SandboxPersistenceRecoveryCandidate {
    SandboxPersistenceRecoveryCandidate {
        scoped_session_id: SCOPE.into(),
        operation_id: operation.into(),
        generation,
        role: sandbox_persistence_recovery_candidate::Role::Target as i32,
        snapshot_committed: false,
        route_committed: false,
    }
}

#[tokio::test]
async fn source_hold_closes_all_admission_and_drains_accepted_work_and_proxies() {
    let f = Fixture::new(1).await;
    f.manager.execute(request("seed", 0)).await.unwrap();
    let m = f.manager.clone();
    let task = tokio::spawn(async move { m.execute(request("block", 0)).await });
    f.backend.started.notified().await;
    let permit = f
        .manager
        .claim_proxy(SCOPE, 8080, 0, SystemTime::now())
        .await
        .unwrap();
    assert_eq!(
        f.manager
            .hold_snapshot_source(SCOPE, "suspend-operation", 1)
            .await
            .unwrap(),
        "box-1"
    );
    assert!(permit.cancel.is_cancelled());
    for generation in [0, 1] {
        assert_eq!(
            code(
                f.manager
                    .execute(request("late", generation))
                    .await
                    .unwrap_err()
            ),
            "session_state_conflict"
        );
    }
    assert_eq!(
        code(
            f.manager
                .renew_lease(TerminalLeaseRenewRequest {
                    session_id: SCOPE.into(),
                    generation: 1,
                    connection_id: Some(CONNECTION.into()),
                    lease_ttl_sec: 300
                })
                .await
                .unwrap_err()
        ),
        "session_state_conflict"
    );
    assert_eq!(
        code(
            f.manager
                .resolve_resource(TerminalResourceRequest {
                    session_id: SCOPE.into(),
                    generation: 1,
                    connection_id: Some(CONNECTION.into()),
                    file_path: "/data".into(),
                    action: "read".into(),
                    signed_url: String::new(),
                    headers: HashMap::new(),
                    deadline_unix_ms: 0
                })
                .await
                .unwrap_err()
        ),
        "session_state_conflict"
    );
    assert!(f
        .manager
        .claim_proxy(SCOPE, 8080, 1, SystemTime::now())
        .await
        .is_err());
    f.manager
        .sessions
        .lock()
        .await
        .get_mut(SCOPE)
        .unwrap()
        .lease_expires_at = SystemTime::UNIX_EPOCH;
    f.manager.cleanup_expired_sessions().await;
    assert!(f.backend.removed.lock().await.is_empty());
    assert_eq!(f.manager.active_session_count().await, 1);
    assert!(tokio::time::timeout(
        Duration::from_millis(10),
        f.manager
            .drain_snapshot_source(SCOPE, "suspend-operation", 1)
    )
    .await
    .is_err());
    f.backend.release.notify_one();
    task.await.unwrap().unwrap();
    assert!(tokio::time::timeout(
        Duration::from_millis(10),
        f.manager
            .drain_snapshot_source(SCOPE, "suspend-operation", 1)
    )
    .await
    .is_err());
    drop(permit);
    f.manager
        .drain_snapshot_source(SCOPE, "suspend-operation", 1)
        .await
        .unwrap();
    f.manager.close().await;
    assert!(f.backend.removed.lock().await.is_empty());
}

#[tokio::test]
async fn a_failed_accepted_command_cannot_destroy_the_held_source() {
    let f = Fixture::new(1).await;
    f.manager.execute(request("seed", 0)).await.unwrap();
    let m = f.manager.clone();
    let task = tokio::spawn(async move { m.execute(request("deadline", 0)).await });
    f.backend.started.notified().await;
    f.manager
        .hold_snapshot_source(SCOPE, "suspend-operation", 1)
        .await
        .unwrap();
    f.backend.release.notify_one();
    assert!(matches!(
        task.await.unwrap(),
        Err(TerminalOperationError::DeadlineExceeded)
    ));
    assert!(f.backend.removed.lock().await.is_empty());
    assert!(matches!(
        f.manager
            .drain_snapshot_source(SCOPE, "suspend-operation", 1)
            .await,
        Err(TerminalOperationError::ExecutionFailed(_))
    ));
    assert_eq!(
        f.manager.sessions.lock().await.get(SCOPE).unwrap().box_id,
        "box-1"
    );
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn restored_binding_stays_inactive_until_durable_activation_and_counts_once() {
    let f = Fixture::new(1).await;
    f.ready().await;
    assert_eq!(f.manager.active_session_count().await, 1);
    assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
    assert_eq!(
        code(f.manager.execute(request("early", 2)).await.unwrap_err()),
        "session_state_conflict"
    );
    let mut other = request("new", 0);
    other.session_id = "obx:owner:other".into();
    assert_eq!(
        code(f.manager.execute(other).await.unwrap_err()),
        "session_capacity_exceeded"
    );
    assert!(f
        .manager
        .activate_snapshot_instance(CONNECTION, SCOPE, OP, 2)
        .await
        .is_err());
    f.journal
        .update(OP, |o| {
            o.phase = Phase::Active;
            Ok(())
        })
        .unwrap();
    f.manager.sync_snapshot_operation(OP).await.unwrap();
    assert!(f.manager.execute(request("still-closed", 2)).await.is_err());
    assert!(f
        .manager
        .activate_snapshot_instance("old-connection", SCOPE, OP, 2)
        .await
        .is_err());
    f.manager
        .activate_snapshot_instance(CONNECTION, SCOPE, OP, 2)
        .await
        .unwrap();
    for generation in [0, 1, 3] {
        assert!(f
            .manager
            .execute(request("stale", generation))
            .await
            .is_err());
    }
    let result = f.manager.execute(request("current", 2)).await.unwrap();
    assert_eq!(result.stdout, "restored-box");
    assert!(!result.created);
    let saved = f.journal.snapshot().unwrap().operations[OP].lease_expires_unix_ms;
    assert_eq!(f.backend.observed_leases.lock().await.as_slice(), [saved]);
    assert_eq!(result.lease_expires_unix_ms, saved);
    assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
    assert!(f
        .manager
        .claim_proxy(SCOPE, 8080, 0, SystemTime::now())
        .await
        .is_err());
    let permit = f
        .manager
        .claim_proxy(SCOPE, 8080, 2, SystemTime::now())
        .await
        .unwrap();
    f.manager
        .activate_snapshot_instance(CONNECTION, SCOPE, OP, 2)
        .await
        .unwrap();
    assert!(!permit.cancel.is_cancelled());
    drop(permit);
    assert!(f
        .manager
        .detach_snapshot_instance(SCOPE, OP, 2)
        .await
        .is_err());
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn reconnect_rejects_old_dispatch_epochs_and_cancels_old_proxies() {
    let f = Fixture::new(1).await;
    f.active().await;
    let old = f
        .manager
        .claim_proxy(SCOPE, 8080, 2, SystemTime::now())
        .await
        .unwrap();
    f.manager
        .disconnect_persistence("unrelated-connection")
        .await;
    assert!(!old.cancel.is_cancelled());
    f.manager.disconnect_persistence(CONNECTION).await;
    assert!(old.cancel.is_cancelled());
    assert!(f.manager.execute(request("disconnected", 2)).await.is_err());
    connect(&f.manager, "connection-2", &[]).await;
    assert!(f.manager.execute(request("old-epoch", 2)).await.is_err());
    let mut current = request("new-epoch", 2);
    current.connection_id = Some("connection-2".into());
    f.manager.execute(current).await.unwrap();
    f.manager.disconnect_persistence(CONNECTION).await;
    let new = f
        .manager
        .claim_proxy(SCOPE, 8080, 2, SystemTime::now())
        .await
        .unwrap();
    assert!(!new.cancel.is_cancelled());
    drop(new);
    drop(old);
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn unavailable_journal_fails_before_any_native_recovery_or_orphan_cleanup() {
    let backend = Arc::new(Backend::default());
    let m = manager(backend.clone(), 1);
    assert!(m
        .protect_persistence_recovery(CONNECTION, &[], &[hint(OP, 2)])
        .await
        .is_err());
    let results = m
        .recover_sessions(&[TerminalSessionRecoveryCandidate {
            session_id: SCOPE.into(),
            generation: 0,
            lease_expires_unix_ms: future(60),
        }])
        .await;
    assert_eq!(
        results[0].status,
        terminal_session_recovery_result::Status::Invalid as i32
    );
    assert!(backend.named.lock().await.is_empty());
    assert!(backend.pinned.lock().await.is_empty());
    assert!(backend.orphan_names.lock().await.is_empty());
    assert_eq!(
        code(m.execute(request("blank", 0)).await.unwrap_err()),
        "persistence_unavailable"
    );
    assert_eq!(backend.created.load(Ordering::SeqCst), 0);
    m.close_preserving().await;
}

#[tokio::test]
async fn held_recovery_protects_both_legacy_and_operation_names_without_starting() {
    let f = Fixture::new(1).await;
    f.journal.claim(target()).unwrap();
    f.manager
        .install_snapshot_journal(f.journal.clone())
        .await
        .unwrap();
    connect(&f.manager, CONNECTION, &[hint(OP, 2)]).await;
    let results = f
        .manager
        .recover_sessions(&[TerminalSessionRecoveryCandidate {
            session_id: SCOPE.into(),
            generation: 2,
            lease_expires_unix_ms: future(60),
        }])
        .await;
    assert_eq!(
        results[0].status,
        terminal_session_recovery_result::Status::Invalid as i32
    );
    assert!(f.backend.named.lock().await.is_empty());
    assert!(f.backend.pinned.lock().await.is_empty());
    let protected = f.backend.orphan_names.lock().await;
    assert!(protected[0].contains(&terminal_session_resource_name(SCOPE)));
    assert!(protected[0].contains(&worker_boxlite::persistence::target_name(SCOPE, OP, 2)));
    drop(protected);
    assert_eq!(f.manager.active_session_count().await, 1);
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn active_recovery_uses_only_the_selected_id_and_never_falls_back() {
    for outcome in ["running", "missing", "alias"] {
        let f = Fixture::new(1).await;
        f.journal.claim(target()).unwrap();
        f.journal
            .update(OP, |o| {
                o.phase = Phase::Importing;
                o.verified = true;
                Ok(())
            })
            .unwrap();
        f.journal
            .update(OP, |o| {
                o.phase = Phase::Ready;
                o.box_id = "recorded-id".into();
                o.lease_expires_unix_ms = future(60);
                Ok(())
            })
            .unwrap();
        f.journal
            .update(OP, |o| {
                o.phase = Phase::Active;
                Ok(())
            })
            .unwrap();
        f.backend
            .missing
            .store(outcome == "missing", Ordering::SeqCst);
        f.backend.alias.store(outcome == "alias", Ordering::SeqCst);
        f.manager
            .install_snapshot_journal(f.journal.clone())
            .await
            .unwrap();
        f.manager
            .protect_persistence_recovery(CONNECTION, &[], &[])
            .await
            .unwrap();
        let result = f
            .manager
            .recover_sessions(&[TerminalSessionRecoveryCandidate {
                session_id: SCOPE.into(),
                generation: 2,
                lease_expires_unix_ms: future(60),
            }])
            .await;
        let expected = match outcome {
            "running" => terminal_session_recovery_result::Status::Recovered,
            "missing" => terminal_session_recovery_result::Status::Missing,
            _ => terminal_session_recovery_result::Status::Invalid,
        };
        assert_eq!(result[0].status, expected as i32);
        assert_eq!(f.backend.pinned.lock().await.as_slice(), ["recorded-id"]);
        assert!(f.backend.named.lock().await.is_empty());
        assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
        assert!(f
            .manager
            .execute(request("before-recovery-ack", 2))
            .await
            .is_err());
        f.manager
            .finish_persistence_recovery(CONNECTION)
            .await
            .unwrap();
        if outcome == "running" {
            assert_eq!(
                f.manager.execute(request("ready", 2)).await.unwrap().stdout,
                "recorded-id"
            );
        } else {
            assert!(f.manager.execute(request("missing", 2)).await.is_err());
        }
        f.manager.close_preserving().await;
    }
}

#[tokio::test]
async fn lease_write_failure_closes_admission_and_preserves_the_owned_root() {
    let f = Fixture::new(1).await;
    f.active().await;
    f.journal.close().unwrap();
    assert_eq!(
        code(
            f.manager
                .execute(request("must-not-run", 2))
                .await
                .unwrap_err()
        ),
        "persistence_unavailable"
    );
    assert!(f.backend.commands.lock().await.is_empty());
    f.manager.cleanup_expired_sessions().await;
    f.manager.close().await;
    assert!(f.backend.removed.lock().await.is_empty());
}

#[tokio::test]
async fn cleanup_releases_capacity_only_after_a_terminal_journal_record() {
    let f = Fixture::new(1).await;
    f.ready().await;
    assert!(f
        .manager
        .detach_snapshot_instance(SCOPE, OP, 2)
        .await
        .is_err());
    assert_eq!(f.manager.active_session_count().await, 1);
    f.journal
        .update(OP, |o| {
            o.phase = Phase::Canceling;
            Ok(())
        })
        .unwrap();
    assert!(f
        .manager
        .detach_snapshot_instance(SCOPE, OP, 2)
        .await
        .is_err());
    f.journal
        .update(OP, |o| {
            o.phase = Phase::Canceled;
            Ok(())
        })
        .unwrap();
    f.manager
        .detach_snapshot_instance(SCOPE, OP, 2)
        .await
        .unwrap();
    assert_eq!(f.manager.active_session_count().await, 0);
    assert!(f.manager.execute(request("blank", 0)).await.is_err());
    assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn permanent_delete_cannot_be_reserved_or_revived_by_later_generations() {
    let f = Fixture::new(1).await;
    let mut deletion = target();
    deletion.operation_id = "delete-operation".into();
    deletion.kind = Kind::Delete;
    deletion.phase = Phase::Deleting;
    deletion.generation = 3;
    deletion.archive = None;
    deletion.artifact.clear();
    deletion.staging_bytes = 0;
    deletion.lease_ttl_sec = 0;
    f.journal.claim(deletion).unwrap();
    f.manager
        .install_snapshot_journal(f.journal.clone())
        .await
        .unwrap();
    connect(&f.manager, CONNECTION, &[hint("delete-operation", 3)]).await;
    assert!(f
        .manager
        .reserve_snapshot_target(SCOPE, "delete-operation", 3)
        .await
        .is_err());
    assert!(f
        .manager
        .reserve_snapshot_target(SCOPE, "later-operation", 4)
        .await
        .is_err());
    f.journal
        .update("delete-operation", |o| {
            o.phase = Phase::Deleted;
            o.tombstone_box_id = "sentinel-id".into();
            Ok(())
        })
        .unwrap();
    f.manager
        .install_snapshot_journal(f.journal.clone())
        .await
        .unwrap();
    connect(&f.manager, CONNECTION, &[hint("delete-operation", 3)]).await;
    assert_eq!(f.manager.active_session_count().await, 0);
    assert!(f
        .manager
        .reserve_snapshot_target(SCOPE, "new-restore", 4)
        .await
        .is_err());
    assert!(f.manager.execute(request("revive", 0)).await.is_err());
    assert_eq!(f.backend.created.load(Ordering::SeqCst), 0);
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn a_detached_sdk_tunnel_open_keeps_the_source_drain_permit_until_connection_drop() {
    struct Tunnel(std::sync::Arc<std::sync::atomic::AtomicBool>);
    impl Drop for Tunnel {
        fn drop(&mut self) {
            self.0.store(true, Ordering::SeqCst);
        }
    }
    let f = Fixture::new(1).await;
    f.manager.execute(request("seed", 0)).await.unwrap();
    let permit = f
        .manager
        .claim_proxy(SCOPE, 8080, 0, SystemTime::now())
        .await
        .unwrap();
    let (tx, rx) = tokio::sync::oneshot::channel::<()>();
    let dropped = Arc::new(AtomicBool::new(false));
    let observed = dropped.clone();
    let opening = crate::runner::sandbox_proxy::spawn_proxy_open(permit, move |_| async move {
        rx.await.unwrap();
        Ok::<_, ()>(Tunnel(observed))
    });
    f.manager
        .hold_snapshot_source(SCOPE, "suspend-operation", 1)
        .await
        .unwrap();
    drop(opening);
    assert!(tokio::time::timeout(
        Duration::from_millis(10),
        f.manager
            .drain_snapshot_source(SCOPE, "suspend-operation", 1)
    )
    .await
    .is_err());
    tx.send(()).unwrap();
    tokio::time::timeout(
        Duration::from_secs(1),
        f.manager
            .drain_snapshot_source(SCOPE, "suspend-operation", 1),
    )
    .await
    .unwrap()
    .unwrap();
    assert!(dropped.load(Ordering::SeqCst));
    f.manager.close_preserving().await;
}

#[tokio::test]
async fn retiring_managed_capacity_is_not_counted_as_a_second_virtual_slot() {
    let f = Fixture::new(1).await;
    f.active().await;
    f.backend.cleanup_block.store(true, Ordering::SeqCst);
    let m = f.manager.clone();
    let task = tokio::spawn(async move { m.execute(request("deadline", 2)).await });
    f.backend.started.notified().await;
    f.backend.release.notify_one();
    f.backend.cleanup_started.notified().await;
    assert_eq!(f.manager.active_session_count().await, 1);
    f.backend.cleanup_release.notify_one();
    assert!(matches!(
        task.await.unwrap(),
        Err(TerminalOperationError::DeadlineExceeded)
    ));
    assert_eq!(f.manager.active_session_count().await, 1);
    f.journal
        .update(OP, |op| {
            op.phase = Phase::Canceling;
            Ok(())
        })
        .unwrap();
    f.journal
        .update(OP, |op| {
            op.phase = Phase::Canceled;
            Ok(())
        })
        .unwrap();
    f.manager.sync_snapshot_operation(OP).await.unwrap();
    assert_eq!(f.manager.active_session_count().await, 0);
    f.manager.close_preserving().await;
}
