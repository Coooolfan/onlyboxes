use std::collections::HashMap;

use crate::config::Config;
use crate::proto::registryv1::{CapabilityDeclaration, ConnectHello};

use super::{
    ECHO_CAPABILITY_NAME, PYTHON_EXEC_CAPABILITY_DECLARED, TERMINAL_EXEC_CAPABILITY_DECLARED,
    TERMINAL_RESOURCE_CAPABILITY_DECLARED,
};

pub(crate) fn build_hello(cfg: &Config) -> ConnectHello {
    let node_name = if cfg.node_name.trim().is_empty() {
        let suffix = cfg.worker_id.chars().take(8).collect::<String>();
        format!("worker-boxlite-{suffix}")
    } else {
        cfg.node_name.trim().to_owned()
    };

    let mut labels = HashMap::with_capacity(cfg.labels.len());
    for (key, value) in &cfg.labels {
        labels.insert(key.clone(), value.clone());
    }

    ConnectHello {
        node_id: cfg.worker_id.clone(),
        node_name,
        executor_kind: cfg.executor_kind.clone(),
        labels,
        version: cfg.version.clone(),
        capabilities: vec![
            CapabilityDeclaration {
                name: ECHO_CAPABILITY_NAME.to_owned(),
                max_inflight: cfg.echo_max_inflight,
            },
            CapabilityDeclaration {
                name: PYTHON_EXEC_CAPABILITY_DECLARED.to_owned(),
                max_inflight: cfg.python_exec_max_inflight,
            },
            CapabilityDeclaration {
                name: TERMINAL_EXEC_CAPABILITY_DECLARED.to_owned(),
                max_inflight: cfg.terminal_exec_max_inflight,
            },
            CapabilityDeclaration {
                name: TERMINAL_RESOURCE_CAPABILITY_DECLARED.to_owned(),
                max_inflight: cfg.terminal_resource_max_inflight,
            },
        ],
        worker_secret: cfg.worker_secret.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use std::time::Duration;

    fn test_config() -> Config {
        Config {
            console_grpc_target: "127.0.0.1:50051".to_owned(),
            console_tls: false,
            worker_id: "worker-12345678".to_owned(),
            worker_secret: "secret".to_owned(),
            heartbeat_interval: Duration::from_secs(5),
            heartbeat_jitter_pct: 20,
            call_timeout: Duration::from_secs(13),
            node_name: String::new(),
            executor_kind: "boxlite".to_owned(),
            version: "dev".to_owned(),
            labels: Default::default(),
            boxlite_home: String::new(),
            python_exec_image: "ghcr.io/astral-sh/uv:python3.12-bookworm-slim".to_owned(),
            python_exec_memory_mib: 256,
            python_exec_cpus: 1,
            python_exec_max_processes: 128,
            terminal_exec_image: "coolfan1024/onlyboxes-default-worker:0.0.5".to_owned(),
            terminal_exec_memory_mib: 256,
            terminal_exec_cpus: 1,
            terminal_exec_max_processes: 128,
            terminal_lease_min_sec: 60,
            terminal_lease_max_sec: 1800,
            terminal_lease_default_sec: 60,
            terminal_output_limit_bytes: 1024 * 1024,
            terminal_export_max_bytes: 0,
            echo_max_inflight: 4,
            python_exec_max_inflight: 4,
            terminal_exec_max_inflight: 4,
            terminal_resource_max_inflight: 4,
            log_level: "info".to_owned(),
            log_format: "json".to_owned(),
            log_add_source: false,
        }
    }

    #[test]
    fn build_hello_contains_expected_capabilities() {
        let hello = build_hello(&test_config());
        assert_eq!(hello.node_name, "worker-boxlite-worker-1");
        assert_eq!(hello.capabilities.len(), 4);
        assert_eq!(hello.capabilities[0].name, "echo");
        assert_eq!(hello.capabilities[1].name, "pythonExec");
        assert_eq!(hello.capabilities[2].name, "terminalExec");
        assert_eq!(hello.capabilities[3].name, "terminalResource");
    }
}
