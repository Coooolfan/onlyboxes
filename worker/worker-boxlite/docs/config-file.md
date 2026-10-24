# Worker BoxLite Configuration

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

Public preview is opt-in. Set `WORKER_PROXY_ENABLED=true`, a fixed `WORKER_PROXY_LISTEN_ADDR`, the Nginx-reachable `WORKER_PROXY_ADVERTISE_ADDR` using the same port, and a comma-separated `WORKER_PROXY_SANDBOX_PORTS` list. The allowlist controls guest ports reachable through SDK tunnels. The worker exposes its token-protected proxy listener; guest connections do not require persistent host port mappings.

See `config.example.toml` in the worker root for a full annotated template.

`terminal_max_active_sessions` maps to `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS`. `0` keeps the existing unlimited behavior; a positive value limits terminal sessions managed by this worker. Creating, ready, destroying, and Box cleanup in progress sessions all consume capacity. The configured maximum and current reservation count are sent in every Connect Hello.

```toml
id = "wk_..."
secret = "..."
console_grpc_target = "console.internal:50051"
heartbeat_interval_sec = 5
terminal_exec_boxlite_image = "coolfan1024/onlyboxes-runtime:default"
terminal_exec_boxlite_disk_size_gb = 0
terminal_max_active_sessions = 0
proxy_enabled = true
proxy_listen_addr = "0.0.0.0:8091"
proxy_advertise_addr = "10.20.1.16:8091"
proxy_sandbox_ports = [3000, 8080]
log_level = "info"

[labels]
region = "cn"
```

`terminal_exec_boxlite_disk_size_gb` maps to `WORKER_TERMINAL_EXEC_BOXLITE_DISK_SIZE_GB`. Omission or `0` uses the SDK's image-derived root disk, which may provide substantially less than 1 GiB of writable space. Values 1–10240 request a sparse root disk in GiB; the SDK keeps at least the base image size. Invalid values fail startup. This affects newly created terminal Boxes; it does not resize existing sessions or pythonExec Boxes. Virtual capacity does not reserve host disk space.

Filesystem snapshots save the optional capacity in their private restore specification. Native inspection, archive validation and cache matching include it. Resume uses that recorded value even when the target Worker's configured default differs; changing a snapshot's requested capacity is rejected. Omitted capacity remains omitted for existing image-sized snapshots. Account archive quotas, staging budgets and actual free space continue to apply independently.

Changing this default while a Worker retains initial-boot records without lifecycle adoption is not yet an accepted transition. A local write-full → ordinary-session logical deletion → default change from image-sized to 4 GiB → restart did not reconnect one Worker and reported ownership/companion unavailable. The exact rejecting stage remains unresolved; its native data, journal and lease records remain retained. Successful new-session snapshot cycles do not establish recovery of that state, and startup must not replace its bound home with an empty one.

Filesystem persistence is opt-in through `[persistence] enabled = true` / `WORKER_PERSISTENCE_ENABLED=true`. It requires a dedicated `boxlite_home`, separate journal, already provisioned external lease state/control directories, a pinned store UUID, and a separate pythonExec home. Malformed persistence values fail startup. See [persistence configuration and deployment](sandbox-snapshot-deployment.md) for the complete field table, storage budget calculation, companion provisioning and remaining acceptance boundaries. `config.example.toml` includes all defaults.
