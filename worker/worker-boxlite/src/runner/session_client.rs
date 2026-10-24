use std::future::Future;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use rand::Rng;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::sync::CancellationToken;
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};

use crate::config::Config;
use crate::proto::registryv1::{
    connect_request, connect_response, terminal_session_recovery_result,
    worker_registry_service_client::WorkerRegistryServiceClient, CommandDispatch, ConnectRequest,
    ConnectResponse, HeartbeatAck, HeartbeatFrame, TerminalSessionRecoveryReport,
};

use super::terminal_session_manager::{
    shared_active_session_count, shared_recover_terminal_sessions, shared_terminal_session_manager,
};
use super::{build_hello, command_dispatch_summary_for_log, duration_from_server, RunnerError};

pub(crate) async fn run_session(
    shutdown: CancellationToken,
    cfg: &Config,
) -> Result<(), RunnerError> {
    let manager = shared_terminal_session_manager(cfg);
    if manager.has_snapshot_journal().await {
        manager.disconnect_current_persistence().await;
    }
    let result =
        run_session_with_builder(shutdown, cfg, Arc::new(DefaultCommandResultBuilder)).await;
    if manager.has_snapshot_journal().await {
        manager.disconnect_current_persistence().await;
    }
    result
}

async fn run_session_with_builder(
    shutdown: CancellationToken,
    cfg: &Config,
    command_result_builder: Arc<dyn CommandResultBuilder>,
) -> Result<(), RunnerError> {
    let connection_shutdown = shutdown.child_token();
    let _connection_guard = CancelConnectionOnDrop(connection_shutdown.clone());
    let (outbound_tx, outbound_rx) = mpsc::channel(64);
    let hello = build_hello(cfg, shared_active_session_count().await)?;
    outbound_tx
        .send(ConnectRequest {
            payload: Some(connect_request::Payload::Hello(hello.clone())),
        })
        .await
        .map_err(|_| RunnerError::Message("send hello failed".to_owned()))?;

    let channel = dial(cfg).await?;
    let mut client = WorkerRegistryServiceClient::new(channel);
    let response = client.connect(ReceiverStream::new(outbound_rx)).await?;
    let mut inbound = response.into_inner();

    let connect_ack = recv_with_timeout(shutdown.clone(), cfg.call_timeout, inbound.message())
        .await?
        .ok_or_else(|| RunnerError::Message("unexpected first response frame".to_owned()))?;

    let connect_response::Payload::ConnectAck(ack) = connect_ack
        .payload
        .ok_or_else(|| RunnerError::Message("unexpected first response frame".to_owned()))?
    else {
        return Err(RunnerError::Message(
            "unexpected first response frame".to_owned(),
        ));
    };

    if ack.session_id.trim().is_empty() {
        return Err(RunnerError::Message(
            "connect_ack.session_id is required".to_owned(),
        ));
    }

    let dispatcher = command_result_builder.persistence_dispatcher();
    let _dispatcher_guard = dispatcher
        .as_ref()
        .map(|dispatcher| DispatcherConnectionGuard {
            dispatcher: dispatcher.clone(),
            connection: ack.session_id.clone(),
        });
    if let Some(dispatcher) = &dispatcher {
        tokio::time::timeout(
            cfg.call_timeout,
            dispatcher.begin_connection(&ack.session_id),
        )
        .await
        .map_err(|_| {
            RunnerError::Message(
                "sandbox persistence is still finishing previous connection work".into(),
            )
        })?
        .map_err(|_| {
            RunnerError::Message("sandbox persistence connection admission failed".into())
        })?;
    }
    let manager = shared_terminal_session_manager(cfg);
    let persistent_recovery = manager.has_snapshot_journal().await
        || !ack.sandbox_persistence_recovery_candidates.is_empty()
        || ack
            .terminal_session_recovery_candidates
            .iter()
            .any(|c| c.generation != 0);
    if persistent_recovery {
        manager
            .protect_persistence_recovery(
                &ack.session_id,
                &ack.terminal_session_recovery_candidates,
                &ack.sandbox_persistence_recovery_candidates,
            )
            .await
            .map_err(|_| {
                RunnerError::Message(
                    "sandbox persistence recovery could not establish ownership".into(),
                )
            })?;
    }
    let recovery_results = tokio::select! {
        _ = shutdown.cancelled() => return Err(RunnerError::Message("worker shutdown during terminal session recovery".to_owned())),
        result = tokio::time::timeout(
            cfg.call_timeout,
            command_result_builder.recover(cfg, &ack.terminal_session_recovery_candidates),
        ) => result.map_err(|_| RunnerError::Message("terminal session recovery deadline exceeded".to_owned()))?,
    };
    outbound_tx
        .send(ConnectRequest {
            payload: Some(connect_request::Payload::TerminalSessionRecoveryReport(
                TerminalSessionRecoveryReport {
                    results: recovery_results.clone(),
                },
            )),
        })
        .await
        .map_err(|_| {
            RunnerError::Message("send terminal session recovery report failed".to_owned())
        })?;
    let recovery_ack = recv_with_timeout(shutdown.clone(), cfg.call_timeout, inbound.message())
        .await?
        .ok_or_else(|| {
            RunnerError::Message("stream closed before terminal session recovery ack".to_owned())
        })?;
    if !matches!(
        recovery_ack.payload,
        Some(connect_response::Payload::TerminalSessionRecoveryAck(_))
    ) {
        return Err(RunnerError::Message(
            "unexpected response while waiting for terminal session recovery ack".to_owned(),
        ));
    }

    if persistent_recovery {
        manager
            .finish_persistence_recovery(&ack.session_id)
            .await
            .map_err(|_| {
                RunnerError::Message("sandbox persistence recovery could not open admission".into())
            })?;
    }
    if let Some(dispatcher) = &dispatcher {
        dispatcher.finish_connection(&ack.session_id).map_err(|_| {
            RunnerError::Message("sandbox persistence connection recovery did not finish".into())
        })?;
    }
    tracing::info!(
        executor_kind = "boxlite",
        candidates = recovery_results.len(),
        recovered = recovery_results
            .iter()
            .filter(|result| {
                result.status == terminal_session_recovery_result::Status::Recovered as i32
            })
            .count(),
        missing = recovery_results
            .iter()
            .filter(|result| {
                result.status == terminal_session_recovery_result::Status::Missing as i32
            })
            .count(),
        invalid = recovery_results
            .iter()
            .filter(|result| {
                result.status == terminal_session_recovery_result::Status::Invalid as i32
            })
            .count(),
        "terminal session recovery acknowledged"
    );

    let heartbeat_interval =
        duration_from_server(ack.heartbeat_interval_sec, cfg.heartbeat_interval);
    tracing::info!(
        node_id = %hello.node_id,
        node_name = %hello.node_name,
        session_id = %ack.session_id,
        "worker connected"
    );

    let (heartbeat_ack_tx, heartbeat_ack_rx) = mpsc::channel(16);
    let (session_err_tx, session_err_rx) = mpsc::channel(4);

    tokio::spawn(receiver_loop(
        connection_shutdown.clone(),
        inbound,
        outbound_tx.clone(),
        heartbeat_ack_tx,
        session_err_tx,
        cfg.clone(),
        ConnectionCommands {
            builder: command_result_builder,
            connection_id: ack.session_id.clone(),
            dispatcher,
        },
    ));

    heartbeat_loop(
        shutdown,
        outbound_tx,
        heartbeat_ack_rx,
        session_err_rx,
        cfg,
        ack.session_id,
        heartbeat_interval,
    )
    .await
}

#[async_trait]
trait CommandResultBuilder: Send + Sync {
    fn persistence_dispatcher(&self) -> Option<Arc<super::snapshot_control::ControlDispatcher>> {
        None
    }

    async fn recover(
        &self,
        cfg: &Config,
        candidates: &[crate::proto::registryv1::TerminalSessionRecoveryCandidate],
    ) -> Vec<crate::proto::registryv1::TerminalSessionRecoveryResult> {
        shared_recover_terminal_sessions(cfg, candidates).await
    }
    async fn build(
        &self,
        cfg: &Config,
        dispatch: CommandDispatch,
        connection: &str,
    ) -> ConnectRequest;
}

struct DefaultCommandResultBuilder;

#[async_trait]
impl CommandResultBuilder for DefaultCommandResultBuilder {
    fn persistence_dispatcher(&self) -> Option<Arc<super::snapshot_control::ControlDispatcher>> {
        super::snapshot_control::shared_dispatcher()
    }

    async fn build(
        &self,
        cfg: &Config,
        dispatch: CommandDispatch,
        connection: &str,
    ) -> ConnectRequest {
        super::capability_executor::build_command_result_for_connection(cfg, dispatch, connection)
            .await
    }
}

async fn dial(cfg: &Config) -> Result<Channel, RunnerError> {
    let scheme = if cfg.console_tls { "https" } else { "http" };
    let mut endpoint = Endpoint::from_shared(format!("{scheme}://{}", cfg.console_grpc_target))
        .map_err(|err| RunnerError::Message(format!("invalid console target: {err}")))?;
    if cfg.console_tls {
        endpoint = endpoint.tls_config(ClientTlsConfig::new())?;
    }
    endpoint.connect().await.map_err(RunnerError::from)
}

struct ConnectionCommands {
    builder: Arc<dyn CommandResultBuilder>,
    connection_id: String,
    dispatcher: Option<Arc<super::snapshot_control::ControlDispatcher>>,
}

async fn receiver_loop(
    shutdown: CancellationToken,
    mut inbound: tonic::Streaming<ConnectResponse>,
    outbound_tx: mpsc::Sender<ConnectRequest>,
    heartbeat_ack_tx: mpsc::Sender<HeartbeatAck>,
    session_err_tx: mpsc::Sender<RunnerError>,
    cfg: Config,
    command: ConnectionCommands,
) {
    let ConnectionCommands {
        builder: command_result_builder,
        connection_id,
        dispatcher,
    } = command;
    let report_slots = Arc::new(tokio::sync::Semaphore::new(64));
    loop {
        let message = tokio::select! {
            _ = shutdown.cancelled() => return,
            message = inbound.message() => message,
        };

        match message {
            Ok(Some(response)) => match response.payload {
                Some(connect_response::Payload::HeartbeatAck(ack)) => {
                    if heartbeat_ack_tx.send(ack).await.is_err() {
                        return;
                    }
                }
                Some(connect_response::Payload::CommandDispatch(dispatch)) => {
                    handle_command_dispatch(
                        shutdown.clone(),
                        outbound_tx.clone(),
                        session_err_tx.clone(),
                        cfg.clone(),
                        dispatch,
                        command_result_builder.clone(),
                        connection_id.clone(),
                    );
                }
                Some(connect_response::Payload::SandboxPersistenceControl(control)) => {
                    if let Some(dispatcher) = &dispatcher {
                        if let Err(error) = dispatch_lifecycle_control(
                            dispatcher,
                            &connection_id,
                            control,
                            outbound_tx.clone(),
                            shutdown.clone(),
                            report_slots.clone(),
                        ) {
                            report_session_error(&session_err_tx, error).await;
                            return;
                        }
                        continue;
                    }
                    let report = super::snapshot_control::unavailable_report(&control);
                    if outbound_tx
                        .send(ConnectRequest {
                            payload: Some(connect_request::Payload::SandboxPersistenceReport(
                                report,
                            )),
                        })
                        .await
                        .is_err()
                    {
                        return;
                    }
                }
                _ => {
                    report_session_error(
                        &session_err_tx,
                        RunnerError::Message("unexpected response frame".to_owned()),
                    )
                    .await;
                    return;
                }
            },
            Ok(None) => {
                report_session_error(
                    &session_err_tx,
                    RunnerError::Message("stream closed before command loop finished".to_owned()),
                )
                .await;
                return;
            }
            Err(status) => {
                report_session_error(&session_err_tx, RunnerError::from(status)).await;
                return;
            }
        }
    }
}

fn handle_command_dispatch(
    shutdown: CancellationToken,
    outbound_tx: mpsc::Sender<ConnectRequest>,
    session_err_tx: mpsc::Sender<RunnerError>,
    cfg: Config,
    dispatch: CommandDispatch,
    command_result_builder: Arc<dyn CommandResultBuilder>,
    connection_id: String,
) {
    let capability = dispatch.capability.trim().to_ascii_lowercase();
    let command_id = dispatch.command_id.trim().to_owned();
    let summary = command_dispatch_summary_for_log(&capability, &dispatch.payload_json);
    tracing::info!(
        command_id = %command_id,
        capability = %capability,
        summary = %summary,
        "command dispatch received"
    );

    tokio::spawn(async move {
        if shutdown.is_cancelled() {
            return;
        }
        let result = command_result_builder
            .build(&cfg, dispatch, &connection_id)
            .await;
        let send_result = tokio::select! {
            _ = shutdown.cancelled() => Err(()),
            send_result = outbound_tx.send(result) => send_result.map_err(|_| ())
        };

        if send_result.is_err() {
            report_session_error(
                &session_err_tx,
                RunnerError::Message("enqueue command result failed".to_owned()),
            )
            .await;
        }
    });
}

async fn heartbeat_loop(
    shutdown: CancellationToken,
    outbound_tx: mpsc::Sender<ConnectRequest>,
    mut heartbeat_ack_rx: mpsc::Receiver<HeartbeatAck>,
    mut session_err_rx: mpsc::Receiver<RunnerError>,
    cfg: &Config,
    session_id: String,
    heartbeat_interval: Duration,
) -> Result<(), RunnerError> {
    let mut interval = heartbeat_interval;
    let mut consecutive_ack_timeouts = 0_u8;

    loop {
        let wait_for = jitter_duration(interval, cfg.heartbeat_jitter_pct);
        tokio::select! {
            _ = shutdown.cancelled() => return Err(RunnerError::Cancelled),
            maybe_err = session_err_rx.recv() => {
                if let Some(err) = maybe_err {
                    return Err(err);
                }
                return Err(RunnerError::Message("session error channel closed".to_owned()));
            }
            _ = tokio::time::sleep(wait_for) => {}
        }

        let heartbeat = ConnectRequest {
            payload: Some(connect_request::Payload::Heartbeat(HeartbeatFrame {
                sandbox_persistence: super::persistence_startup::usage(cfg)?,
                node_id: cfg.worker_id.clone(),
                session_id: session_id.clone(),
                active_session_count: shared_active_session_count().await,
            })),
        };
        enqueue_heartbeat(&shutdown, &outbound_tx, &mut session_err_rx, heartbeat).await?;

        let ack = tokio::select! {
            _ = shutdown.cancelled() => return Err(RunnerError::Cancelled),
            maybe_err = session_err_rx.recv() => {
                if let Some(err) = maybe_err {
                    return Err(err);
                }
                return Err(RunnerError::Message("session error channel closed".to_owned()));
            }
            maybe_ack = heartbeat_ack_rx.recv() => maybe_ack,
            _ = tokio::time::sleep(cfg.call_timeout) => {
                consecutive_ack_timeouts = consecutive_ack_timeouts.saturating_add(1);
                if consecutive_ack_timeouts >= 2 {
                    return Err(RunnerError::Message("heartbeat ack deadline exceeded".to_owned()));
                }
                continue;
            }
        };

        let ack =
            ack.ok_or_else(|| RunnerError::Message("heartbeat ack channel closed".to_owned()))?;
        consecutive_ack_timeouts = 0;
        interval = duration_from_server(ack.heartbeat_interval_sec, interval);
    }
}

async fn enqueue_heartbeat(
    shutdown: &CancellationToken,
    outbound: &mpsc::Sender<ConnectRequest>,
    errors: &mut mpsc::Receiver<RunnerError>,
    heartbeat: ConnectRequest,
) -> Result<(), RunnerError> {
    tokio::select! {
        _ = shutdown.cancelled() => Err(RunnerError::Cancelled),
        error = errors.recv() => Err(error.unwrap_or_else(|| RunnerError::Message("session error channel closed".into()))),
        result = outbound.send(heartbeat) => result.map_err(|_| RunnerError::Message("enqueue heartbeat failed".into())),
    }
}

fn jitter_duration(base: Duration, pct: u8) -> Duration {
    if pct == 0 {
        return base;
    }

    let base_ms = base.as_millis().max(1);
    let min_ms = base_ms.saturating_mul((100 - pct as u128).max(1)) / 100;
    let max_ms = base_ms.saturating_mul(100 + pct as u128) / 100;
    let sample_ms = rand::thread_rng().gen_range(min_ms..=max_ms.max(min_ms));
    Duration::from_millis(sample_ms as u64)
}

async fn report_session_error(session_err_tx: &mpsc::Sender<RunnerError>, err: RunnerError) {
    let _ = session_err_tx.try_send(err);
}

async fn recv_with_timeout<F, T>(
    shutdown: CancellationToken,
    timeout: Duration,
    future: F,
) -> Result<Option<T>, RunnerError>
where
    F: Future<Output = Result<Option<T>, tonic::Status>>,
{
    tokio::select! {
        _ = shutdown.cancelled() => Err(RunnerError::Cancelled),
        result = tokio::time::timeout(timeout, future) => {
            match result {
                Ok(Ok(value)) => Ok(value),
                Ok(Err(status)) => Err(RunnerError::from(status)),
                Err(_) => Err(RunnerError::Message("receive timed out".to_owned())),
            }
        }
    }
}

struct CancelConnectionOnDrop(CancellationToken);
impl Drop for CancelConnectionOnDrop {
    fn drop(&mut self) {
        self.0.cancel();
    }
}

fn dispatch_lifecycle_control(
    dispatcher: &Arc<super::snapshot_control::ControlDispatcher>,
    connection: &str,
    control: crate::proto::registryv1::SandboxPersistenceControl,
    outbound: mpsc::Sender<ConnectRequest>,
    stopped: CancellationToken,
    report_slots: Arc<tokio::sync::Semaphore>,
) -> Result<(), RunnerError> {
    let permit = report_slots.try_acquire_owned().map_err(|_| {
        RunnerError::Message("sandbox lifecycle report capacity is exhausted".into())
    })?;
    // Reserve bounded report capacity BEFORE admitting another native action.
    let receipt = dispatcher.submit(connection, control);
    tokio::spawn(async move {
        let _permit = permit;
        let report = tokio::select! {
            _ = stopped.cancelled() => return,
            report = receipt => match report { Ok(report) => report, Err(_) => return },
        };
        tokio::select! {
            _ = stopped.cancelled() => (),
            _ = outbound.send(ConnectRequest { payload: Some(connect_request::Payload::SandboxPersistenceReport(report)) }) => (),
        }
    });
    Ok(())
}

struct DispatcherConnectionGuard {
    dispatcher: Arc<super::snapshot_control::ControlDispatcher>,
    connection: String,
}
impl Drop for DispatcherConnectionGuard {
    fn drop(&mut self) {
        self.dispatcher.disconnect(&self.connection);
    }
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};

    use tokio::net::TcpListener;
    use tokio::sync::{oneshot, Mutex};
    use tokio::task::JoinHandle;
    use tokio::time::timeout;
    use tokio_stream::wrappers::{ReceiverStream, TcpListenerStream};
    use tonic::{Request, Response, Status};

    mod test_registryv1 {
        include!(concat!(
            env!("OUT_DIR"),
            "/test-server/onlyboxes.registry.v1.rs"
        ));
    }

    use crate::proto::registryv1::{
        connect_request, CommandDispatch, CommandResult as ClientCommandResult, ConnectRequest,
    };
    use test_registryv1::worker_registry_service_server::{
        WorkerRegistryService, WorkerRegistryServiceServer,
    };
    use test_registryv1::{
        connect_request as test_connect_request, connect_response as test_connect_response,
        CommandDispatch as TestCommandDispatch, CommandResult, ConnectAck, ConnectHello,
        ConnectRequest as TestConnectRequest, ConnectResponse, HeartbeatAck, HeartbeatFrame,
        TerminalSessionRecoveryAck,
    };

    use super::*;

    const TEST_SESSION_ID: &str = "registry-session-1";

    struct FakeCommandResultBuilder;

    #[derive(Clone)]
    struct FakeRegistry {
        shared: std::sync::Arc<FakeRegistryShared>,
    }

    struct FakeRegistryShared {
        heartbeat_interval_sec: i32,
        ack_heartbeats: bool,
        dispatches: Vec<TestCommandDispatch>,
        controls: Vec<test_registryv1::SandboxPersistenceControl>,
        persistence_report_tx: mpsc::UnboundedSender<test_registryv1::SandboxPersistenceReport>,
        hello_tx: Mutex<Option<oneshot::Sender<ConnectHello>>>,
        command_result_tx: mpsc::UnboundedSender<CommandResult>,
        heartbeat_tx: mpsc::UnboundedSender<HeartbeatFrame>,
    }

    struct TestRegistryHandle {
        target: String,
        hello_rx: oneshot::Receiver<ConnectHello>,
        command_result_rx: mpsc::UnboundedReceiver<CommandResult>,
        heartbeat_rx: mpsc::UnboundedReceiver<HeartbeatFrame>,
        persistence_report_rx: mpsc::UnboundedReceiver<test_registryv1::SandboxPersistenceReport>,
        shutdown: CancellationToken,
        server_task: JoinHandle<Result<(), tonic::transport::Error>>,
    }

    #[async_trait]
    impl CommandResultBuilder for FakeCommandResultBuilder {
        async fn recover(
            &self,
            _cfg: &Config,
            candidates: &[crate::proto::registryv1::TerminalSessionRecoveryCandidate],
        ) -> Vec<crate::proto::registryv1::TerminalSessionRecoveryResult> {
            assert!(candidates.is_empty());
            Vec::new()
        }

        async fn build(
            &self,
            _cfg: &Config,
            dispatch: CommandDispatch,
            connection: &str,
        ) -> ConnectRequest {
            assert_eq!(connection, TEST_SESSION_ID);
            let payload_json = match dispatch.capability.as_str() {
                "pythonExec" => serde_json::json!({
                    "output": "hello from python\n",
                    "stderr": "",
                    "exit_code": 0
                }),
                "terminalExec" => serde_json::json!({
                    "session_id": "sess-term-1",
                    "created": true,
                    "stdout": "pwd\n",
                    "stderr": "",
                    "exit_code": 0,
                    "stdout_truncated": false,
                    "stderr_truncated": false,
                    "lease_expires_unix_ms": 123456789
                }),
                "terminalResource" => serde_json::json!({
                    "session_id": "sess-term-1",
                    "file_path": "/tmp/app.txt",
                    "mime_type": "text/plain",
                    "size_bytes": 3,
                    "blob": "YWJj"
                }),
                _ => serde_json::json!({"message":"hello"}),
            };

            ConnectRequest {
                payload: Some(connect_request::Payload::CommandResult(
                    ClientCommandResult {
                        command_id: dispatch.command_id,
                        error: None,
                        payload_json: serde_json::to_vec(&payload_json).unwrap(),
                        completed_unix_ms: 1,
                    },
                )),
            }
        }
    }

    #[tonic::async_trait]
    impl WorkerRegistryService for FakeRegistry {
        type ConnectStream = ReceiverStream<Result<ConnectResponse, Status>>;

        async fn connect(
            &self,
            request: Request<tonic::Streaming<TestConnectRequest>>,
        ) -> Result<Response<Self::ConnectStream>, Status> {
            let mut inbound = request.into_inner();
            let shared = self.shared.clone();
            let first = match inbound.message().await {
                Ok(Some(frame)) => frame,
                Ok(None) => return Err(Status::invalid_argument("first frame must be hello")),
                Err(status) => return Err(status),
            };

            let Some(test_connect_request::Payload::Hello(hello)) = first.payload else {
                return Err(Status::invalid_argument("first frame must be hello"));
            };

            if let Some(hello_tx) = shared.hello_tx.lock().await.take() {
                let _ = hello_tx.send(hello);
            }

            let (response_tx, response_rx) = mpsc::channel(32);
            response_tx
                .send(Ok(ConnectResponse {
                    payload: Some(test_connect_response::Payload::ConnectAck(ConnectAck {
                        session_id: TEST_SESSION_ID.to_owned(),
                        heartbeat_interval_sec: shared.heartbeat_interval_sec,
                        terminal_session_recovery_candidates: Vec::new(),
                        sandbox_persistence_recovery_candidates: Vec::new(),
                    })),
                }))
                .await
                .map_err(|_| Status::internal("failed to enqueue connect ack"))?;

            tokio::spawn(async move {
                match inbound.message().await {
                    Ok(Some(frame))
                        if matches!(
                            frame.payload,
                            Some(test_connect_request::Payload::TerminalSessionRecoveryReport(_))
                        ) => {}
                    _ => return,
                }
                if response_tx
                    .send(Ok(ConnectResponse {
                        payload: Some(test_connect_response::Payload::TerminalSessionRecoveryAck(
                            TerminalSessionRecoveryAck {},
                        )),
                    }))
                    .await
                    .is_err()
                {
                    return;
                }

                for dispatch in &shared.dispatches {
                    if response_tx
                        .send(Ok(ConnectResponse {
                            payload: Some(test_connect_response::Payload::CommandDispatch(
                                dispatch.clone(),
                            )),
                        }))
                        .await
                        .is_err()
                    {
                        return;
                    }
                }

                for control in &shared.controls {
                    if response_tx
                        .send(Ok(ConnectResponse {
                            payload: Some(
                                test_connect_response::Payload::SandboxPersistenceControl(
                                    control.clone(),
                                ),
                            ),
                        }))
                        .await
                        .is_err()
                    {
                        return;
                    }
                }
                loop {
                    match inbound.message().await {
                        Ok(Some(frame)) => match frame.payload {
                            Some(test_connect_request::Payload::Heartbeat(heartbeat)) => {
                                let _ = shared.heartbeat_tx.send(heartbeat);
                                if shared.ack_heartbeats {
                                    let _ = response_tx
                                        .send(Ok(ConnectResponse {
                                            payload: Some(
                                                test_connect_response::Payload::HeartbeatAck(
                                                    HeartbeatAck {
                                                        heartbeat_interval_sec: shared
                                                            .heartbeat_interval_sec,
                                                    },
                                                ),
                                            ),
                                        }))
                                        .await;
                                }
                            }
                            Some(test_connect_request::Payload::CommandResult(result)) => {
                                let _ = shared.command_result_tx.send(result);
                            }
                            Some(test_connect_request::Payload::SandboxPersistenceReport(
                                report,
                            )) => {
                                let _ = shared.persistence_report_tx.send(report);
                            }
                            Some(test_connect_request::Payload::Hello(_))
                            | Some(test_connect_request::Payload::TerminalSessionRecoveryReport(
                                _,
                            ))
                            | None => return,
                        },
                        Ok(None) | Err(_) => return,
                    }
                }
            });

            Ok(Response::new(ReceiverStream::new(response_rx)))
        }
    }

    async fn spawn_fake_registry(
        dispatches: Vec<CommandDispatch>,
        ack_heartbeats: bool,
        heartbeat_interval_sec: i32,
    ) -> TestRegistryHandle {
        spawn_fake_registry_with_controls(
            dispatches,
            ack_heartbeats,
            heartbeat_interval_sec,
            Vec::new(),
        )
        .await
    }
    async fn spawn_fake_registry_with_controls(
        dispatches: Vec<CommandDispatch>,
        ack_heartbeats: bool,
        heartbeat_interval_sec: i32,
        controls: Vec<test_registryv1::SandboxPersistenceControl>,
    ) -> TestRegistryHandle {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let target = listener.local_addr().unwrap().to_string();
        let incoming = TcpListenerStream::new(listener);
        let shutdown = CancellationToken::new();
        let server_shutdown = shutdown.clone();

        let (hello_tx, hello_rx) = oneshot::channel();
        let (command_result_tx, command_result_rx) = mpsc::unbounded_channel();
        let (heartbeat_tx, heartbeat_rx) = mpsc::unbounded_channel();
        let (persistence_report_tx, persistence_report_rx) = mpsc::unbounded_channel();

        let shared = std::sync::Arc::new(FakeRegistryShared {
            heartbeat_interval_sec,
            ack_heartbeats,
            dispatches: dispatches
                .into_iter()
                .map(|dispatch| TestCommandDispatch {
                    command_id: dispatch.command_id,
                    capability: dispatch.capability,
                    payload_json: dispatch.payload_json,
                    deadline_unix_ms: dispatch.deadline_unix_ms,
                })
                .collect(),
            hello_tx: Mutex::new(Some(hello_tx)),
            command_result_tx,
            heartbeat_tx,
            controls,
            persistence_report_tx,
        });

        let server_task = tokio::spawn(async move {
            tonic::transport::Server::builder()
                .add_service(WorkerRegistryServiceServer::new(FakeRegistry { shared }))
                .serve_with_incoming_shutdown(incoming, server_shutdown.cancelled())
                .await
        });

        TestRegistryHandle {
            target,
            hello_rx,
            command_result_rx,
            heartbeat_rx,
            shutdown,
            server_task,
            persistence_report_rx,
        }
    }

    fn test_config(target: &str) -> Config {
        Config {
            config_file: None,
            console_grpc_target: target.to_owned(),
            console_tls: false,
            worker_id: "worker-12345678".to_owned(),
            worker_secret: "secret".to_owned(),
            heartbeat_interval: Duration::from_millis(25),
            heartbeat_jitter_pct: 0,
            call_timeout: Duration::from_millis(250),
            node_name: "worker-boxlite-test".to_owned(),
            executor_kind: "boxlite".to_owned(),
            labels: BTreeMap::new(),
            persistence: Default::default(),
            boxlite_home: std::env::temp_dir()
                .join(format!(
                    "onlyboxes-worker-boxlite-session-client-tests-{}",
                    std::process::id()
                ))
                .to_string_lossy()
                .into_owned(),
            python_exec_image: "ghcr.io/astral-sh/uv:python3.12-bookworm-slim".to_owned(),
            python_exec_memory_mib: 256,
            python_exec_cpus: 1,
            python_exec_max_processes: 128,
            terminal_exec_image: "coolfan1024/onlyboxes-runtime:default".to_owned(),
            terminal_exec_memory_mib: 256,
            terminal_exec_disk_size_gb: None,
            terminal_exec_cpus: 1,
            terminal_exec_max_processes: 128,
            terminal_lease_min_sec: 60,
            terminal_lease_max_sec: 1800,
            terminal_lease_default_sec: 60,
            terminal_output_limit_bytes: 1024 * 1024,
            terminal_export_max_bytes: 0,
            terminal_session_max_inflight: 1,
            terminal_max_active_sessions: 0,
            echo_max_inflight: 4,
            python_exec_max_inflight: 4,
            terminal_exec_max_inflight: 4,
            terminal_resource_max_inflight: 4,
            proxy_enabled: false,
            proxy_listen_addr: "0.0.0.0:8091".to_owned(),
            proxy_advertise_addr: String::new(),
            proxy_sandbox_ports: Vec::new(),
            log_level: "info".to_owned(),
            log_format: "json".to_owned(),
            log_add_source: false,
        }
    }

    fn assert_stopped_by_shutdown(result: Result<(), RunnerError>) {
        match result {
            Err(RunnerError::Cancelled) => {}
            Err(RunnerError::Message(message)) if message == "session error channel closed" => {}
            Err(RunnerError::Message(message)) if message == "heartbeat ack channel closed" => {}
            other => panic!("unexpected session result: {other:?}"),
        }
    }

    #[tokio::test]
    async fn run_session_sends_hello_handles_dispatch_and_heartbeat() {
        let TestRegistryHandle {
            target,
            hello_rx,
            mut command_result_rx,
            mut heartbeat_rx,
            shutdown: server_shutdown,
            server_task,
            persistence_report_rx: _,
        } = spawn_fake_registry(
            vec![CommandDispatch {
                command_id: "cmd-1".to_owned(),
                capability: "echo".to_owned(),
                payload_json: br#"{"message":"hello"}"#.to_vec(),
                deadline_unix_ms: 0,
            }],
            true,
            0,
        )
        .await;

        let cfg = test_config(&target);
        let shutdown = CancellationToken::new();
        let session_shutdown = shutdown.clone();
        let task_cfg = cfg.clone();
        let session_task = tokio::spawn(async move {
            run_session_with_builder(
                session_shutdown,
                &task_cfg,
                Arc::new(FakeCommandResultBuilder),
            )
            .await
        });

        let hello = timeout(Duration::from_secs(2), hello_rx)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(hello.node_id, cfg.worker_id);
        assert_eq!(hello.worker_secret, cfg.worker_secret);
        assert_eq!(hello.executor_kind, "boxlite");
        let terminal_capacity = hello
            .terminal_session_capacity
            .as_ref()
            .expect("terminal capacity declaration");
        assert_eq!(terminal_capacity.max_active_sessions, 0);
        assert_eq!(terminal_capacity.active_session_count, 0);

        let capabilities = hello
            .capabilities
            .iter()
            .map(|capability| (capability.name.clone(), capability.max_inflight))
            .collect::<BTreeSet<_>>();
        assert_eq!(
            capabilities,
            BTreeSet::from([
                ("echo".to_owned(), 4),
                ("pythonExec".to_owned(), 4),
                ("terminalExec".to_owned(), 4),
                ("terminalLeaseRenew".to_owned(), 4),
                ("terminalResource".to_owned(), 4),
            ])
        );

        let command_result = timeout(Duration::from_secs(2), command_result_rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(command_result.command_id, "cmd-1");
        assert!(command_result.error.is_none());
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&command_result.payload_json).unwrap(),
            serde_json::json!({"message":"hello"})
        );

        let heartbeat = timeout(Duration::from_secs(2), heartbeat_rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(heartbeat.node_id, cfg.worker_id);
        assert_eq!(heartbeat.session_id, TEST_SESSION_ID);
        assert_eq!(heartbeat.active_session_count, 0);

        shutdown.cancel();
        let result = session_task.await.unwrap();
        assert_stopped_by_shutdown(result);

        server_shutdown.cancel();
        assert!(server_task.await.unwrap().is_ok());
    }

    #[tokio::test]
    async fn run_session_returns_results_for_multiple_dispatches() {
        let dispatches = (0..6)
            .map(|idx| CommandDispatch {
                command_id: format!("cmd-{idx}"),
                capability: "echo".to_owned(),
                payload_json: format!(r#"{{"message":"value-{idx}"}}"#).into_bytes(),
                deadline_unix_ms: 0,
            })
            .collect::<Vec<_>>();

        let TestRegistryHandle {
            target,
            hello_rx,
            mut command_result_rx,
            heartbeat_rx: _,
            shutdown: server_shutdown,
            server_task,
            persistence_report_rx: _,
        } = spawn_fake_registry(dispatches, false, 0).await;

        let mut cfg = test_config(&target);
        cfg.heartbeat_interval = Duration::from_millis(250);
        let shutdown = CancellationToken::new();
        let session_shutdown = shutdown.clone();
        let task_cfg = cfg;
        let session_task = tokio::spawn(async move {
            run_session_with_builder(
                session_shutdown,
                &task_cfg,
                Arc::new(FakeCommandResultBuilder),
            )
            .await
        });

        let _hello = timeout(Duration::from_secs(2), hello_rx)
            .await
            .unwrap()
            .unwrap();

        let mut command_ids = BTreeSet::new();
        for _ in 0..6 {
            let result = timeout(Duration::from_secs(2), command_result_rx.recv())
                .await
                .unwrap()
                .unwrap();
            assert!(result.error.is_none());
            command_ids.insert(result.command_id);
        }

        assert_eq!(
            command_ids,
            BTreeSet::from([
                "cmd-0".to_owned(),
                "cmd-1".to_owned(),
                "cmd-2".to_owned(),
                "cmd-3".to_owned(),
                "cmd-4".to_owned(),
                "cmd-5".to_owned(),
            ])
        );

        shutdown.cancel();
        let result = session_task.await.unwrap();
        assert_stopped_by_shutdown(result);

        server_shutdown.cancel();
        assert!(server_task.await.unwrap().is_ok());
    }

    #[tokio::test]
    async fn run_session_handles_python_and_terminal_capabilities_over_stream() {
        let TestRegistryHandle {
            target,
            hello_rx,
            mut command_result_rx,
            heartbeat_rx: _,
            shutdown: server_shutdown,
            server_task,
            persistence_report_rx: _,
        } = spawn_fake_registry(
            vec![
                CommandDispatch {
                    command_id: "cmd-py".to_owned(),
                    capability: "pythonExec".to_owned(),
                    payload_json: br#"{"code":"print('hi')"}"#.to_vec(),
                    deadline_unix_ms: 0,
                },
                CommandDispatch {
                    command_id: "cmd-term".to_owned(),
                    capability: "terminalExec".to_owned(),
                    payload_json: br#"{"command":"pwd"}"#.to_vec(),
                    deadline_unix_ms: 0,
                },
                CommandDispatch {
                    command_id: "cmd-res".to_owned(),
                    capability: "terminalResource".to_owned(),
                    payload_json: br#"{"session_id":"sess-term-1","file_path":"/tmp/app.txt","action":"read"}"#.to_vec(),
                    deadline_unix_ms: 0,
                },
            ],
            false,
            0,
        )
        .await;

        let cfg = test_config(&target);
        let shutdown = CancellationToken::new();
        let session_shutdown = shutdown.clone();
        let task_cfg = cfg;
        let session_task = tokio::spawn(async move {
            run_session_with_builder(
                session_shutdown,
                &task_cfg,
                Arc::new(FakeCommandResultBuilder),
            )
            .await
        });

        let _hello = timeout(Duration::from_secs(2), hello_rx)
            .await
            .unwrap()
            .unwrap();

        let mut payloads = BTreeMap::new();
        for _ in 0..3 {
            let result = timeout(Duration::from_secs(2), command_result_rx.recv())
                .await
                .unwrap()
                .unwrap();
            assert!(result.error.is_none());
            payloads.insert(
                result.command_id,
                serde_json::from_slice::<serde_json::Value>(&result.payload_json).unwrap(),
            );
        }

        assert_eq!(
            payloads.get("cmd-py"),
            Some(&serde_json::json!({
                "output": "hello from python\n",
                "stderr": "",
                "exit_code": 0
            }))
        );
        assert_eq!(
            payloads.get("cmd-term"),
            Some(&serde_json::json!({
                "session_id": "sess-term-1",
                "created": true,
                "stdout": "pwd\n",
                "stderr": "",
                "exit_code": 0,
                "stdout_truncated": false,
                "stderr_truncated": false,
                "lease_expires_unix_ms": 123456789
            }))
        );
        assert_eq!(
            payloads.get("cmd-res"),
            Some(&serde_json::json!({
                "session_id": "sess-term-1",
                "file_path": "/tmp/app.txt",
                "mime_type": "text/plain",
                "size_bytes": 3,
                "blob": "YWJj"
            }))
        );

        shutdown.cancel();
        let result = session_task.await.unwrap();
        assert_stopped_by_shutdown(result);

        server_shutdown.cancel();
        assert!(server_task.await.unwrap().is_ok());
    }

    #[tokio::test]
    async fn run_session_fails_after_two_heartbeat_ack_timeouts() {
        let TestRegistryHandle {
            target,
            hello_rx,
            command_result_rx: _,
            mut heartbeat_rx,
            shutdown: server_shutdown,
            server_task,
            persistence_report_rx: _,
        } = spawn_fake_registry(Vec::new(), false, 0).await;

        let cfg = test_config(&target);
        let shutdown = CancellationToken::new();
        let session_task = {
            let task_cfg = cfg.clone();
            let session_shutdown = shutdown.clone();
            tokio::spawn(async move {
                run_session_with_builder(
                    session_shutdown,
                    &task_cfg,
                    Arc::new(FakeCommandResultBuilder),
                )
                .await
            })
        };

        let _hello = timeout(Duration::from_secs(2), hello_rx)
            .await
            .unwrap()
            .unwrap();
        for _ in 0..2 {
            let heartbeat = timeout(Duration::from_secs(2), heartbeat_rx.recv())
                .await
                .unwrap()
                .unwrap();
            assert_eq!(heartbeat.session_id, TEST_SESSION_ID);
        }

        let result = timeout(Duration::from_secs(2), session_task)
            .await
            .unwrap()
            .unwrap();
        match result {
            Err(RunnerError::Message(message)) => {
                assert_eq!(message, "heartbeat ack deadline exceeded");
            }
            other => panic!("unexpected session result: {other:?}"),
        }

        shutdown.cancel();
        server_shutdown.cancel();
        assert!(server_task.await.unwrap().is_ok());
    }
    struct LifecycleBuilder {
        dispatcher: Arc<super::super::snapshot_control::ControlDispatcher>,
    }
    #[async_trait]
    impl CommandResultBuilder for LifecycleBuilder {
        fn persistence_dispatcher(
            &self,
        ) -> Option<Arc<super::super::snapshot_control::ControlDispatcher>> {
            Some(self.dispatcher.clone())
        }
        async fn recover(
            &self,
            cfg: &Config,
            candidates: &[crate::proto::registryv1::TerminalSessionRecoveryCandidate],
        ) -> Vec<crate::proto::registryv1::TerminalSessionRecoveryResult> {
            FakeCommandResultBuilder.recover(cfg, candidates).await
        }
        async fn build(
            &self,
            cfg: &Config,
            dispatch: CommandDispatch,
            connection: &str,
        ) -> ConnectRequest {
            FakeCommandResultBuilder
                .build(cfg, dispatch, connection)
                .await
        }
    }
    #[derive(Default)]
    struct PausedLifecycleEngine {
        entered: tokio::sync::Notify,
        release: tokio::sync::Notify,
        calls: std::sync::atomic::AtomicU32,
    }
    #[async_trait]
    impl super::super::snapshot_control::LifecycleEngine for PausedLifecycleEngine {
        async fn execute(
            &self,
            action: super::super::snapshot_control::ActionContext,
        ) -> Result<
            super::super::snapshot_control::ResultPayload,
            super::super::snapshot_control::ControlError,
        > {
            assert_eq!(action.connection, TEST_SESSION_ID);
            assert_eq!(action.frame.operation_id, "restore-operation");
            self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            self.entered.notify_one();
            self.release.notified().await;
            assert!(action.authorized());
            Ok(super::super::snapshot_control::ResultPayload {
                state: "prepared".into(),
                ..Default::default()
            })
        }
        fn fail_closed(&self) {
            panic!("transport fixture unexpectedly failed");
        }
    }
    fn lifecycle_frame() -> test_registryv1::SandboxPersistenceControl {
        test_registryv1::SandboxPersistenceControl {
            request_id: "lifecycle-request".into(),
            operation_id: "restore-operation".into(),
            scoped_session_id: "obx:owner:session".into(),
            generation: 2,
            action: test_registryv1::sandbox_persistence_control::Action::Restore as i32,
            deadline_unix_ms: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_millis() as i64
                + 60_000,
            payload_json: b"{}".to_vec(),
        }
    }
    #[tokio::test]
    async fn lifecycle_actions_are_multiplexed_without_blocking_heartbeats() {
        let mut server =
            spawn_fake_registry_with_controls(Vec::new(), true, 0, vec![lifecycle_frame()]).await;
        let mut cfg = test_config(&server.target);
        cfg.call_timeout = Duration::from_secs(2);
        let engine = Arc::new(PausedLifecycleEngine::default());
        let dispatcher =
            super::super::snapshot_control::ControlDispatcher::new(engine.clone(), 1).unwrap();
        let shutdown = CancellationToken::new();
        let session = tokio::spawn({
            let shutdown = shutdown.clone();
            let dispatcher = dispatcher.clone();
            async move {
                run_session_with_builder(shutdown, &cfg, Arc::new(LifecycleBuilder { dispatcher }))
                    .await
            }
        });
        timeout(Duration::from_secs(3), engine.entered.notified())
            .await
            .unwrap();
        for _ in 0..2 {
            let heartbeat = timeout(Duration::from_secs(2), server.heartbeat_rx.recv())
                .await
                .unwrap()
                .unwrap();
            assert_eq!(heartbeat.session_id, TEST_SESSION_ID);
        }
        assert!(server.persistence_report_rx.try_recv().is_err());
        engine.release.notify_one();
        let report = timeout(Duration::from_secs(2), server.persistence_report_rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert!(report.completed && report.error.is_none());
        assert_eq!(
            (
                report.request_id.as_str(),
                report.operation_id.as_str(),
                report.generation
            ),
            ("lifecycle-request", "restore-operation", 2)
        );
        assert_eq!(report.phase, "prepared");
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&report.payload_json).unwrap()["state"],
            "prepared"
        );
        shutdown.cancel();
        assert_stopped_by_shutdown(session.await.unwrap());
        dispatcher.close().await.unwrap();
        server.shutdown.cancel();
        assert!(server.server_task.await.unwrap().is_ok());
    }
    #[tokio::test]
    async fn lifecycle_frames_without_a_registered_engine_report_unavailable() {
        let mut server =
            spawn_fake_registry_with_controls(Vec::new(), true, 0, vec![lifecycle_frame()]).await;
        let mut cfg = test_config(&server.target);
        cfg.call_timeout = Duration::from_secs(2);
        let shutdown = CancellationToken::new();
        let session = tokio::spawn({
            let shutdown = shutdown.clone();
            async move {
                run_session_with_builder(shutdown, &cfg, Arc::new(FakeCommandResultBuilder)).await
            }
        });
        let report = timeout(Duration::from_secs(3), server.persistence_report_rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(report.error.unwrap().code, "persistence_unavailable");
        assert!(timeout(Duration::from_secs(2), server.heartbeat_rx.recv())
            .await
            .unwrap()
            .is_some());
        shutdown.cancel();
        assert_stopped_by_shutdown(session.await.unwrap());
        server.shutdown.cancel();
        assert!(server.server_task.await.unwrap().is_ok());
    }
    #[tokio::test]
    async fn report_capacity_is_reserved_before_starting_more_lifecycle_work() {
        let engine = Arc::new(PausedLifecycleEngine::default());
        let dispatcher =
            super::super::snapshot_control::ControlDispatcher::new(engine.clone(), 2).unwrap();
        dispatcher.begin_connection(TEST_SESSION_ID).await.unwrap();
        dispatcher.finish_connection(TEST_SESSION_ID).unwrap();
        let (outbound, mut reports) = mpsc::channel(4);
        let slots = Arc::new(tokio::sync::Semaphore::new(1));
        let shutdown = CancellationToken::new();
        let source = lifecycle_frame();
        let control = crate::proto::registryv1::SandboxPersistenceControl {
            request_id: source.request_id,
            operation_id: source.operation_id,
            scoped_session_id: source.scoped_session_id,
            generation: source.generation,
            action: source.action,
            deadline_unix_ms: source.deadline_unix_ms,
            payload_json: source.payload_json,
        };
        dispatch_lifecycle_control(
            &dispatcher,
            TEST_SESSION_ID,
            control.clone(),
            outbound.clone(),
            shutdown.clone(),
            slots.clone(),
        )
        .unwrap();
        timeout(Duration::from_secs(2), engine.entered.notified())
            .await
            .unwrap();
        let mut next = control;
        next.scoped_session_id = "obx:owner:other".into();
        assert!(dispatch_lifecycle_control(
            &dispatcher,
            TEST_SESSION_ID,
            next,
            outbound,
            shutdown,
            slots
        )
        .is_err());
        tokio::task::yield_now().await;
        assert_eq!(engine.calls.load(std::sync::atomic::Ordering::SeqCst), 1);
        engine.release.notify_one();
        assert!(timeout(Duration::from_secs(2), reports.recv())
            .await
            .unwrap()
            .is_some());
        dispatcher.close().await.unwrap();
    }
    #[tokio::test]
    async fn heartbeat_backpressure_preserves_shutdown_and_session_error_handling() {
        for signal in ["shutdown", "session_error"] {
            let (outbound, _pending) = mpsc::channel(1);
            outbound
                .send(ConnectRequest { payload: None })
                .await
                .unwrap();
            let (errors, mut error_rx) = mpsc::channel(1);
            let stopped = CancellationToken::new();
            let blocked = enqueue_heartbeat(
                &stopped,
                &outbound,
                &mut error_rx,
                ConnectRequest { payload: None },
            );
            tokio::pin!(blocked);
            assert!(timeout(Duration::from_millis(30), &mut blocked)
                .await
                .is_err());
            if signal == "shutdown" {
                stopped.cancel();
            } else {
                errors
                    .send(RunnerError::Message("bounded report queue is full".into()))
                    .await
                    .unwrap();
            }
            let result = timeout(Duration::from_secs(2), blocked).await.unwrap();
            match result {
                Err(RunnerError::Cancelled) => assert_eq!(signal, "shutdown"),
                Err(RunnerError::Message(message)) => {
                    assert_eq!(signal, "session_error");
                    assert_eq!(message, "bounded report queue is full");
                }
                other => panic!("unexpected heartbeat enqueue result: {other:?}"),
            }
        }
    }
}
