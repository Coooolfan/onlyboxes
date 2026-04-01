use std::collections::BTreeMap;
use std::env;
use std::time::Duration;

const DEFAULT_CONSOLE_TARGET: &str = "127.0.0.1:50051";
const DEFAULT_HEARTBEAT_INTERVAL_SEC: u64 = 5;
const DEFAULT_HEARTBEAT_JITTER_PCT: u8 = 20;
const DEFAULT_EXECUTOR_KIND: &str = "boxlite";
const DEFAULT_PYTHON_EXEC_IMAGE: &str = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim";
const DEFAULT_PYTHON_EXEC_MEMORY_MIB: u32 = 256;
const DEFAULT_PYTHON_EXEC_CPUS: u32 = 1;
const DEFAULT_PYTHON_EXEC_MAX_PROCESSES: u32 = 128;
const DEFAULT_TERMINAL_EXEC_IMAGE: &str = "coolfan1024/onlyboxes-default-worker:0.0.3";
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
        let heartbeat_interval_sec = parse_positive_u64_env(
            "WORKER_HEARTBEAT_INTERVAL_SEC",
            DEFAULT_HEARTBEAT_INTERVAL_SEC,
        );
        let heartbeat_jitter_pct =
            parse_percent_u8_env("WORKER_HEARTBEAT_JITTER_PCT", DEFAULT_HEARTBEAT_JITTER_PCT);
        let call_timeout_sec = parse_positive_u64_env(
            "WORKER_CALL_TIMEOUT_SEC",
            default_call_timeout_sec(heartbeat_interval_sec),
        );

        let terminal_lease_min_sec = parse_positive_u32_env(
            "WORKER_TERMINAL_LEASE_MIN_SEC",
            DEFAULT_TERMINAL_LEASE_MIN_SEC,
        );
        let mut terminal_lease_max_sec = parse_positive_u32_env(
            "WORKER_TERMINAL_LEASE_MAX_SEC",
            DEFAULT_TERMINAL_LEASE_MAX_SEC,
        );
        if terminal_lease_max_sec < terminal_lease_min_sec {
            terminal_lease_max_sec = terminal_lease_min_sec;
        }
        let terminal_lease_default_sec = clamp_u32(
            parse_positive_u32_env(
                "WORKER_TERMINAL_LEASE_DEFAULT_SEC",
                DEFAULT_TERMINAL_LEASE_DEFAULT_SEC,
            ),
            terminal_lease_min_sec,
            terminal_lease_max_sec,
        );

        let default_version = build_default_version();

        Self {
            console_grpc_target: get_env("WORKER_CONSOLE_GRPC_TARGET", DEFAULT_CONSOLE_TARGET),
            console_tls: env::var("WORKER_CONSOLE_INSECURE").unwrap_or_default() != "true",
            worker_id: env::var("WORKER_ID").unwrap_or_default().trim().to_owned(),
            worker_secret: env::var("WORKER_SECRET")
                .unwrap_or_default()
                .trim()
                .to_owned(),
            heartbeat_interval: Duration::from_secs(heartbeat_interval_sec),
            heartbeat_jitter_pct,
            call_timeout: Duration::from_secs(call_timeout_sec),
            node_name: env::var("WORKER_NODE_NAME").unwrap_or_default(),
            executor_kind: DEFAULT_EXECUTOR_KIND.to_owned(),
            version: get_env("WORKER_VERSION", &default_version),
            labels: parse_labels(&env::var("WORKER_LABELS").unwrap_or_default()),
            boxlite_home: env::var("WORKER_BOXLITE_HOME").unwrap_or_default(),
            python_exec_image: get_env(
                "WORKER_PYTHON_EXEC_BOXLITE_IMAGE",
                DEFAULT_PYTHON_EXEC_IMAGE,
            ),
            python_exec_memory_mib: parse_positive_u32_env(
                "WORKER_PYTHON_EXEC_MEMORY_MIB",
                DEFAULT_PYTHON_EXEC_MEMORY_MIB,
            ),
            python_exec_cpus: parse_positive_u32_env(
                "WORKER_PYTHON_EXEC_CPUS",
                DEFAULT_PYTHON_EXEC_CPUS,
            ),
            python_exec_max_processes: parse_positive_u32_env(
                "WORKER_PYTHON_EXEC_MAX_PROCESSES",
                DEFAULT_PYTHON_EXEC_MAX_PROCESSES,
            ),
            terminal_exec_image: get_env(
                "WORKER_TERMINAL_EXEC_BOXLITE_IMAGE",
                DEFAULT_TERMINAL_EXEC_IMAGE,
            ),
            terminal_exec_memory_mib: parse_positive_u32_env(
                "WORKER_TERMINAL_EXEC_MEMORY_MIB",
                DEFAULT_TERMINAL_EXEC_MEMORY_MIB,
            ),
            terminal_exec_cpus: parse_positive_u32_env(
                "WORKER_TERMINAL_EXEC_CPUS",
                DEFAULT_TERMINAL_EXEC_CPUS,
            ),
            terminal_exec_max_processes: parse_positive_u32_env(
                "WORKER_TERMINAL_EXEC_MAX_PROCESSES",
                DEFAULT_TERMINAL_EXEC_MAX_PROCESSES,
            ),
            terminal_lease_min_sec,
            terminal_lease_max_sec,
            terminal_lease_default_sec,
            terminal_output_limit_bytes: parse_positive_usize_env(
                "WORKER_TERMINAL_OUTPUT_LIMIT_BYTES",
                DEFAULT_TERMINAL_OUTPUT_LIMIT_BYTES,
            ),
            terminal_export_max_bytes: parse_positive_usize_env(
                "WORKER_TERMINAL_EXPORT_MAX_BYTES",
                0,
            ),
            echo_max_inflight: parse_positive_u32_env(
                "WORKER_ECHO_MAX_INFLIGHT",
                DEFAULT_MAX_INFLIGHT,
            ) as i32,
            python_exec_max_inflight: parse_positive_u32_env(
                "WORKER_PYTHON_EXEC_MAX_INFLIGHT",
                DEFAULT_MAX_INFLIGHT,
            ) as i32,
            terminal_exec_max_inflight: parse_positive_u32_env(
                "WORKER_TERMINAL_EXEC_MAX_INFLIGHT",
                DEFAULT_MAX_INFLIGHT,
            ) as i32,
            terminal_resource_max_inflight: parse_positive_u32_env(
                "WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT",
                DEFAULT_MAX_INFLIGHT,
            ) as i32,
            log_level: parse_log_level_env("WORKER_LOG_LEVEL", DEFAULT_LOG_LEVEL),
            log_format: parse_log_format_env("WORKER_LOG_FORMAT", DEFAULT_LOG_FORMAT),
            log_add_source: parse_bool_env("WORKER_LOG_ADD_SOURCE", DEFAULT_LOG_ADD_SOURCE),
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

fn get_env(key: &str, default_value: &str) -> String {
    match env::var(key) {
        Ok(value) if !value.is_empty() => value,
        _ => default_value.to_owned(),
    }
}

fn parse_positive_u64_env(key: &str, default_value: u64) -> u64 {
    env::var(key)
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .filter(|value| *value > 0)
        .unwrap_or(default_value)
}

fn parse_positive_u32_env(key: &str, default_value: u32) -> u32 {
    env::var(key)
        .ok()
        .and_then(|value| value.parse::<u32>().ok())
        .filter(|value| *value > 0)
        .unwrap_or(default_value)
}

fn parse_positive_usize_env(key: &str, default_value: usize) -> usize {
    env::var(key)
        .ok()
        .and_then(|value| value.parse::<usize>().ok())
        .filter(|value| *value > 0)
        .unwrap_or(default_value)
}

fn parse_percent_u8_env(key: &str, default_value: u8) -> u8 {
    env::var(key)
        .ok()
        .and_then(|value| value.parse::<u8>().ok())
        .filter(|value| *value <= 100)
        .unwrap_or(default_value)
}

fn parse_bool_env(key: &str, default_value: bool) -> bool {
    match env::var(key)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "1" | "true" | "yes" | "on" => true,
        "0" | "false" | "no" | "off" => false,
        _ => default_value,
    }
}

fn parse_log_level_env(key: &str, default_value: &str) -> String {
    let value = env::var(key)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();

    match value.as_str() {
        "debug" | "info" | "warn" | "error" => value,
        _ => default_value.to_owned(),
    }
}

fn parse_log_format_env(key: &str, default_value: &str) -> String {
    let value = env::var(key)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();

    match value.as_str() {
        "json" | "text" => value,
        _ => default_value.to_owned(),
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
