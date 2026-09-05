use std::collections::HashMap;

use crate::config::Config;
use crate::proto::registryv1::{CapabilityDeclaration, ConnectHello, TerminalSessionCapacity};

use super::{
    validate_terminal_max_active_sessions, RunnerError, ECHO_CAPABILITY_NAME,
    PYTHON_EXEC_CAPABILITY_DECLARED, TERMINAL_EXEC_CAPABILITY_DECLARED,
    TERMINAL_RESOURCE_CAPABILITY_DECLARED,
};

pub(crate) fn build_hello(
    cfg: &Config,
    active_session_count: i32,
) -> Result<ConnectHello, RunnerError> {
    validate_terminal_max_active_sessions(cfg.terminal_max_active_sessions)?;
    if active_session_count < 0 {
        return Err(RunnerError::Message(
            "terminal active session count must be non-negative".to_owned(),
        ));
    }

    let node_name = if cfg.node_name.trim().is_empty() {
        let suffix = cfg.worker_id.chars().take(8).collect::<String>();
        format!("worker-boxlite-{suffix}")
    } else {
        cfg.node_name.trim().to_owned()
    };

    let mut labels = HashMap::with_capacity(cfg.labels.len());
    for (key, value) in &cfg.labels {
        if key.trim() == super::PROXY_ENDPOINT_LABEL {
            continue;
        }
        labels.insert(key.clone(), value.clone());
    }
    if cfg.proxy_enabled {
        labels.insert(
            super::PROXY_ENDPOINT_LABEL.to_owned(),
            cfg.proxy_advertise_addr.trim().to_owned(),
        );
    }

    Ok(ConnectHello {
        node_id: cfg.worker_id.clone(),
        node_name,
        executor_kind: "boxlite".to_owned(),
        labels,
        version: crate::buildinfo::version().to_owned(),
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
        terminal_session_capacity: Some(TerminalSessionCapacity {
            max_active_sessions: cfg.terminal_max_active_sessions as i32,
            active_session_count,
        }),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use std::time::Duration;

    fn test_config() -> Config {
        Config {
            config_file: None,
            console_grpc_target: "127.0.0.1:50051".to_owned(),
            console_tls: false,
            worker_id: "worker-12345678".to_owned(),
            worker_secret: "secret".to_owned(),
            heartbeat_interval: Duration::from_secs(5),
            heartbeat_jitter_pct: 20,
            call_timeout: Duration::from_secs(13),
            node_name: String::new(),
            labels: Default::default(),
            boxlite_home: String::new(),
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

    #[test]
    fn build_hello_contains_expected_capabilities_and_capacity() {
        let mut cfg = test_config();
        cfg.terminal_max_active_sessions = 12;
        let hello = build_hello(&cfg, 7).expect("build hello");
        assert_eq!(hello.node_name, "worker-boxlite-worker-1");
        assert_eq!(hello.capabilities.len(), 4);
        assert_eq!(hello.capabilities[0].name, "echo");
        assert_eq!(hello.capabilities[1].name, "pythonExec");
        assert_eq!(hello.capabilities[2].name, "terminalExec");
        assert_eq!(hello.capabilities[3].name, "terminalResource");
        let capacity = hello
            .terminal_session_capacity
            .expect("terminal capacity declaration");
        assert_eq!(capacity.max_active_sessions, 12);
        assert_eq!(capacity.active_session_count, 7);
    }

    #[test]
    fn build_hello_advertises_explicit_unlimited_capacity() {
        let hello = build_hello(&test_config(), 0).expect("build hello");
        let capacity = hello
            .terminal_session_capacity
            .expect("terminal capacity declaration");
        assert_eq!(capacity.max_active_sessions, 0);
    }

    #[test]
    fn build_hello_advertises_proxy_endpoint_when_enabled() {
        let mut cfg = test_config();
        cfg.proxy_enabled = true;
        cfg.proxy_advertise_addr = "10.0.2.16:8091".to_owned();
        cfg.labels.insert(
            super::super::PROXY_ENDPOINT_LABEL.to_owned(),
            "forged:1".to_owned(),
        );
        let hello = build_hello(&cfg, 0).expect("build hello");
        assert_eq!(
            hello
                .labels
                .get(super::super::PROXY_ENDPOINT_LABEL)
                .map(String::as_str),
            Some("10.0.2.16:8091")
        );
    }

    #[test]
    fn build_hello_rejects_invalid_active_count() {
        assert!(build_hello(&test_config(), -1).is_err());
    }
}
