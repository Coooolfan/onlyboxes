use std::collections::BTreeMap;
use std::time::Duration;

use crate::config_source::Source;

const DEFAULT_CONSOLE_TARGET: &str = "127.0.0.1:50051";
const DEFAULT_HEARTBEAT_INTERVAL_SEC: u64 = 5;
const DEFAULT_HEARTBEAT_JITTER_PCT: u8 = 20;
const DEFAULT_EXECUTOR_KIND: &str = "boxlite";
const DEFAULT_PYTHON_EXEC_IMAGE: &str = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim";
const DEFAULT_PYTHON_EXEC_MEMORY_MIB: u32 = 256;
const DEFAULT_PYTHON_EXEC_CPUS: u32 = 1;
const DEFAULT_PYTHON_EXEC_MAX_PROCESSES: u32 = 128;
const DEFAULT_TERMINAL_EXEC_IMAGE: &str = "coolfan1024/onlyboxes-default-worker:0.0.5";
const DEFAULT_TERMINAL_EXEC_MEMORY_MIB: u32 = 256;
const DEFAULT_TERMINAL_EXEC_CPUS: u32 = 1;
const DEFAULT_TERMINAL_EXEC_MAX_PROCESSES: u32 = 128;
const DEFAULT_TERMINAL_LEASE_MIN_SEC: u32 = 60;
const DEFAULT_TERMINAL_LEASE_MAX_SEC: u32 = 1800;
const DEFAULT_TERMINAL_LEASE_DEFAULT_SEC: u32 = 60;
const DEFAULT_TERMINAL_OUTPUT_LIMIT_BYTES: usize = 1024 * 1024;
const DEFAULT_MAX_INFLIGHT: u32 = 4;
const DEFAULT_LOG_LEVEL: &str = "info";
const DEFAULT_LOG_FORMAT: &str = "json";
const DEFAULT_LOG_ADD_SOURCE: bool = false;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Config {
    pub config_file: Option<String>,
    pub console_grpc_target: String,
    pub console_tls: bool,
    pub worker_id: String,
    pub worker_secret: String,
    pub heartbeat_interval: Duration,
    pub heartbeat_jitter_pct: u8,
    pub call_timeout: Duration,
    pub node_name: String,
    pub executor_kind: String,
    pub version: String,
    pub labels: BTreeMap<String, String>,
    pub boxlite_home: String,
    pub python_exec_image: String,
    pub python_exec_memory_mib: u32,
    pub python_exec_cpus: u32,
    pub python_exec_max_processes: u32,
    pub terminal_exec_image: String,
    pub terminal_exec_memory_mib: u32,
    pub terminal_exec_cpus: u32,
    pub terminal_exec_max_processes: u32,
    pub terminal_lease_min_sec: u32,
    pub terminal_lease_max_sec: u32,
    pub terminal_lease_default_sec: u32,
    pub terminal_output_limit_bytes: usize,
    pub terminal_export_max_bytes: usize,
    pub echo_max_inflight: i32,
    pub python_exec_max_inflight: i32,
    pub terminal_exec_max_inflight: i32,
    pub terminal_resource_max_inflight: i32,
    pub log_level: String,
    pub log_format: String,
    pub log_add_source: bool,
}

impl Config {
    pub fn load() -> Self {
        let src = Source::load();

        let heartbeat_interval_sec = src.positive_u64(
            "WORKER_HEARTBEAT_INTERVAL_SEC",
            DEFAULT_HEARTBEAT_INTERVAL_SEC,
        );
        let heartbeat_jitter_pct =
            src.percent_u8("WORKER_HEARTBEAT_JITTER_PCT", DEFAULT_HEARTBEAT_JITTER_PCT);
        let call_timeout_sec = src.positive_u64(
            "WORKER_CALL_TIMEOUT_SEC",
            default_call_timeout_sec(heartbeat_interval_sec),
        );

        let terminal_lease_min_sec = src.positive_u32(
            "WORKER_TERMINAL_LEASE_MIN_SEC",
            DEFAULT_TERMINAL_LEASE_MIN_SEC,
        );
        let mut terminal_lease_max_sec = src.positive_u32(
            "WORKER_TERMINAL_LEASE_MAX_SEC",
            DEFAULT_TERMINAL_LEASE_MAX_SEC,
        );
        if terminal_lease_max_sec < terminal_lease_min_sec {
            terminal_lease_max_sec = terminal_lease_min_sec;
        }
        let terminal_lease_default_sec = clamp_u32(
            src.positive_u32(
                "WORKER_TERMINAL_LEASE_DEFAULT_SEC",
                DEFAULT_TERMINAL_LEASE_DEFAULT_SEC,
            ),
            terminal_lease_min_sec,
            terminal_lease_max_sec,
        );

        let default_version = build_default_version();

        Self {
            config_file: src.path().map(str::to_owned),
            console_grpc_target: src
                .string_value("WORKER_CONSOLE_GRPC_TARGET", DEFAULT_CONSOLE_TARGET),
            console_tls: src.get("WORKER_CONSOLE_INSECURE") != "true",
            worker_id: src.get("WORKER_ID").trim().to_owned(),
            worker_secret: src.get("WORKER_SECRET").trim().to_owned(),
            heartbeat_interval: Duration::from_secs(heartbeat_interval_sec),
            heartbeat_jitter_pct,
            call_timeout: Duration::from_secs(call_timeout_sec),
            node_name: src.get("WORKER_NODE_NAME"),
            executor_kind: DEFAULT_EXECUTOR_KIND.to_owned(),
            version: src.string_value("WORKER_VERSION", &default_version),
            labels: parse_labels(&src.get("WORKER_LABELS")),
            boxlite_home: src.get("WORKER_BOXLITE_HOME"),
            python_exec_image: src.string_value(
                "WORKER_PYTHON_EXEC_BOXLITE_IMAGE",
                DEFAULT_PYTHON_EXEC_IMAGE,
            ),
            python_exec_memory_mib: src.positive_u32(
                "WORKER_PYTHON_EXEC_MEMORY_MIB",
                DEFAULT_PYTHON_EXEC_MEMORY_MIB,
            ),
            python_exec_cpus: src.positive_u32("WORKER_PYTHON_EXEC_CPUS", DEFAULT_PYTHON_EXEC_CPUS),
            python_exec_max_processes: src.positive_u32(
                "WORKER_PYTHON_EXEC_MAX_PROCESSES",
                DEFAULT_PYTHON_EXEC_MAX_PROCESSES,
            ),
            terminal_exec_image: src.string_value(
                "WORKER_TERMINAL_EXEC_BOXLITE_IMAGE",
                DEFAULT_TERMINAL_EXEC_IMAGE,
            ),
            terminal_exec_memory_mib: src.positive_u32(
                "WORKER_TERMINAL_EXEC_MEMORY_MIB",
                DEFAULT_TERMINAL_EXEC_MEMORY_MIB,
            ),
            terminal_exec_cpus: src
                .positive_u32("WORKER_TERMINAL_EXEC_CPUS", DEFAULT_TERMINAL_EXEC_CPUS),
            terminal_exec_max_processes: src.positive_u32(
                "WORKER_TERMINAL_EXEC_MAX_PROCESSES",
                DEFAULT_TERMINAL_EXEC_MAX_PROCESSES,
            ),
            terminal_lease_min_sec,
            terminal_lease_max_sec,
            terminal_lease_default_sec,
            terminal_output_limit_bytes: src.positive_usize(
                "WORKER_TERMINAL_OUTPUT_LIMIT_BYTES",
                DEFAULT_TERMINAL_OUTPUT_LIMIT_BYTES,
            ),
            terminal_export_max_bytes: src.positive_usize("WORKER_TERMINAL_EXPORT_MAX_BYTES", 0),
            echo_max_inflight: src.positive_u32("WORKER_ECHO_MAX_INFLIGHT", DEFAULT_MAX_INFLIGHT)
                as i32,
            python_exec_max_inflight: src
                .positive_u32("WORKER_PYTHON_EXEC_MAX_INFLIGHT", DEFAULT_MAX_INFLIGHT)
                as i32,
            terminal_exec_max_inflight: src
                .positive_u32("WORKER_TERMINAL_EXEC_MAX_INFLIGHT", DEFAULT_MAX_INFLIGHT)
                as i32,
            terminal_resource_max_inflight: src.positive_u32(
                "WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT",
                DEFAULT_MAX_INFLIGHT,
            ) as i32,
            log_level: src.log_level("WORKER_LOG_LEVEL", DEFAULT_LOG_LEVEL),
            log_format: src.log_format("WORKER_LOG_FORMAT", DEFAULT_LOG_FORMAT),
            log_add_source: src.bool_value("WORKER_LOG_ADD_SOURCE", DEFAULT_LOG_ADD_SOURCE),
        }
    }
}

fn build_default_version() -> String {
    let version = crate::buildinfo::version().trim();
    if version.is_empty() {
        "dev".to_owned()
    } else {
        version.to_owned()
    }
}

fn parse_labels(raw: &str) -> BTreeMap<String, String> {
    let mut labels = BTreeMap::new();
    for part in raw.split(',') {
        let entry = part.trim();
        if entry.is_empty() {
            continue;
        }
        let Some((key, value)) = entry.split_once('=') else {
            continue;
        };
        let key = key.trim();
        if key.is_empty() {
            continue;
        }
        labels.insert(key.to_owned(), value.trim().to_owned());
    }
    labels
}

fn default_call_timeout_sec(heartbeat_interval_sec: u64) -> u64 {
    let heartbeat = heartbeat_interval_sec.max(DEFAULT_HEARTBEAT_INTERVAL_SEC);
    (heartbeat * 5 + 1) / 2
}

fn clamp_u32(value: u32, min: u32, max: u32) -> u32 {
    value.max(min).min(max)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn load_uses_dynamic_call_timeout_by_default() {
        std::env::set_var("WORKER_HEARTBEAT_INTERVAL_SEC", "7");
        std::env::remove_var("WORKER_CALL_TIMEOUT_SEC");

        let cfg = Config::load();
        assert_eq!(cfg.call_timeout, Duration::from_secs(18));
    }

    #[test]
    fn load_parses_and_clamps_terminal_lease_values() {
        std::env::set_var("WORKER_TERMINAL_LEASE_MIN_SEC", "120");
        std::env::set_var("WORKER_TERMINAL_LEASE_MAX_SEC", "90");
        std::env::set_var("WORKER_TERMINAL_LEASE_DEFAULT_SEC", "500");

        let cfg = Config::load();
        assert_eq!(cfg.terminal_lease_min_sec, 120);
        assert_eq!(cfg.terminal_lease_max_sec, 120);
        assert_eq!(cfg.terminal_lease_default_sec, 120);
    }

    #[test]
    fn load_uses_boxlite_specific_defaults() {
        std::env::remove_var("WORKER_PYTHON_EXEC_BOXLITE_IMAGE");
        std::env::remove_var("WORKER_TERMINAL_EXEC_BOXLITE_IMAGE");
        std::env::set_var("WORKER_LABELS", "region=cn, owner = team-a, invalid");

        let cfg = Config::load();
        assert_eq!(cfg.python_exec_image, DEFAULT_PYTHON_EXEC_IMAGE);
        assert_eq!(cfg.terminal_exec_image, DEFAULT_TERMINAL_EXEC_IMAGE);
        assert_eq!(cfg.labels.get("region"), Some(&"cn".to_owned()));
        assert_eq!(cfg.labels.get("owner"), Some(&"team-a".to_owned()));
    }
}
