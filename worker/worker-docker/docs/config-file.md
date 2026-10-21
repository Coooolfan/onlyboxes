# Worker Docker Configuration

`worker-docker` reads configuration from environment variables and from a `config.toml` file.

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

Public preview proxy configuration:

- `WORKER_PROXY_ENABLED`: start the fixed HTTP proxy endpoint (default `false`).
- `WORKER_PROXY_LISTEN_ADDR`: local IP listener (default `:8091`); port `0` and hostnames are rejected, and a specific bind IP must match the advertise IP.
- `WORKER_PROXY_ADVERTISE_ADDR`: routable unicast IP and the same port reported to Console, for example `10.0.2.15:8091`; required when enabled.

Sandbox Docker network configuration:

- `WORKER_DOCKER_NETWORK`: optional bridge used by both `terminalExec` and `pythonExec` containers. The Worker creates a missing network with inter-container communication disabled, or validates that an existing network uses the `bridge` driver with `com.docker.network.bridge.enable_icc=false`. Pre-create the network when a specific subnet or gateway is required.

When `WORKER_DOCKER_NETWORK` is unset, existing behavior is preserved: containers use Docker's default network unless public preview is enabled, in which case only terminal containers join the automatically managed `onlyboxes-sandbox` bridge. When it is set, terminal container creation, IP inspection, session recovery, and Python execution all use the configured network.

The Docker daemon must manage its iptables/nftables firewall rules; deployments with Docker firewalling disabled are unsupported. Nginx must be the only network source allowed to reach the proxy listener.

See `config.example.toml` in the worker root for a full annotated template.

`terminal_max_active_sessions` maps to `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS`. `0` keeps the existing unlimited behavior; a positive value limits terminal sessions managed by this worker. Creating sessions, ready sessions, sessions waiting for in-flight commands to drain, and backend cleanup in progress all consume capacity. The configured maximum and current reservation count are sent in every Connect Hello.

```toml
id = "wk_..."
secret = "..."
console_grpc_target = "console.internal:50051"
heartbeat_interval_sec = 5
terminal_exec_docker_image = "coolfan1024/onlyboxes-runtime:default"
docker_network = "onlyboxes-sandbox-custom"
terminal_max_active_sessions = 0
log_level = "info"

[labels]
region = "cn"
```

## Snapshot lifecycle configuration

`WORKER_PERSISTENCE_ENABLED` defaults to `false`. When enabled, snapshot settings fail startup on invalid values. The Worker probes the local Docker daemon and trusted helper before advertising capabilities; it does not download an unpinned helper automatically. Public Console suspend/resume APIs must also be implemented and enabled independently.

| Environment variable | Default / requirement |
| --- | --- |
| `WORKER_SNAPSHOT_TMP_DIR` | Required absolute private directory on durable local storage; journal and archive files share this root |
| `WORKER_SNAPSHOT_DOCKER_HELPER_IMAGE` | Required locally available GNU tar image pinned by digest |
| `WORKER_SNAPSHOT_LEASE_GUARD_IMAGE` | Required locally available independent helper image, pinned by repository digest or full `sha256:` image ID; build with `Dockerfile.runtime-lease-guard` |
| `WORKER_SNAPSHOT_LEASE_GUARD_DOCKER_SOCKET` | `/var/run/docker.sock`; absolute Unix socket path on the daemon host, mounted read-only into the helper |
| `WORKER_SNAPSHOT_MAX_INFLIGHT` | `1`; range `1..64` |
| `WORKER_SNAPSHOT_TMP_QUOTA_BYTES` | `107374182400` (100 GiB); positive |
| `WORKER_SNAPSHOT_CACHE_MAX_BYTES` | `10737418240` (10 GiB); `0` disables unused committed-copy caching; cannot exceed tmp quota |
| `WORKER_SNAPSHOT_CACHE_TTL_SEC` | `86400` |
| `WORKER_SNAPSHOT_TRANSFER_MAX_CONCURRENCY` | `4`; range `1..16` across the Worker |
| `WORKER_SNAPSHOT_TRANSFER_IDLE_TIMEOUT_SEC` | `60`; maximum interval without byte progress |
| `WORKER_SNAPSHOT_DRAIN_TIMEOUT_SEC` | `60`; maximum wait for admitted source commands |

All snapshot duration settings accept `1..2592000` seconds. Each key maps to a flat TOML key without `WORKER_`, or to `[snapshot]` fields with the `snapshot_` prefix removed. `persistence_enabled` remains a top-level key.

The directory uses mode `0700`, files use `0600`, and a process lock binds the journal to one Worker identity. Keep this directory across binary upgrades, process restarts and container recreation; mount durable storage when the Worker itself runs in a container. Deleting the journal is not a cache reset. Startup rejects a missing journal if this Worker still owns persistence-labeled Docker resources, and rejects disabling persistence when the configured directory contains a journal. Console also rejects a Worker without persistence support while durable ownership remains, including expired routes.

The independent helper is created and validated before Hello. Its private volume and immutable native identity are bound in the Worker journal; removing or replacing them is not a cache reset. Startup refuses unknown or changed bindings and verifies that the helper reaches the same daemon as the Worker. The helper remains alive across Worker exit. See [lease guard deployment and current boundaries](runtime-lease-guard.md). Initial-session adoption and full Worker SIGKILL acceptance still require integration/validation.

Terminal containers carry a Worker identity label. Automatic orphan discovery is restricted to that identity, allowing isolated Workers to share one daemon without cleaning each other's resources. Unlabeled legacy orphans require explicit operator cleanup.

See [snapshot lifecycle controller](sandbox-snapshot-controller.md) for recovery, cache and validation boundaries.
