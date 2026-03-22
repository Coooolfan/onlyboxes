use std::sync::Arc;
use std::time::Duration;

use tokio::sync::{mpsc, watch};
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};

use crate::capability::CapabilityExecutor;
use crate::config::Config;
use crate::proto;
use crate::proto::worker_registry_service_client::WorkerRegistryServiceClient;

const OUTBOUND_BUFFER: usize = 64;
const MIN_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(1);
const INITIAL_RECONNECT_DELAY: Duration = Duration::from_secs(1);
const MAX_RECONNECT_DELAY: Duration = Duration::from_secs(15);

pub struct SessionClient {
    cfg: Config,
    session_id: Option<String>,
}

impl SessionClient {
    pub fn new(cfg: Config) -> Self {
        Self {
            cfg,
            session_id: None,
        }
    }

    pub async fn run(
        &mut self,
        executor: Arc<CapabilityExecutor>,
        mut shutdown: watch::Receiver<bool>,
    ) {
        let mut reconnect_delay = INITIAL_RECONNECT_DELAY;

        loop {
            tokio::select! {
                result = self.run_session(&executor) => {
                    match result {
                        Ok(()) => {
                            tracing::info!("session ended normally");
                            return;
                        }
                        Err(e) => {
                            // Check if we should shut down
                            if *shutdown.borrow() {
                                tracing::info!("shutdown requested, stopping reconnect");
                                return;
                            }

                            // Reset delay on FailedPrecondition (session replaced)
                            if let Some(status) = e.downcast_ref::<tonic::Status>() {
                                if status.code() == tonic::Code::FailedPrecondition {
                                    reconnect_delay = INITIAL_RECONNECT_DELAY;
                                }
                            }

                            tracing::warn!(
                                error = %e,
                                delay_ms = reconnect_delay.as_millis(),
                                "session disconnected, reconnecting"
                            );

                            tokio::select! {
                                _ = tokio::time::sleep(reconnect_delay) => {}
                                _ = shutdown.changed() => {
                                    tracing::info!("shutdown during reconnect backoff");
                                    return;
                                }
                            }

                            reconnect_delay = (reconnect_delay * 2).min(MAX_RECONNECT_DELAY);
                        }
                    }
                }
                _ = shutdown.changed() => {
                    tracing::info!("shutdown requested");
                    return;
                }
            }
        }
    }

    async fn run_session(
        &mut self,
        executor: &Arc<CapabilityExecutor>,
    ) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        // 1. Build channel
        let channel = self.build_channel().await?;
        let mut client = WorkerRegistryServiceClient::new(channel);

        // 2. Setup bidirectional stream
        let (outbound_tx, outbound_rx) = mpsc::channel::<proto::ConnectRequest>(OUTBOUND_BUFFER);
        let request_stream = ReceiverStream::new(outbound_rx);
        let response = client.connect(request_stream).await?;
        let mut inbound = response.into_inner();

        // 3. Send Hello
        outbound_tx.send(self.build_hello()).await?;

        // 4. Wait for ConnectAck
        let ack = tokio::time::timeout(self.cfg.call_timeout, inbound.message())
            .await
            .map_err(|_| "timeout waiting for ConnectAck")?
            .map_err(|e| format!("stream error waiting for ack: {e}"))?
            .ok_or("stream closed before ConnectAck")?;

        let (session_id, heartbeat_interval) = match ack.payload {
            Some(proto::connect_response::Payload::ConnectAck(ack)) => {
                let interval = if ack.heartbeat_interval_sec > 0 {
                    Duration::from_secs(ack.heartbeat_interval_sec as u64)
                } else {
                    self.cfg.heartbeat_interval
                };
                (ack.session_id, interval)
            }
            _ => return Err("expected ConnectAck".into()),
        };

        if session_id.is_empty() {
            return Err("ConnectAck session_id is empty".into());
        }

        self.session_id = Some(session_id.clone());
        tracing::info!(
            node_id = %self.cfg.worker_id,
            node_name = %self.cfg.node_name,
            session_id = %session_id,
            "connected to console"
        );

        // 5. Concurrent loops: heartbeat + receive
        let (hb_ack_tx, hb_ack_rx) = mpsc::channel::<proto::HeartbeatAck>(4);
        let (session_err_tx, mut session_err_rx) =
            mpsc::channel::<Box<dyn std::error::Error + Send + Sync>>(2);

        // Heartbeat sender
        let hb_outbound = outbound_tx.clone();
        let hb_err_tx = session_err_tx.clone();
        let hb_cfg = self.cfg.clone();
        let hb_session_id = session_id.clone();
        let hb_node_id = self.cfg.worker_id.clone();
        let heartbeat_handle = tokio::spawn(async move {
            if let Err(e) = heartbeat_loop(
                hb_outbound,
                hb_ack_rx,
                hb_cfg,
                heartbeat_interval,
                hb_node_id,
                hb_session_id,
            )
            .await
            {
                let _ = hb_err_tx.send(e).await;
            }
        });

        // Receive loop
        let recv_executor = executor.clone();
        let recv_outbound = outbound_tx.clone();
        let recv_err_tx = session_err_tx;
        let receive_handle = tokio::spawn(async move {
            if let Err(e) =
                receive_loop(inbound, recv_outbound, hb_ack_tx, recv_executor).await
            {
                let _ = recv_err_tx.send(e).await;
            }
        });

        // Wait for any error
        let err = session_err_rx.recv().await;
        heartbeat_handle.abort();
        receive_handle.abort();

        match err {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }

    async fn build_channel(&self) -> Result<Channel, tonic::transport::Error> {
        let target = &self.cfg.console_grpc_target;
        let scheme = if self.cfg.console_tls { "https" } else { "http" };
        let uri = format!("{scheme}://{target}");

        let mut endpoint = Endpoint::from_shared(uri)?;

        if self.cfg.console_tls {
            endpoint = endpoint.tls_config(ClientTlsConfig::new())?;
        }

        endpoint.connect().await
    }

    fn build_hello(&self) -> proto::ConnectRequest {
        proto::ConnectRequest {
            payload: Some(proto::connect_request::Payload::Hello(
                proto::ConnectHello {
                    node_id: self.cfg.worker_id.clone(),
                    node_name: self.cfg.node_name.clone(),
                    executor_kind: "boxlite".to_string(),
                    labels: self.cfg.labels.clone(),
                    version: self.cfg.version.clone(),
                    worker_secret: self.cfg.worker_secret.clone(),
                    capabilities: vec![
                        proto::CapabilityDeclaration {
                            name: "echo".into(),
                            max_inflight: 4,
                        },
                        proto::CapabilityDeclaration {
                            name: "pythonExec".into(),
                            max_inflight: 4,
                        },
                        proto::CapabilityDeclaration {
                            name: "terminalExec".into(),
                            max_inflight: 4,
                        },
                        proto::CapabilityDeclaration {
                            name: "terminalResource".into(),
                            max_inflight: 4,
                        },
                    ],
                },
            )),
        }
    }
}

async fn heartbeat_loop(
    outbound: mpsc::Sender<proto::ConnectRequest>,
    mut ack_rx: mpsc::Receiver<proto::HeartbeatAck>,
    cfg: Config,
    mut interval: Duration,
    node_id: String,
    session_id: String,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut consecutive_timeouts: u32 = 0;

    loop {
        let jittered = jitter_duration(interval, cfg.heartbeat_jitter_pct);
        tokio::time::sleep(jittered).await;

        // Send heartbeat
        let frame = proto::ConnectRequest {
            payload: Some(proto::connect_request::Payload::Heartbeat(
                proto::HeartbeatFrame {
                    node_id: node_id.clone(),
                    session_id: session_id.clone(),
                },
            )),
        };
        outbound.send(frame).await?;

        // Wait for ack
        match tokio::time::timeout(cfg.call_timeout, ack_rx.recv()).await {
            Ok(Some(ack)) => {
                consecutive_timeouts = 0;
                if ack.heartbeat_interval_sec > 0 {
                    interval = Duration::from_secs(ack.heartbeat_interval_sec as u64);
                }
            }
            Ok(None) => {
                return Err("heartbeat ack channel closed".into());
            }
            Err(_) => {
                consecutive_timeouts += 1;
                if consecutive_timeouts >= 2 {
                    return Err("heartbeat ack timeout (2 consecutive)".into());
                }
                tracing::warn!("heartbeat ack timeout (tolerating 1)");
            }
        }
    }
}

fn jitter_duration(base: Duration, jitter_pct: u32) -> Duration {
    use rand::Rng;
    let nanos = base.as_nanos() as i128;
    let max_delta = nanos * jitter_pct as i128 / 100;
    if max_delta == 0 {
        return base;
    }
    let delta = rand::thread_rng().gen_range(-max_delta..=max_delta);
    let result = (nanos + delta).max(MIN_HEARTBEAT_INTERVAL.as_nanos() as i128);
    Duration::from_nanos(result as u64)
}

async fn receive_loop(
    mut inbound: tonic::Streaming<proto::ConnectResponse>,
    outbound: mpsc::Sender<proto::ConnectRequest>,
    hb_ack_tx: mpsc::Sender<proto::HeartbeatAck>,
    executor: Arc<CapabilityExecutor>,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    loop {
        let msg = inbound
            .message()
            .await?
            .ok_or("server closed the stream")?;

        match msg.payload {
            Some(proto::connect_response::Payload::HeartbeatAck(ack)) => {
                let _ = hb_ack_tx.send(ack).await;
            }
            Some(proto::connect_response::Payload::CommandDispatch(dispatch)) => {
                let exec = executor.clone();
                let tx = outbound.clone();
                tokio::spawn(async move {
                    let command_id = dispatch.command_id.clone();
                    let capability = dispatch.capability.clone();
                    tracing::debug!(command_id = %command_id, capability = %capability, "dispatching command");

                    let result = exec.execute(dispatch).await;
                    if let Err(e) = tx
                        .send(proto::ConnectRequest {
                            payload: Some(
                                proto::connect_request::Payload::CommandResult(result),
                            ),
                        })
                        .await
                    {
                        tracing::error!(command_id = %command_id, error = %e, "failed to send result");
                    }
                });
            }
            _ => {
                return Err("unexpected message type from server".into());
            }
        }
    }
}
