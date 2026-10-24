//! Private companion service. Worker registry/configuration is not loaded here.
use std::io::Write;
use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use tokio_util::sync::CancellationToken;
use worker_boxlite::persistence::{ExecutionLeaseLedger, LeaseGuardServer};

async fn run() -> Result<(), ()> {
    let mut state: Option<PathBuf> = None;
    let mut socket: Option<PathBuf> = None;
    let mut worker = None;
    let mut store = None;
    let mut args = std::env::args_os().skip(1);
    while let Some(flag) = args.next() {
        let value = args.next().ok_or(())?;
        match flag.to_str().ok_or(())? {
            "--state-dir" if state.is_none() => state = Some(value.into()),
            "--socket-dir" if socket.is_none() => socket = Some(value.into()),
            "--worker-id" if worker.is_none() => {
                worker = Some(value.into_string().map_err(|_| ())?)
            }
            "--store-id" if store.is_none() => store = Some(value.into_string().map_err(|_| ())?),
            _ => return Err(()),
        }
    }
    let state = state.ok_or(())?;
    let socket = socket.ok_or(())?;
    let worker = worker.ok_or(())?;
    let ledger = if let Some(store) = store {
        ExecutionLeaseLedger::open_bound(&state, &worker, &store)
    } else {
        // Existing stores must use the identity retained by their caller;
        // discovery of a replacement owner cannot silently authorize reopen.
        if std::fs::symlink_metadata(state.join("owner.json")).is_ok() {
            return Err(());
        }
        ExecutionLeaseLedger::open(&state, &worker)
    }
    .map_err(|_| ())?;
    let shutdown = CancellationToken::new();
    let signal = shutdown.clone();
    let signal_reason = Arc::new(AtomicUsize::new(0));
    let reason = signal_reason.clone();
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .map_err(|_| ())?;
    let watcher = tokio::spawn(async move {
        let received = tokio::select! {
            result=tokio::signal::ctrl_c()=>if result.is_ok() { 1 } else { 3 },
            result=term.recv()=>if result.is_some() { 2 } else { 3 }
        };
        reason.store(received, Ordering::SeqCst);
        signal.cancel();
    });
    let result = LeaseGuardServer::serve(Arc::new(ledger), socket, shutdown)
        .await
        .map_err(|_| ());
    watcher.abort();
    let _ = watcher.await;
    // Native enforcement has joined before any diagnostic output. Logging
    // failure cannot prevent signal cancellation or writer shutdown.
    let reason = signal_reason.load(Ordering::SeqCst);
    let message = match reason {
        1 => Some("BoxLite lease guard stopped: SIGINT"),
        2 => Some("BoxLite lease guard stopped: SIGTERM"),
        3 => Some("BoxLite lease guard failure: signal_watcher"),
        _ => None,
    };
    if let Some(message) = message {
        let _ = writeln!(std::io::stderr().lock(), "{message}");
    }
    if reason == 3 {
        Err(())
    } else {
        result
    }
}

#[tokio::main]
async fn main() {
    if run().await.is_err() {
        let _ = writeln!(
            std::io::stderr().lock(),
            "BoxLite lease guard is unavailable"
        );
        std::process::exit(1);
    }
}
