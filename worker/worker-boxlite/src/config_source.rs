use std::collections::BTreeMap;
use std::env;
use std::fs;
use std::path::{Path, PathBuf};

const CONFIG_FILE_NAME: &str = "config.toml";
const CONFIG_FILE_ENV_KEY: &str = "WORKER_CONFIG_FILE";
const ENV_PREFIX: &str = "WORKER_";

/// Resolves configuration values from environment variables first and falls
/// back to the values declared in `config.toml`.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Source {
    path: Option<String>,
    values: BTreeMap<String, String>,
}

impl Source {
    pub fn load() -> Self {
        let Some((path, explicit)) = config_file_path() else {
            return Self::default();
        };

        let raw = match fs::read_to_string(&path) {
            Ok(raw) => raw,
            Err(err) => {
                if !explicit && err.kind() == std::io::ErrorKind::NotFound {
                    return Self::default();
                }
                eprintln!("failed to read config file {}: {err}", path.display());
                std::process::exit(1);
            }
        };

        let parsed = match raw.parse::<toml::Value>() {
            Ok(toml::Value::Table(table)) => table,
            Ok(_) => {
                eprintln!("config file {} must be a TOML table", path.display());
                std::process::exit(1);
            }
            Err(err) => {
                eprintln!("failed to parse config file {}: {err}", path.display());
                std::process::exit(1);
            }
        };

        Self {
            path: Some(path.to_string_lossy().into_owned()),
            values: flatten(parsed),
        }
    }

    /// Returns the loaded config file path, `None` when no file was used.
    pub fn path(&self) -> Option<&str> {
        self.path.as_deref()
    }

    /// Returns the value for an environment variable key, falling back to the
    /// matching config file key (env key without the `WORKER_` prefix, lowercased).
    pub fn get(&self, env_key: &str) -> String {
        match env::var(env_key) {
            Ok(value) => value,
            Err(_) => self
                .values
                .get(&file_key(env_key))
                .cloned()
                .unwrap_or_default(),
        }
    }

    pub fn string_value(&self, env_key: &str, default_value: &str) -> String {
        let value = self.get(env_key);
        if value.is_empty() {
            default_value.to_owned()
        } else {
            value
        }
    }

    pub fn positive_u64(&self, env_key: &str, default_value: u64) -> u64 {
        self.parse_positive(env_key, default_value)
    }

    pub fn positive_u32(&self, env_key: &str, default_value: u32) -> u32 {
        self.parse_positive(env_key, default_value)
    }

    pub fn positive_usize(&self, env_key: &str, default_value: usize) -> usize {
        self.parse_positive(env_key, default_value)
    }

    fn parse_positive<T>(&self, env_key: &str, default_value: T) -> T
    where
        T: std::str::FromStr + PartialOrd + Default,
    {
        self.get(env_key)
            .trim()
            .parse::<T>()
            .ok()
            .filter(|value| *value > T::default())
            .unwrap_or(default_value)
    }

    pub fn percent_u8(&self, env_key: &str, default_value: u8) -> u8 {
        self.get(env_key)
            .trim()
            .parse::<u8>()
            .ok()
            .filter(|value| *value <= 100)
            .unwrap_or(default_value)
    }

    pub fn bool_value(&self, env_key: &str, default_value: bool) -> bool {
        match self.get(env_key).trim().to_ascii_lowercase().as_str() {
            "1" | "true" | "yes" | "on" => true,
            "0" | "false" | "no" | "off" => false,
            _ => default_value,
        }
    }

    pub fn log_level(&self, env_key: &str, default_value: &str) -> String {
        let value = self.get(env_key).trim().to_ascii_lowercase();
        match value.as_str() {
            "debug" | "info" | "warn" | "error" => value,
            _ => default_value.to_owned(),
        }
    }

    pub fn log_format(&self, env_key: &str, default_value: &str) -> String {
        let value = self.get(env_key).trim().to_ascii_lowercase();
        match value.as_str() {
            "json" | "text" => value,
            _ => default_value.to_owned(),
        }
    }
}

/// Returns the config file to load and whether it was requested explicitly
/// through `WORKER_CONFIG_FILE`.
fn config_file_path() -> Option<(PathBuf, bool)> {
    if let Ok(explicit) = env::var(CONFIG_FILE_ENV_KEY) {
        let explicit = explicit.trim();
        if !explicit.is_empty() {
            return Some((PathBuf::from(explicit), true));
        }
    }

    if let Ok(executable) = env::current_exe() {
        let executable = fs::canonicalize(&executable).unwrap_or(executable);
        if let Some(dir) = executable.parent() {
            let candidate = dir.join(CONFIG_FILE_NAME);
            if is_file(&candidate) {
                return Some((candidate, false));
            }
        }
    }

    let candidate = PathBuf::from(CONFIG_FILE_NAME);
    if is_file(&candidate) {
        return Some((candidate, false));
    }

    None
}

fn is_file(path: &Path) -> bool {
    path.metadata().map(|meta| meta.is_file()).unwrap_or(false)
}

fn file_key(env_key: &str) -> String {
    env_key
        .strip_prefix(ENV_PREFIX)
        .unwrap_or(env_key)
        .to_ascii_lowercase()
}

/// Converts TOML values into the same string form accepted by the environment
/// variable parsers.
fn encode_value(value: &toml::Value) -> Option<String> {
    match value {
        toml::Value::String(value) => Some(value.clone()),
        toml::Value::Integer(value) => Some(value.to_string()),
        toml::Value::Float(value) => Some(value.to_string()),
        toml::Value::Boolean(value) => Some(value.to_string()),
        toml::Value::Array(items) => {
            let encoded = items
                .iter()
                .map(encode_value)
                .collect::<Option<Vec<String>>>()?;
            serde_json::to_string(&encoded).ok()
        }
        toml::Value::Table(table) => {
            let encoded = table
                .iter()
                .filter_map(|(key, value)| encode_value(value).map(|value| (key.clone(), value)))
                .collect::<BTreeMap<String, String>>();
            serde_json::to_string(&encoded).ok()
        }
        toml::Value::Datetime(_) => None,
    }
}

/// Flattens a TOML table into the string values used by the parsers. Nested
/// tables are additionally exposed as `parent_child` keys so that grouped
/// sections map onto flat env names.
fn flatten(table: toml::value::Table) -> BTreeMap<String, String> {
    let mut values = BTreeMap::new();
    flatten_into(&mut values, "", &table);
    values
}

fn flatten_into(values: &mut BTreeMap<String, String>, prefix: &str, table: &toml::value::Table) {
    for (key, value) in table {
        let full_key = if prefix.is_empty() {
            key.to_ascii_lowercase()
        } else {
            format!("{prefix}_{}", key.to_ascii_lowercase())
        };
        if let toml::Value::Table(nested) = value {
            flatten_into(values, &full_key, nested);
        }
        if let Some(encoded) = encode_value(value) {
            values.insert(full_key, encoded);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn source_from_toml(raw: &str) -> Source {
        let table = match raw.parse::<toml::Value>().expect("valid toml") {
            toml::Value::Table(table) => table,
            _ => panic!("expected table"),
        };
        Source {
            path: None,
            values: flatten(table),
        }
    }

    #[test]
    fn file_key_strips_worker_prefix() {
        assert_eq!(file_key("WORKER_LOG_LEVEL"), "log_level");
        assert_eq!(file_key("LOG_LEVEL"), "log_level");
    }

    #[test]
    fn encodes_scalars_arrays_and_tables() {
        let src = source_from_toml(
            r#"
console_grpc_target = "10.0.0.1:50051"
heartbeat_interval_sec = 7
console_insecure = true
read_image_allowed_paths = ["/tmp", "/var"]

[labels]
owner = "team-a"
region = "cn"
"#,
        );

        // Asserted on the file values directly: process-wide environment
        // variables would otherwise take precedence.
        assert_eq!(
            src.values.get("console_grpc_target").map(String::as_str),
            Some("10.0.0.1:50051")
        );
        assert_eq!(
            src.values.get("heartbeat_interval_sec").map(String::as_str),
            Some("7")
        );
        assert_eq!(
            src.values.get("console_insecure").map(String::as_str),
            Some("true")
        );
        assert_eq!(
            src.values
                .get("read_image_allowed_paths")
                .map(String::as_str),
            Some(r#"["/tmp","/var"]"#)
        );
        assert_eq!(
            src.values.get("labels").map(String::as_str),
            Some(r#"{"owner":"team-a","region":"cn"}"#)
        );
    }

    #[test]
    fn falls_back_to_defaults_for_missing_keys() {
        let src = source_from_toml("log_level = \"nope\"\n");

        assert_eq!(src.string_value("WORKER_NODE_NAME", "local"), "local");
        assert_eq!(src.log_level("WORKER_LOG_LEVEL", "info"), "info");
        assert_eq!(src.log_format("WORKER_LOG_FORMAT", "json"), "json");
        assert_eq!(src.percent_u8("WORKER_HEARTBEAT_JITTER_PCT", 20), 20);
    }

    #[test]
    fn empty_env_overrides_config_file_value() {
        let key = "WORKER_CONFIG_SOURCE_EMPTY_OVERRIDE_TEST";
        let mut src = Source::default();
        src.values.insert(file_key(key), "from-file".to_owned());
        env::set_var(key, "");

        assert_eq!(src.get(key), "");

        env::remove_var(key);
    }
}
