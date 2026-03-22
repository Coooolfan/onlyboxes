use std::collections::HashMap;
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct Config {
    // Identity
    pub worker_id: String,
    pub worker_secret: String,
    pub version: String,
    pub node_name: String,

    // Console connection
    pub console_grpc_target: String,
    pub console_tls: bool,

    // Heartbeat
    pub heartbeat_interval: Duration,
    pub heartbeat_jitter_pct: u32,
    pub call_timeout: Duration,

    // Boxlite
    pub python_exec_image: String,
    pub terminal_exec_image: String,
    pub boxlite_default_cpus: u8,
    pub boxlite_default_memory_mib: u32,

    // Terminal session
    pub terminal_lease_min_sec: u64,
    pub terminal_lease_max_sec: u64,
    pub terminal_lease_default_sec: u64,
    pub terminal_output_limit_bytes: usize,

    // Logging
    pub log_level: String,
    pub log_format: String,

    // Labels
    pub labels: HashMap<String, String>,
}

impl Config {
    pub fn load() -> Self {
        let worker_id = env_required("WORKER_ID");
        let worker_secret = env_required("WORKER_SECRET");

        let heartbeat_sec = env_positive_int("WORKER_HEARTBEAT_INTERVAL_SEC", 5);
        let heartbeat_interval = Duration::from_secs(heartbeat_sec);

        let call_timeout_sec = env_positive_int(
            "WORKER_CALL_TIMEOUT_SEC",
            ((heartbeat_sec as f64 * 2.5).ceil()) as u64,
        );

        let node_name = env_or(
            "WORKER_NODE_NAME",
            &format!("worker-boxlite-{}", &worker_id[..worker_id.len().min(8)]),
        );

        let lease_min = env_positive_int("WORKER_TERMINAL_LEASE_MIN_SEC", 60);
        let mut lease_max = env_positive_int("WORKER_TERMINAL_LEASE_MAX_SEC", 1800);
        if lease_max < lease_min {
            lease_max = lease_min;
        }
        let lease_default = env_positive_int("WORKER_TERMINAL_LEASE_DEFAULT_SEC", 60)
            .clamp(lease_min, lease_max);

        Config {
            worker_id,
            worker_secret,
            version: env_or("WORKER_VERSION", "dev"),
            node_name,
            console_grpc_target: env_or("WORKER_CONSOLE_GRPC_TARGET", "127.0.0.1:50051"),
            console_tls: !env_bool("WORKER_CONSOLE_INSECURE", false),
            heartbeat_interval,
            heartbeat_jitter_pct: env_percent("WORKER_HEARTBEAT_JITTER_PCT", 20),
            call_timeout: Duration::from_secs(call_timeout_sec),
            python_exec_image: env_or("WORKER_PYTHON_EXEC_IMAGE", "python:slim"),
            terminal_exec_image: env_or(
                "WORKER_TERMINAL_EXEC_IMAGE",
                "coolfan1024/onlyboxes-default-worker:0.0.3",
            ),
            boxlite_default_cpus: env_positive_int("WORKER_BOXLITE_DEFAULT_CPUS", 1) as u8,
            boxlite_default_memory_mib: env_positive_int("WORKER_BOXLITE_DEFAULT_MEMORY_MIB", 512)
                as u32,
            terminal_lease_min_sec: lease_min,
            terminal_lease_max_sec: lease_max,
            terminal_lease_default_sec: lease_default,
            terminal_output_limit_bytes: env_positive_int(
                "WORKER_TERMINAL_OUTPUT_LIMIT_BYTES",
                1_048_576,
            ) as usize,
            log_level: env_log_level(),
            log_format: env_log_format(),
            labels: parse_labels(&env_or("WORKER_LABELS", "")),
        }
    }
}

fn env_required(key: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| panic!("{key} is required"))
}

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn env_positive_int(key: &str, default: u64) -> u64 {
    match std::env::var(key) {
        Ok(v) => v.parse::<u64>().unwrap_or(default).max(1),
        Err(_) => default,
    }
}

fn env_percent(key: &str, default: u32) -> u32 {
    match std::env::var(key) {
        Ok(v) => v.parse::<u32>().unwrap_or(default).min(100),
        Err(_) => default,
    }
}

fn env_bool(key: &str, default: bool) -> bool {
    match std::env::var(key) {
        Ok(v) => matches!(v.to_lowercase().as_str(), "1" | "true" | "yes" | "on"),
        Err(_) => default,
    }
}

fn env_log_level() -> String {
    let level = env_or("WORKER_LOG_LEVEL", "info").to_lowercase();
    match level.as_str() {
        "debug" | "info" | "warn" | "error" => level,
        _ => "info".to_string(),
    }
}

fn env_log_format() -> String {
    let fmt = env_or("WORKER_LOG_FORMAT", "json").to_lowercase();
    match fmt.as_str() {
        "json" | "text" => fmt,
        _ => "json".to_string(),
    }
}

fn parse_labels(csv: &str) -> HashMap<String, String> {
    let mut labels = HashMap::new();
    for pair in csv.split(',') {
        let pair = pair.trim();
        if pair.is_empty() {
            continue;
        }
        if let Some((k, v)) = pair.split_once('=') {
            labels.insert(k.trim().to_string(), v.trim().to_string());
        }
    }
    labels
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_labels_basic() {
        let labels = parse_labels("env=prod,region=us-east");
        assert_eq!(labels.get("env").unwrap(), "prod");
        assert_eq!(labels.get("region").unwrap(), "us-east");
    }

    #[test]
    fn test_parse_labels_empty() {
        let labels = parse_labels("");
        assert!(labels.is_empty());
    }

    #[test]
    fn test_parse_labels_with_spaces() {
        let labels = parse_labels(" key = value , foo = bar ");
        assert_eq!(labels.get("key").unwrap(), "value");
        assert_eq!(labels.get("foo").unwrap(), "bar");
    }

    #[test]
    fn test_parse_labels_no_equals() {
        let labels = parse_labels("invalid,also-invalid");
        assert!(labels.is_empty());
    }

    #[test]
    fn test_env_bool_true_variants() {
        for (i, val) in ["1", "true", "yes", "on", "TRUE", "Yes"].iter().enumerate() {
            let key = format!("__TEST_BOOL_TRUE_{i}");
            std::env::set_var(&key, val);
            assert!(env_bool(&key, false), "failed for {val}");
            std::env::remove_var(&key);
        }
    }

    #[test]
    fn test_env_bool_false_variants() {
        for (i, val) in ["0", "false", "no", "off", "anything"].iter().enumerate() {
            let key = format!("__TEST_BOOL_FALSE_{i}");
            std::env::set_var(&key, val);
            assert!(!env_bool(&key, true), "failed for {val}");
            std::env::remove_var(&key);
        }
    }

    #[test]
    fn test_env_bool_default() {
        assert!(env_bool("__TEST_BOOL_MISSING_T", true));
        assert!(!env_bool("__TEST_BOOL_MISSING_F", false));
    }

    #[test]
    fn test_env_positive_int() {
        std::env::set_var("__TEST_POSINT_42", "42");
        assert_eq!(env_positive_int("__TEST_POSINT_42", 1), 42);
        std::env::remove_var("__TEST_POSINT_42");
    }

    #[test]
    fn test_env_positive_int_zero_becomes_one() {
        std::env::set_var("__TEST_POSINT_0", "0");
        assert_eq!(env_positive_int("__TEST_POSINT_0", 5), 1);
        std::env::remove_var("__TEST_POSINT_0");
    }

    #[test]
    fn test_env_percent_clamped() {
        std::env::set_var("__TEST_PCT_200", "200");
        assert_eq!(env_percent("__TEST_PCT_200", 20), 100);
        std::env::remove_var("__TEST_PCT_200");
    }
}
