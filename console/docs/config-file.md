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

Value mapping:

- strings, integers, floats and booleans map to their environment variable form.
- arrays map to a list; `hidden_tools = ["echo"]` is equivalent to `CONSOLE_HIDDEN_TOOLS="echo"`.

Validation is identical to the environment variable path: an invalid or out-of-range value falls back to the default instead of aborting startup.

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
