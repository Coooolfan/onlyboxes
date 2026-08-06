# Worker Boxlite Config File

`worker-boxlite` reads configuration from environment variables and from a `config.toml` file.

Lookup order for the config file:

1. `WORKER_CONFIG_FILE` — explicit path; the worker exits when the file is missing or invalid.
2. `config.toml` next to the worker binary.
3. `config.toml` in the current working directory.

When no file is found, the worker runs on environment variables and defaults only.

Value priority: environment variable > `config.toml` > built-in default.

Key mapping: a config file key is the environment variable name without the `WORKER_` prefix, lowercased.
For example `WORKER_TERMINAL_LEASE_MIN_SEC` becomes `terminal_lease_min_sec`, and `WORKER_ID` becomes `id`.

Value mapping:

- strings, integers, floats and booleans map to their environment variable form.
- arrays map to the JSON array form used by the matching environment variable.
- tables remain structured; `[labels]` replaces `WORKER_LABELS` without losing delimiters in label values.
- nested tables are joined with `_`, so `[a.b] c = 1` matches `WORKER_A_B_C`.

Validation is identical to the environment variable path: an invalid value normally falls back to the default. `terminal_max_active_sessions` is additionally bounded by the gRPC `int32` field; values above `2147483647` fail startup before the reconnect loop.

The loaded config file path is reported once at startup via the `config file loaded` log line.

See `config.example.toml` in the worker root for a full annotated template.

`terminal_max_active_sessions` maps to `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS`. `0` keeps the existing unlimited behavior; a positive value limits terminal sessions managed by this worker. Creating, ready, destroying, and Box cleanup in progress sessions all consume capacity. The configured maximum and current reservation count are sent in every Connect Hello.

```toml
id = "wk_..."
secret = "..."
console_grpc_target = "console.internal:50051"
heartbeat_interval_sec = 5
terminal_exec_boxlite_image = "coolfan1024/onlyboxes-runtime:default"
terminal_max_active_sessions = 0
log_level = "info"

[labels]
region = "cn"
```
