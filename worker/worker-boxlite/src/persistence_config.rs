//! Persistence settings are strict: malformed limits must never silently enable
//! a different storage or ownership policy.
use super::Source;
use std::path::{Component, Path};

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PersistenceConfig {
    pub enabled: bool,
    pub journal_dir: String,
    pub lease_state_dir: String,
    pub lease_socket_dir: String,
    pub lease_store_id: String,
    pub python_home: String,
    pub max_inflight: usize,
    pub tmp_quota_bytes: i64,
    pub cache_max_bytes: i64,
    pub cache_ttl_sec: u64,
    pub drain_timeout_sec: u64,
    pub readiness_timeout_sec: u64,
    pub transfer_concurrency: usize,
    pub transfer_idle_sec: u64,
    invalid: Option<String>,
}
impl Default for PersistenceConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            journal_dir: String::new(),
            lease_state_dir: String::new(),
            lease_socket_dir: String::new(),
            lease_store_id: String::new(),
            python_home: String::new(),
            max_inflight: 2,
            tmp_quota_bytes: 64 << 30,
            cache_max_bytes: 4 << 30,
            cache_ttl_sec: 3600,
            drain_timeout_sec: 30,
            readiness_timeout_sec: 30,
            transfer_concurrency: 4,
            transfer_idle_sec: 30,
            invalid: None,
        }
    }
}
impl PersistenceConfig {
    pub(super) fn load(src: &Source) -> Self {
        Self::load_from(|key| src.get(key))
    }
    fn load_from(value: impl Fn(&str) -> String) -> Self {
        let mut cfg = Self::default();
        let enabled = value("WORKER_PERSISTENCE_ENABLED");
        cfg.enabled = match enabled.trim().to_ascii_lowercase().as_str() {
            "" | "false" | "0" | "no" | "off" => false,
            "true" | "1" | "yes" | "on" => true,
            _ => {
                cfg.invalid = Some("WORKER_PERSISTENCE_ENABLED".into());
                false
            }
        };
        cfg.journal_dir = value("WORKER_PERSISTENCE_JOURNAL_DIR");
        cfg.lease_state_dir = value("WORKER_PERSISTENCE_LEASE_STATE_DIR");
        cfg.lease_socket_dir = value("WORKER_PERSISTENCE_LEASE_SOCKET_DIR");
        cfg.lease_store_id = value("WORKER_PERSISTENCE_LEASE_STORE_ID");
        cfg.python_home = value("WORKER_PERSISTENCE_PYTHON_HOME");
        macro_rules! number {
            ($field:ident, $key:literal) => {
                let raw = value($key);
                if !raw.is_empty() {
                    match raw.parse() {
                        Ok(value) => cfg.$field = value,
                        Err(_) => cfg.invalid = Some($key.into()),
                    }
                }
            };
        }
        number!(max_inflight, "WORKER_PERSISTENCE_MAX_INFLIGHT");
        number!(tmp_quota_bytes, "WORKER_PERSISTENCE_TMP_QUOTA_BYTES");
        number!(cache_max_bytes, "WORKER_PERSISTENCE_CACHE_MAX_BYTES");
        number!(cache_ttl_sec, "WORKER_PERSISTENCE_CACHE_TTL_SEC");
        number!(drain_timeout_sec, "WORKER_PERSISTENCE_DRAIN_TIMEOUT_SEC");
        number!(
            readiness_timeout_sec,
            "WORKER_PERSISTENCE_READINESS_TIMEOUT_SEC"
        );
        number!(
            transfer_concurrency,
            "WORKER_PERSISTENCE_TRANSFER_CONCURRENCY"
        );
        number!(transfer_idle_sec, "WORKER_PERSISTENCE_TRANSFER_IDLE_SEC");
        cfg
    }
    pub fn validate(&self, home: &str) -> Result<(), String> {
        if let Some(key) = &self.invalid {
            return Err(format!("invalid {key}"));
        }
        if !self.enabled {
            return Ok(());
        }
        for (key, value) in [
            ("WORKER_BOXLITE_HOME", home),
            ("WORKER_PERSISTENCE_JOURNAL_DIR", &self.journal_dir),
            ("WORKER_PERSISTENCE_LEASE_STATE_DIR", &self.lease_state_dir),
            (
                "WORKER_PERSISTENCE_LEASE_SOCKET_DIR",
                &self.lease_socket_dir,
            ),
            ("WORKER_PERSISTENCE_PYTHON_HOME", &self.python_home),
        ] {
            if value.trim() != value
                || value.chars().any(char::is_control)
                || !Path::new(value).is_absolute()
                || Path::new(value)
                    .components()
                    .any(|c| matches!(c, Component::ParentDir | Component::CurDir))
                || value.split('/').any(|part| matches!(part, "." | ".."))
            {
                return Err(format!("{key} must be a normalized absolute directory"));
            }
        }
        let id = uuid::Uuid::parse_str(&self.lease_store_id)
            .map_err(|_| "WORKER_PERSISTENCE_LEASE_STORE_ID must be a canonical nonzero UUID")?;
        if id.is_nil() || id.to_string() != self.lease_store_id {
            return Err(
                "WORKER_PERSISTENCE_LEASE_STORE_ID must be a canonical nonzero UUID".into(),
            );
        }
        if !(1..=64).contains(&self.max_inflight)
            || !(1..=(1 << 60)).contains(&self.tmp_quota_bytes)
            || self.cache_max_bytes < 0
            || self.cache_max_bytes > self.tmp_quota_bytes
            || !(1..=600).contains(&self.drain_timeout_sec)
            || !(1..=600).contains(&self.readiness_timeout_sec)
            || !(1..=86400 * 30).contains(&self.cache_ttl_sec)
            || !(1..=16).contains(&self.transfer_concurrency)
            || !(1..=600).contains(&self.transfer_idle_sec)
        {
            return Err(
                "invalid WORKER_PERSISTENCE operation, staging, cache or transfer limits".into(),
            );
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn enabled() -> PersistenceConfig {
        PersistenceConfig {
            enabled: true,
            journal_dir: "/worker/journal".into(),
            lease_state_dir: "/worker/leases".into(),
            lease_socket_dir: "/worker/control".into(),
            lease_store_id: uuid::Uuid::new_v4().to_string(),
            python_home: "/worker/python".into(),
            ..Default::default()
        }
    }
    #[test]
    fn explicit_invalid_values_are_errors_and_disabled_defaults_need_no_paths() {
        PersistenceConfig::default().validate("").unwrap();
        for (key, value) in [
            ("WORKER_PERSISTENCE_ENABLED", "tru"),
            ("WORKER_PERSISTENCE_TMP_QUOTA_BYTES", "invalid"),
        ] {
            let config = PersistenceConfig::load_from(|candidate| {
                if candidate == key {
                    value.into()
                } else {
                    String::new()
                }
            });
            assert!(config.validate("").unwrap_err().contains(key));
        }
        let config = enabled();
        config.validate("/worker/home").unwrap();
        for path in [
            "relative",
            "/worker/../home",
            "/worker/./home",
            "/worker/home\n",
        ] {
            assert!(config.validate(path).is_err());
        }
        let mut config = enabled();
        config.lease_store_id = uuid::Uuid::nil().to_string();
        assert!(config.validate("/worker/home").is_err());
    }
    #[test]
    fn limits_cannot_overflow_or_silently_change_storage_policy() {
        for field in 0..7 {
            let mut config = enabled();
            match field {
                0 => config.max_inflight = 65,
                1 => config.tmp_quota_bytes = 0,
                2 => config.cache_max_bytes = config.tmp_quota_bytes + 1,
                3 => config.cache_max_bytes = -1,
                4 => config.transfer_concurrency = 17,
                5 => config.drain_timeout_sec = 601,
                _ => config.cache_ttl_sec = u64::MAX,
            }
            assert!(config.validate("/worker/home").is_err());
        }
    }
}
