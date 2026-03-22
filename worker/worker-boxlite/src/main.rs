mod boxlite_runtime;
mod capability;
mod config;
mod proto;
mod session_client;

use std::sync::Arc;
use tracing_subscriber::EnvFilter;

fn main() {
    let cfg = config::Config::load();

    // Initialize tracing
    let filter = EnvFilter::try_new(&cfg.log_level).unwrap_or_else(|_| EnvFilter::new("info"));
    match cfg.log_format.as_str() {
        "json" => {
            tracing_subscriber::fmt()
                .json()
                .with_env_filter(filter)
                .init();
        }
        _ => {
            tracing_subscriber::fmt().with_env_filter(filter).init();
        }
    }

    tracing::info!(
        worker_id = %cfg.worker_id,
        node_name = %cfg.node_name,
        version = %cfg.version,
        target = %cfg.console_grpc_target,
        tls = cfg.console_tls,
        python_exec_image = %cfg.python_exec_image,
        terminal_exec_image = %cfg.terminal_exec_image,
        lease_min = cfg.terminal_lease_min_sec,
        lease_max = cfg.terminal_lease_max_sec,
        lease_default = cfg.terminal_lease_default_sec,
        output_limit = cfg.terminal_output_limit_bytes,
        "starting worker-boxlite"
    );

    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("failed to build tokio runtime");

    rt.block_on(async {
        // Shutdown signal
        let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);

        tokio::spawn(async move {
            let mut sigint =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt())
                    .expect("failed to register SIGINT");
            let mut sigterm =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                    .expect("failed to register SIGTERM");
            tokio::select! {
                _ = sigint.recv() => tracing::info!("received SIGINT"),
                _ = sigterm.recv() => tracing::info!("received SIGTERM"),
            }
            let _ = shutdown_tx.send(true);
        });

        let runtime = boxlite_runtime::init_runtime();
        tracing::info!("boxlite runtime initialized");

        let executor = Arc::new(capability::CapabilityExecutor::new(
            runtime,
            cfg.clone(),
            shutdown_rx.clone(),
        ));
        let mut client = session_client::SessionClient::new(cfg);
        client.run(executor.clone(), shutdown_rx).await;

        // Graceful shutdown: clean up all sessions
        executor.shutdown().await;
        tracing::info!("worker-boxlite shut down");
    });
}
