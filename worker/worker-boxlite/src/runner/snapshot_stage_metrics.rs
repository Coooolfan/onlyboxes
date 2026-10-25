//! Awaited stage timings, independent of wire contracts and durable state.
//! Never serialize the future's input, output or error (all may carry secrets).
use std::{future::Future, time::Instant};

#[derive(Clone, Copy)]
pub(super) enum Stage {
    Export,
    UploadSourceVerify,
    UploadParts,
    RemoteVerify,
    Finalize,
    CacheCopy,
    Download,
    Import,
    Start,
}

impl Stage {
    fn label(self) -> &'static str {
        match self {
            Self::Export => "export",
            Self::UploadSourceVerify => "upload_source_verify",
            Self::UploadParts => "upload_parts",
            Self::RemoteVerify => "remote_verify",
            Self::Finalize => "finalize",
            Self::CacheCopy => "cache_copy",
            Self::Download => "download",
            Self::Import => "import",
            Self::Start => "start_and_readiness",
        }
    }
}

pub(super) async fn measure<T, E>(
    operation: &str,
    stage: Stage,
    work: impl Future<Output = Result<T, E>>,
) -> Result<T, E> {
    let attempt = uuid::Uuid::new_v4();
    let stage = stage.label();
    tracing::info!(operation_id = operation, attempt_id = %attempt, stage,
        "sandbox snapshot stage started");
    let started = Instant::now();
    let result = work.await;
    let elapsed_us = u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX);
    tracing::info!(operation_id = operation, attempt_id = %attempt, stage,
        elapsed_us, succeeded = result.is_ok(), "sandbox snapshot stage finished");
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        io::Write,
        sync::{Arc, Mutex},
    };
    use tracing::instrument::WithSubscriber;
    #[derive(Clone)]
    struct Capture(Arc<Mutex<Vec<u8>>>);
    impl Write for Capture {
        fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(bytes);
            Ok(bytes.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    fn capture() -> (Capture, impl tracing::Subscriber + Send + Sync) {
        let output = Capture(Arc::new(Mutex::new(Vec::new())));
        let writer = output.clone();
        let subscriber = tracing_subscriber::fmt()
            .json()
            .with_writer(move || writer.clone())
            .with_ansi(false)
            .finish();
        (output, subscriber)
    }
    fn fields(output: &Capture) -> Vec<serde_json::Value> {
        String::from_utf8(output.0.lock().unwrap().clone())
            .unwrap()
            .lines()
            .map(|line| serde_json::from_str::<serde_json::Value>(line).unwrap()["fields"].clone())
            .collect()
    }
    #[tokio::test]
    async fn completed_attempts_pair_by_id_without_serializing_values_or_errors() {
        let (output, subscriber) = capture();
        async {
            let value = measure("operation", Stage::Export, async {
                Ok::<_, &str>("private-result")
            })
            .await
            .unwrap();
            assert_eq!(value, "private-result");
            assert!(measure("operation", Stage::Import, async {
                Err::<(), _>("private-signed-url")
            })
            .await
            .is_err());
        }
        .with_subscriber(subscriber)
        .await;
        let records = fields(&output);
        assert_eq!(records.len(), 4);
        for pair in records.chunks_exact(2) {
            assert_eq!(pair[0]["message"], "sandbox snapshot stage started");
            assert_eq!(pair[1]["message"], "sandbox snapshot stage finished");
            assert_eq!(pair[0]["attempt_id"], pair[1]["attempt_id"]);
            assert_eq!(pair[0]["stage"], pair[1]["stage"]);
            assert_eq!(pair[0]["operation_id"], "operation");
            assert!(pair[1]["elapsed_us"].is_u64());
        }
        assert_eq!(records[1]["succeeded"], true);
        assert_eq!(records[3]["succeeded"], false);
        assert_ne!(records[0]["attempt_id"], records[2]["attempt_id"]);
        assert!(!String::from_utf8(output.0.lock().unwrap().clone())
            .unwrap()
            .contains("private-"));
    }
    #[tokio::test]
    async fn dropped_waiter_never_reports_a_completed_io_stage() {
        let (output, subscriber) = capture();
        let waiting = measure(
            "operation",
            Stage::Download,
            std::future::pending::<Result<(), ()>>(),
        );
        assert!(tokio::time::timeout(
            std::time::Duration::from_millis(10),
            waiting.with_subscriber(subscriber)
        )
        .await
        .is_err());
        let records = fields(&output);
        assert_eq!(records.len(), 1);
        assert_eq!(records[0]["message"], "sandbox snapshot stage started");
    }
}
