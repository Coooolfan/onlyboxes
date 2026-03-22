mod boxlite_runtime;
mod buildinfo;
mod config;
mod logging;
mod proto;
mod runner;

use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() {
    let cfg = config::Config::load();
    logging::configure(&cfg.log_level, &cfg.log_format, cfg.log_add_source);

    let shutdown = CancellationToken::new();
    let shutdown_watcher = tokio::spawn(wait_for_shutdown_signal(shutdown.clone()));

    let result = runner::run(shutdown.clone(), cfg).await;
    shutdown.cancel();
    shutdown_watcher.abort();
    runner::shutdown().await;
    boxlite_runtime::shutdown().await;

    match result {
        Ok(()) | Err(runner::RunnerError::Cancelled) => {
            tracing::info!("worker stopped");
        }
        Err(err) => {
            tracing::error!(error = %err, "worker stopped with error");
            std::process::exit(1);
        }
    }
}

async fn wait_for_shutdown_signal(shutdown: CancellationToken) {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};

        let mut sigint = signal(SignalKind::interrupt()).ok();
        let mut sigterm = signal(SignalKind::terminate()).ok();

        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = async {
                if let Some(ref mut stream) = sigint {
                    stream.recv().await;
                } else {
                    std::future::pending::<()>().await;
                }
            } => {}
            _ = async {
                if let Some(ref mut stream) = sigterm {
                    stream.recv().await;
                } else {
                    std::future::pending::<()>().await;
                }
            } => {}
        }
    }

    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }

    shutdown.cancel();
}
