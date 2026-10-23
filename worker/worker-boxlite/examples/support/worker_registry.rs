//! Local registry transport for actual runner acceptance; no Console database.
pub mod proto {
    include!(concat!(
        env!("OUT_DIR"),
        "/test-server/onlyboxes.registry.v1.rs"
    ));
}
use proto::*;
use std::sync::Arc;
use tokio::sync::{mpsc, Mutex};
use tokio_stream::wrappers::{ReceiverStream, TcpListenerStream};
use tokio_util::sync::CancellationToken;
use tonic::{Request, Response, Status};
type Sender = mpsc::Sender<Result<ConnectResponse, Status>>;
#[derive(Clone)]
struct Service {
    responses: Arc<Mutex<Option<Sender>>>,
    events: mpsc::UnboundedSender<ConnectRequest>,
}
#[tonic::async_trait]
impl worker_registry_service_server::WorkerRegistryService for Service {
    type ConnectStream = ReceiverStream<Result<ConnectResponse, Status>>;
    async fn connect(
        &self,
        request: Request<tonic::Streaming<ConnectRequest>>,
    ) -> Result<Response<Self::ConnectStream>, Status> {
        let mut input = request.into_inner();
        let hello = input
            .message()
            .await?
            .ok_or_else(|| Status::invalid_argument("missing hello"))?;
        if !matches!(hello.payload, Some(connect_request::Payload::Hello(_))) {
            return Err(Status::invalid_argument("missing hello"));
        }
        let (tx, rx) = mpsc::channel(32);
        *self.responses.lock().await = Some(tx.clone());
        self.events
            .send(hello)
            .map_err(|_| Status::cancelled("fixture ended"))?;
        tx.send(Ok(ConnectResponse {
            payload: Some(connect_response::Payload::ConnectAck(ConnectAck {
                session_id: "native-worker-connection".into(),
                heartbeat_interval_sec: 1,
                terminal_session_recovery_candidates: vec![],
                sandbox_persistence_recovery_candidates: vec![],
            })),
        }))
        .await
        .map_err(|_| Status::cancelled("fixture ended"))?;
        let events = self.events.clone();
        tokio::spawn(async move {
            while let Ok(Some(frame)) = input.message().await {
                let response = match &frame.payload {
                    Some(connect_request::Payload::TerminalSessionRecoveryReport(_)) => {
                        Some(connect_response::Payload::TerminalSessionRecoveryAck(
                            TerminalSessionRecoveryAck {},
                        ))
                    }
                    Some(connect_request::Payload::Heartbeat(_)) => {
                        Some(connect_response::Payload::HeartbeatAck(HeartbeatAck {
                            heartbeat_interval_sec: 1,
                        }))
                    }
                    _ => None,
                };
                if let Some(payload) = response {
                    if tx
                        .send(Ok(ConnectResponse {
                            payload: Some(payload),
                        }))
                        .await
                        .is_err()
                    {
                        break;
                    }
                }
                if events.send(frame).is_err() {
                    break;
                }
            }
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }
}
pub struct Registry {
    pub target: String,
    pub events: mpsc::UnboundedReceiver<ConnectRequest>,
    responses: Arc<Mutex<Option<Sender>>>,
    shutdown: CancellationToken,
    task: tokio::task::JoinHandle<Result<(), tonic::transport::Error>>,
}
impl Registry {
    pub async fn start() -> Result<Self, Box<dyn std::error::Error>> {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
        let target = listener.local_addr()?.to_string();
        let (events, rx) = mpsc::unbounded_channel();
        let responses = Arc::new(Mutex::new(None));
        let service = Service {
            responses: responses.clone(),
            events,
        };
        let shutdown = CancellationToken::new();
        let signal = shutdown.clone();
        let task = tokio::spawn(
            tonic::transport::Server::builder()
                .add_service(
                    worker_registry_service_server::WorkerRegistryServiceServer::new(service),
                )
                .serve_with_incoming_shutdown(
                    TcpListenerStream::new(listener),
                    signal.cancelled_owned(),
                ),
        );
        Ok(Self {
            target,
            events: rx,
            responses,
            shutdown,
            task,
        })
    }
    pub async fn send(
        &self,
        payload: connect_response::Payload,
    ) -> Result<(), Box<dyn std::error::Error>> {
        let tx = self
            .responses
            .lock()
            .await
            .clone()
            .ok_or("Worker has not connected")?;
        tx.send(Ok(ConnectResponse {
            payload: Some(payload),
        }))
        .await?;
        Ok(())
    }
    pub async fn event(&mut self) -> Result<connect_request::Payload, Box<dyn std::error::Error>> {
        tokio::time::timeout(std::time::Duration::from_secs(180), self.events.recv())
            .await?
            .ok_or("Worker registry stream ended")?
            .payload
            .ok_or("empty Worker frame".into())
    }
    pub async fn close(self) {
        self.shutdown.cancel();
        self.task.abort();
        let _ = self.task.await;
    }
}
