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
- tables map to the `key=value,key=value` form; `[labels]` replaces `WORKER_LABELS`.
- nested tables are joined with `_`, so `[a.b] c = 1` matches `WORKER_A_B_C`.

Validation is identical to the environment variable path: an invalid or out-of-range value falls back to the default instead of aborting startup.

The loaded config file path is reported once at startup via the `config file loaded` log line.

See `config.example.toml` in the worker root for a full annotated template.

```toml
id = "wk_..."
secret = "..."
console_grpc_target = "console.internal:50051"
heartbeat_interval_sec = 5
terminal_exec_boxlite_image = "coolfan1024/onlyboxes-default-worker:0.0.5"
log_level = "info"

[labels]
region = "cn"
```
