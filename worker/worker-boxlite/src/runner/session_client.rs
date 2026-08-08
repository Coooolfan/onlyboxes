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
    shared_active_session_count, shared_recover_terminal_sessions,
};
use super::{
    build_command_result, build_hello, command_dispatch_summary_for_log, duration_from_server,
    RunnerError,
};

pub(crate) async fn run_session(
    shutdown: CancellationToken,
    cfg: &Config,
) -> Result<(), RunnerError> {
    run_session_with_builder(shutdown, cfg, Arc::new(DefaultCommandResultBuilder)).await
}

async fn run_session_with_builder(
    shutdown: CancellationToken,
    cfg: &Config,
    command_result_builder: Arc<dyn CommandResultBuilder>,
) -> Result<(), RunnerError> {
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

    let recovery_results = tokio::select! {
        _ = shutdown.cancelled() => return Err(RunnerError::Message("worker shutdown during terminal session recovery".to_owned())),
        result = tokio::time::timeout(
            cfg.call_timeout,
            shared_recover_terminal_sessions(cfg, &ack.terminal_session_recovery_candidates),
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
        shutdown.clone(),
        inbound,
        outbound_tx.clone(),
        heartbeat_ack_tx,
        session_err_tx,
        cfg.clone(),
        command_result_builder,
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
    async fn build(&self, cfg: &Config, dispatch: CommandDispatch) -> ConnectRequest;
}

struct DefaultCommandResultBuilder;

#[async_trait]
impl CommandResultBuilder for DefaultCommandResultBuilder {
    async fn build(&self, cfg: &Config, dispatch: CommandDispatch) -> ConnectRequest {
        build_command_result(cfg, dispatch).await
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

async fn receiver_loop(
    shutdown: CancellationToken,
    mut inbound: tonic::Streaming<ConnectResponse>,
    outbound_tx: mpsc::Sender<ConnectRequest>,
    heartbeat_ack_tx: mpsc::Sender<HeartbeatAck>,
    session_err_tx: mpsc::Sender<RunnerError>,
    cfg: Config,
    command_result_builder: Arc<dyn CommandResultBuilder>,
) {
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
                    );
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
        let result = command_result_builder.build(&cfg, dispatch).await;
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

        outbound_tx
            .send(ConnectRequest {
                payload: Some(connect_request::Payload::Heartbeat(HeartbeatFrame {
                    node_id: cfg.worker_id.clone(),
                    session_id: session_id.clone(),
                    active_session_count: shared_active_session_count().await,
                })),
            })
            .await
            .map_err(|_| RunnerError::Message("enqueue heartbeat failed".to_owned()))?;

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
        hello_tx: Mutex<Option<oneshot::Sender<ConnectHello>>>,
        command_result_tx: mpsc::UnboundedSender<CommandResult>,
        heartbeat_tx: mpsc::UnboundedSender<HeartbeatFrame>,
    }

    struct TestRegistryHandle {
        target: String,
        hello_rx: oneshot::Receiver<ConnectHello>,
        command_result_rx: mpsc::UnboundedReceiver<CommandResult>,
        heartbeat_rx: mpsc::UnboundedReceiver<HeartbeatFrame>,
        shutdown: CancellationToken,
        server_task: JoinHandle<Result<(), tonic::transport::Error>>,
    }

    #[async_trait]
    impl CommandResultBuilder for FakeCommandResultBuilder {
        async fn build(&self, _cfg: &Config, dispatch: CommandDispatch) -> ConnectRequest {
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
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let target = listener.local_addr().unwrap().to_string();
        let incoming = TcpListenerStream::new(listener);
        let shutdown = CancellationToken::new();
        let server_shutdown = shutdown.clone();

        let (hello_tx, hello_rx) = oneshot::channel();
        let (command_result_tx, command_result_rx) = mpsc::unbounded_channel();
        let (heartbeat_tx, heartbeat_rx) = mpsc::unbounded_channel();

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
        let session_task =
            tokio::spawn(async move { run_session(session_shutdown, &task_cfg).await });

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
        } = spawn_fake_registry(dispatches, false, 0).await;

        let mut cfg = test_config(&target);
        cfg.heartbeat_interval = Duration::from_millis(250);
        let shutdown = CancellationToken::new();
        let session_shutdown = shutdown.clone();
        let task_cfg = cfg;
        let session_task =
            tokio::spawn(async move { run_session(session_shutdown, &task_cfg).await });

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
        } = spawn_fake_registry(Vec::new(), false, 0).await;

        let cfg = test_config(&target);
        let shutdown = CancellationToken::new();
        let session_task = {
            let task_cfg = cfg.clone();
            let session_shutdown = shutdown.clone();
            tokio::spawn(async move { run_session(session_shutdown, &task_cfg).await })
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
}
