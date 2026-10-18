# Console Config File

`console` reads configuration from environment variables and from a `config.toml` file.

Lookup order for the config file:

1. `CONSOLE_CONFIG_FILE` — explicit path; the console exits when the file is missing or invalid.
2. `config.toml` next to the console binary.
3. `config.toml` in the current working directory.

When no file is found, the console runs on environment variables and defaults only.

Value priority: environment variable > `config.toml` > built-in default.

Key mapping: a config file key is the environment variable name without the `CONSOLE_` prefix, lowercased.
For example `CONSOLE_HTTP_ADDR` becomes `http_addr`, and `CONSOLE_DB_BUSY_TIMEOUT_MS` becomes `db_busy_timeout_ms`.

Nested tables are joined with `_`, so grouped sections map onto the flat environment names:

- `CONSOLE_MCP_TOOL_PYTHON_EXEC_DESCRIPTION` → `[mcp_tool.python_exec] description = "..."`
- `CONSOLE_MCP_TOOL_TERMINAL_EXEC_PARAM_SESSION_ID_DESCRIPTION` → `[mcp_tool.terminal_exec.param.session_id] description = "..."`

Value mapping:

- strings, integers, floats and booleans map to their environment variable form.
- arrays map to a list; `hidden_tools = ["echo"]` is equivalent to `CONSOLE_HIDDEN_TOOLS="echo"`.

Validation is identical to the environment variable path: an invalid or out-of-range value falls back to the default instead of aborting startup. MCP tool overrides keep their `unset` vs `explicitly empty` distinction: a key declared in the config file counts as set, and an empty param description still hides the parameter.

`CONSOLE_COMPUTER_USE_SESSION_ID_PREFIX` / `computer_use_session_id_prefix` controls the case-sensitive prefix used to interpret `readImage` and `exportFile` session IDs as Worker System targets. It defaults to `CU:`; for example, `CU:worker-id` selects that caller-owned Worker System. Changing the prefix changes public session-ID interpretation and is generally not recommended. An empty or whitespace-only value falls back to `CU:` and emits a warning.

The loaded config file path is reported once at startup via the `config file loaded` log line.

Secrets such as `dashboard_password`, `hash_key`, `jit_signing_key` and `export_file_sk` are better supplied through environment variables; keep the config file out of version control when they are inlined.

Public preview proxy configuration:

- `CONSOLE_PROXY_ENABLED`: enable route APIs and Worker proxy registration (default `false`).
- `CONSOLE_PROXY_PUBLIC_BASE_DOMAIN`: wildcard preview base domain, for example `public-preview.example.com`.
- `CONSOLE_PROXY_PUBLIC_SCHEME`: route URL scheme, `http` or `https` (default `https`). Use `http` only for trusted local development.
- `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN`: shared only with Nginx for `/internal/v1/proxy/resolve`; treat it as a secret.
- `CONSOLE_PROXY_ALLOWED_WORKER_CIDRS`: comma-separated CIDRs or a TOML array. Every advertised Worker proxy IP must match one entry.
- `CONSOLE_PROXY_ALLOWED_WORKER_PORTS`: comma-separated ports or a TOML array (default `8091`). Every advertised Worker proxy port must match one entry.
- `CONSOLE_PROXY_ALLOWED_DIRECT_DOMAINS`: comma-separated domain suffixes or a TOML array (default `e2b.app`). Every E2B direct origin must be a subdomain of one entry.
- `CONSOLE_PROXY_ROUTE_TTL_SEC`: persisted route lifetime (default `86400`, maximum `604800`).
- `CONSOLE_PROXY_ROUTE_KEY_LENGTH`: length of newly generated Base32 route keys (default `26`, range `8..26`). Existing routes keep working after this changes. Values below `16` provide less resistance to URL guessing and are intended only for trusted local or low-risk deployments.
- `CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT`: maximum active routes retained for one account (default `16`).
- `CONSOLE_PROXY_ROUTE_MAX_PER_SESSION`: maximum active routes retained for one terminal session (default `2`).

When proxy is enabled, base domain, internal token, and at least one direct domain are required. Worker CIDR/port allowlists are required for Docker/Boxlite registrations but may be empty in an E2B-only deployment. See the [Nginx deployment guide](../../docs/nginx/README.md) and [data-plane configuration example](../../docs/nginx/public-preview.conf.example).

See `config.example.toml` in the console root for a full annotated template.

```toml
http_addr = ":8089"
grpc_addr = ":50051"
db_path = "./db/onlyboxes-console.db"
log_level = "info"

[mcp_tool.python_exec]
description = "Execute python code in a sandbox."
```

## Sandbox persistence

`CONSOLE_PERSISTENCE_ENABLED` defaults to `false`. When enabled, Console starts the asynchronous lifecycle coordinator and accepts management-authenticated suspend/resume requests. Workers independently opt in and advertise exact runtime/version/platform/format support. The Docker Worker has an adapter; BoxLite lifecycle integration remains pending.

Snapshot settings are strict: invalid booleans, limits or duration values fail startup. Environment values override TOML values. Flat keys remove `CONSOLE_` and use lowercase; grouped tables such as `[snapshot.store] endpoint = "..."` and `[persistence] enabled = true` are equivalent.

| Environment variable | Default / requirement |
| --- | --- |
| `CONSOLE_SNAPSHOT_STORE_ENDPOINT` | Required HTTP(S) endpoint reachable by Console and Workers; no embedded credentials, query or fragment |
| `CONSOLE_SNAPSHOT_STORE_REGION` | Required signing region |
| `CONSOLE_SNAPSHOT_STORE_BUCKET_NAME` | Required pre-created bucket |
| `CONSOLE_SNAPSHOT_STORE_PREFIX` | `session-snapshots`; clean relative prefix, isolated from exportFile |
| `CONSOLE_SNAPSHOT_STORE_AK` / `CONSOLE_SNAPSHOT_STORE_SK` | Required Console-only credentials |
| `CONSOLE_SNAPSHOT_STORE_FORCE_PATH_STYLE` | `false` |
| `CONSOLE_SNAPSHOT_STORE_INTEGRITY_MODE` | `readback`; other modes are rejected |
| `CONSOLE_SNAPSHOT_PRESIGN_TTL_SEC` | `900`; at most seven days |
| `CONSOLE_SNAPSHOT_MULTIPART_PART_SIZE_BYTES` | `67108864`; 5 MiB through 5 GiB; at most 10,000 parts per archive |
| `CONSOLE_SNAPSHOT_MULTIPART_CONCURRENCY` | `4`; range `1..16` per upload |
| `CONSOLE_SNAPSHOT_MAX_BYTES` | `10737418240` (10 GiB) |
| `CONSOLE_SNAPSHOT_MAX_EXPANDED_BYTES` | `53687091200` (50 GiB) |
| `CONSOLE_SNAPSHOT_ACCOUNT_QUOTA_BYTES` | `107374182400` (100 GiB); includes retained generations and uploads |
| `CONSOLE_PERSISTENCE_RETENTION_TTL_SEC` | `604800` (7 days) |
| `CONSOLE_PERSISTENCE_MAX_RETENTION_TTL_SEC` | `2592000` (30 days); cannot be smaller than default retention |
| `CONSOLE_PERSISTENCE_OPERATION_TIMEOUT_SEC` | `1800`; range `1..86400` |
| `CONSOLE_SNAPSHOT_GC_INTERVAL_SEC` / `CONSOLE_SNAPSHOT_GC_GRACE_SEC` | `300` / `86400`; retained policy settings, cleanup processing remains pending |

Enabling admission runs a startup capability probe under a unique `<prefix>/probes/<random-id>` namespace with a 90-second deadline. It uploads two parts (5 MiB plus 17 bytes), completes and fully reads back the archive, checks signed Range GET, conditional manifest and multipart writes, object/multipart listing, abort, and deletion. Probe errors omit URLs and provider response bodies. Probe objects and uploads are cleaned with a separate bounded cleanup context. This verifies the configured credentials and endpoint from Console; deployment acceptance must also verify reachability and performance from each Worker and storage durability independent of the source machine.

Disabling admission preserves existing records and leaves operation reconciliation running. Keep the original store configuration while snapshots, accepted operations or pending deletion jobs remain; missing configuration fails startup rather than abandoning that work. Retained object references bind the endpoint/region/bucket/prefix namespace, so switching to a different namespace fails startup until an explicit data/reference migration is performed. Credential rotation within the same namespace is supported.

The coordinator uses eight concurrent jobs, a two-second recovery scan, 30-second reconciliation calls and a 60-second source drain budget. HTTP request cancellation does not cancel accepted operations. Signed transfer grants remain ephemeral; normal archive data flows directly between Worker and storage. Runtime execution leases and suspended data retention are separate budgets.

Logical deletion immediately hides the session and revokes routes, while a SQLite cleanup job retains native ownership and snapshot references. Physical native/object deletion, retention expiry and periodic orphan GC are not processed yet. Independent runtime lease enforcement after Worker process loss and real cross-machine acceptance also remain required before persistence is complete.
