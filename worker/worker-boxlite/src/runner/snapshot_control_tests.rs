use super::*;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::time::Duration;

#[derive(Default)]
struct Engine {
    started: Notify,
    released: Notify,
    entered: AtomicU32,
    failed: AtomicBool,
    canceled: AtomicBool,
    cancellation_observed: Notify,
    panic: AtomicBool,
}
#[async_trait]
impl LifecycleEngine for Engine {
    async fn execute(&self, action: ActionContext) -> Result<ResultPayload, ControlError> {
        self.entered.fetch_add(1, Ordering::SeqCst);
        self.started.notify_one();
        if self.panic.load(Ordering::SeqCst) {
            panic!("controlled engine fault");
        }
        action.cancel.cancelled().await;
        self.canceled.store(true, Ordering::SeqCst);
        self.cancellation_observed.notify_one();
        // Simulate accepted SDK I/O: cancellation does not end the operation.
        self.released.notified().await;
        Ok(ResultPayload {
            state: "prepared".into(),
            ..Default::default()
        })
    }
    fn fail_closed(&self) {
        self.failed.store(true, Ordering::SeqCst);
    }
}
fn frame(scope: &str) -> SandboxPersistenceControl {
    SandboxPersistenceControl {
        request_id: "request".into(),
        operation_id: "operation".into(),
        scoped_session_id: scope.into(),
        generation: 2,
        action: Action::Restore as i32,
        deadline_unix_ms: now_ms() + 60_000,
        payload_json: b"{}".to_vec(),
    }
}
async fn connected(engine: Arc<Engine>, slots: usize) -> Arc<ControlDispatcher> {
    let d = ControlDispatcher::new(engine, slots).unwrap();
    d.begin_connection("connection-1").await.unwrap();
    d.finish_connection("connection-1").unwrap();
    d
}
fn busy(report: SandboxPersistenceReport) {
    assert!(report.completed && report.error.is_none());
    assert_eq!(report.payload_json, br#"{"state":"busy"}"#);
}
#[tokio::test]
async fn dropped_report_and_cancel_retain_owned_work_and_capacity_until_completion() {
    let engine = Arc::new(Engine::default());
    let d = connected(engine.clone(), 1).await;
    let receipt = d.submit("connection-1", frame("scope-1"));
    wait_for(&engine.started).await;
    drop(receipt);
    let mut cancel = frame("scope-1");
    cancel.action = Action::Cancel as i32;
    busy(d.submit("connection-1", cancel).await.unwrap());
    busy(d.submit("connection-1", frame("scope-2")).await.unwrap());
    assert_eq!(engine.entered.load(Ordering::SeqCst), 1);
    engine.released.notify_one();
    let closing = tokio::spawn({
        let d = d.clone();
        async move { d.close().await }
    });
    closing.await.unwrap().unwrap();
    assert!(engine.canceled.load(Ordering::SeqCst));
    assert_eq!(
        d.submit("connection-1", frame("scope-2"))
            .await
            .unwrap()
            .error
            .unwrap()
            .code,
        "persistence_unavailable"
    );
}
#[tokio::test]
async fn reconnect_waits_for_old_sdk_work_and_fences_old_connection_and_stale_disconnect() {
    let engine = Arc::new(Engine::default());
    let d = connected(engine.clone(), 2).await;
    let receipt = d.submit("connection-1", frame("scope"));
    wait_for(&engine.started).await;
    let mut reconnect = tokio::spawn({
        let d = d.clone();
        async move { d.begin_connection("connection-2").await }
    });
    assert!(
        tokio::time::timeout(Duration::from_millis(30), &mut reconnect)
            .await
            .is_err()
    );
    assert_eq!(
        d.submit("connection-1", frame("other"))
            .await
            .unwrap()
            .error
            .unwrap()
            .code,
        "session_state_conflict"
    );
    busy(d.submit("connection-2", frame("other")).await.unwrap());
    engine.released.notify_one();
    receipt.await.unwrap();
    reconnect.await.unwrap().unwrap();
    d.finish_connection("connection-2").unwrap();
    d.disconnect("connection-1");
    let next = d.submit("connection-2", frame("next"));
    wait_for(&engine.started).await;
    d.disconnect("connection-2");
    engine.released.notify_one();
    assert!(next.await.unwrap().error.is_none());
    d.close().await.unwrap();
}
#[tokio::test]
async fn deadline_cancels_but_does_not_abandon_the_selected_sdk_operation() {
    let engine = Arc::new(Engine::default());
    let d = connected(engine.clone(), 1).await;
    let mut command = frame("scope");
    command.deadline_unix_ms = now_ms() + 250;
    let receipt = d.submit("connection-1", command);
    wait_for(&engine.started).await;
    wait_for(&engine.cancellation_observed).await;
    assert!(engine.canceled.load(Ordering::SeqCst));
    busy(d.submit("connection-1", frame("other")).await.unwrap());
    engine.released.notify_one();
    assert!(receipt.await.unwrap().error.is_none());
    d.close().await.unwrap();
}
#[tokio::test]
async fn only_matching_cancel_or_newer_permanent_delete_interrupts_an_action() {
    let engine = Arc::new(Engine::default());
    let d = connected(engine.clone(), 2).await;
    let receipt = d.submit("connection-1", frame("scope"));
    wait_for(&engine.started).await;
    let mut wrong = frame("scope");
    wrong.operation_id = "wrong".into();
    wrong.action = Action::Cancel as i32;
    busy(d.submit("connection-1", wrong.clone()).await.unwrap());
    assert!(!d.state.lock().unwrap().actions["scope"]
        .cancel
        .is_cancelled());
    wrong.generation = 3;
    wrong.payload_json = br#"{"purge_session":true}"#.to_vec();
    busy(d.submit("connection-1", wrong).await.unwrap());
    assert!(d.state.lock().unwrap().actions["scope"]
        .cancel
        .is_cancelled());
    engine.released.notify_one();
    receipt.await.unwrap();
    d.close().await.unwrap();
}
#[tokio::test]
async fn invalid_private_frames_never_enter_the_engine_or_expose_payload_values() {
    let engine = Arc::new(Engine::default());
    let d = connected(engine.clone(), 1).await;
    for payload in [
        b"{\"unknown\":\"private-grant\"}".to_vec(),
        b"{} {}".to_vec(),
        br#"{"rollback":true,"rollback":false}"#.to_vec(),
        vec![b' '; (64 << 10) + 1],
    ] {
        let mut command = frame("scope");
        command.payload_json = payload;
        let response = d.submit("connection-1", command).await.unwrap();
        assert_eq!(
            response.error.as_ref().unwrap().code,
            "session_state_conflict"
        );
        assert!(!response.error.unwrap().message.contains("private-grant"));
    }
    for broken in ["id", "scope", "generation", "deadline", "action"] {
        let mut command = frame("scope");
        match broken {
            "id" => command.operation_id = "../invalid".into(),
            "scope" => command.scoped_session_id = " scope".into(),
            "generation" => command.generation = 0,
            "deadline" => command.deadline_unix_ms = now_ms(),
            _ => command.action = 1234,
        }
        assert!(d
            .submit("connection-1", command)
            .await
            .unwrap()
            .error
            .is_some());
    }
    assert_eq!(engine.entered.load(Ordering::SeqCst), 0);
    d.close().await.unwrap();
}
#[tokio::test]
async fn engine_panic_quarantines_dispatcher_instead_of_releasing_a_new_action() {
    let engine = Arc::new(Engine::default());
    engine.panic.store(true, Ordering::SeqCst);
    let d = connected(engine.clone(), 1).await;
    let response = d.submit("connection-1", frame("scope")).await.unwrap();
    assert_eq!(response.error.unwrap().code, "persistence_unavailable");
    assert!(engine.failed.load(Ordering::SeqCst));
    assert!(d.begin_connection("connection-2").await.is_err());
    assert_eq!(
        d.submit("connection-1", frame("other"))
            .await
            .unwrap()
            .error
            .unwrap()
            .code,
        "persistence_unavailable"
    );
    assert_eq!(d.close().await, Err(ControlError::Unavailable));
}

#[tokio::test]
async fn shutdown_cancels_and_waits_for_uninterruptible_owned_work() {
    let engine = Arc::new(Engine::default());
    let dispatcher = connected(engine.clone(), 1).await;
    let receipt = dispatcher.submit("connection-1", frame("scope"));
    wait_for(&engine.started).await;
    let mut closing = tokio::spawn({
        let dispatcher = dispatcher.clone();
        async move { dispatcher.close().await }
    });
    assert!(
        tokio::time::timeout(Duration::from_millis(30), &mut closing)
            .await
            .is_err()
    );
    assert!(engine.canceled.load(Ordering::SeqCst));
    assert_eq!(
        dispatcher
            .submit("connection-1", frame("other"))
            .await
            .unwrap()
            .error
            .unwrap()
            .code,
        "persistence_unavailable"
    );
    engine.released.notify_one();
    closing.await.unwrap().unwrap();
    assert!(receipt.await.unwrap().error.is_none());
}

async fn wait_for(notice: &Notify) {
    tokio::time::timeout(Duration::from_secs(2), notice.notified())
        .await
        .unwrap();
}

#[tokio::test]
async fn a_delivery_that_expires_before_task_execution_never_enters_the_engine() {
    let engine = Arc::new(Engine::default());
    let dispatcher = connected(engine.clone(), 1).await;
    let mut delivery = frame("scope");
    delivery.deadline_unix_ms = now_ms() + 20;
    let receipt = dispatcher.submit("connection-1", delivery);
    // The current-thread runtime cannot poll the owned task until we yield.
    std::thread::sleep(Duration::from_millis(35));
    busy(
        tokio::time::timeout(Duration::from_secs(2), receipt)
            .await
            .unwrap()
            .unwrap(),
    );
    assert_eq!(engine.entered.load(Ordering::SeqCst), 0);
    dispatcher.close().await.unwrap();
}
