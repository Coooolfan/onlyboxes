# Console Overview

Configuration sources:
- environment variables and `config.toml` (see `README/config-file.md`).
- priority is environment variable > `config.toml` > default.

The console service hosts:
- a gRPC registry endpoint with bidirectional stream `Connect` for worker registration + heartbeat + command dispatch/result.
- embedded web dashboard static hosting:
  - `GET /` serves embedded `web` frontend.
  - `GET /assets/*` serves bundled static assets.
  - `GET /static/*` serves embedded static public files such as `/static/worker-startup.sh`.
  - unknown `GET/HEAD` routes return `404 Not Found`.
  - `/api/*` and `/mcp` are reserved for backend handlers and are not served as frontend pages.
- REST APIs for worker data (dashboard authentication, role-scoped):
  - `GET /api/v1/workers` for paginated worker listing.
  - `GET /api/v1/workers/stats` for aggregated worker status metrics.
  - `GET /api/v1/workers/inflight` for per-capability inflight and terminal session capacity snapshots.
  - `POST /api/v1/workers` for creating provisioned worker credentials and returning startup command.
  - `DELETE /api/v1/workers/:node_id` for deleting a provisioned worker and revoking its credential (online worker is disconnected immediately).
  - `GET /api/v1/workers/:node_id/startup-command` always returns `410 Gone`.
  - `worker_secret` is returned once in `POST /api/v1/workers` response and is not queryable from read APIs.
  - worker types:
    - `normal` (maps to `worker-docker`)
    - `worker-sys` (maps to host-shell worker)
  - worker create/delete visibility rules:
    - admin: list/stats/inflight/delete all workers; create `normal` and `worker-sys`
    - non-admin: list/stats/inflight only own `worker-sys`; can create/delete only own `worker-sys`
  - `worker-sys` constraints:
    - max one per account
    - only `computerUse` and `readImage` capabilities are accepted
    - `computerUse.max_inflight` and `readImage.max_inflight` are both forced to `1`
- public preview route APIs (dashboard cookie/API key/JIT auth):
  - `POST /api/v1/proxy-routes` creates an anonymous preview URL for an owned terminal session and port.
  - `GET /api/v1/proxy-routes` lists only the current account's in-memory routes.
  - `DELETE /api/v1/proxy-routes/:route_key` deletes only the current account's route; cross-account access returns `404`.
  - routes default to 24 hours, are capped at 7 days, use 128-bit DNS-safe keys, and are lost on Console restart.
  - `GET /internal/v1/proxy/resolve` is Nginx-only and protected by `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN`. Docker/Boxlite return a Worker URL plus a 15-second Route Token; E2B is resolved through its internal Worker capability and returns the current sandbox origin plus traffic token.
  - proxy traffic is anonymous and never carries Dashboard credentials; anyone holding the preview URL can access it.
- command APIs (execution, bearer token required):
  - `POST /api/v1/commands/echo` for blocking echo command execution.
  - `POST /api/v1/commands/terminal` for blocking terminal command execution over `terminalExec` capability.
  - `POST /api/v1/commands/computer-use` for blocking host-shell execution over `computerUse` capability.
  - `POST /api/v1/tasks` for sync/async/auto task submission.
  - `GET /api/v1/tasks/:task_id` for task status and result lookup.
  - `POST /api/v1/tasks/:task_id/cancel` for best-effort task cancellation.
  - request header: `Authorization: Bearer <access-token>`.
  - accepted bearer token types:
    - trusted token managed by dashboard `GET/POST/DELETE /api/v1/console/tokens`
    - JIT token (`obx_jit_v1.<payload>.<signature>`) signed with `CONSOLE_JIT_SIGNING_KEY`
  - owner isolation is account-scoped: token resolves to `account_id`, and task/session ownership uses `account_id`.
  - task visibility: task lookup/cancel is owner-scoped by account; same-account tokens can access shared tasks, cross-account access returns `404`.
  - task idempotency: `request_id` de-duplication is scoped per account.
  - terminal session capacity routing:
    - only requests for a console-generated new terminal session use capacity-aware candidate ordering.
    - known-available workers are preferred, legacy/unknown workers are next, and reported-full workers are last-resort probes.
    - an existing session route remains pinned to its original worker even when that worker reports full active-session capacity.
    - `max_inflight` and active-session capacity are evaluated independently; the worker-local session manager remains the final authority.
    - an explicit pre-execution `session_capacity_exceeded` can be retried on an untried worker only after the provisional route is safely removed; all attempts share the task deadline.
  - terminal session restart recovery:
    - confirmed routes retain their worker binding and absolute lease while the worker is disconnected; requests return retryable `session_unavailable` instead of being reassigned.
    - a terminal-capable worker must reconcile every Console candidate and receive a recovery acknowledgement before it becomes dispatchable.
    - recovered routes keep their original lease; missing, invalid, or expired resources delete the route so later calls follow normal `session_not_found`/creation semantics.
    - route and lease state are currently in Console memory and therefore do not survive a Console restart.
- MCP Streamable HTTP API (bearer token required):
  - `POST /mcp` for JSON-RPC requests over Streamable HTTP transport.
  - recommended request header: `Authorization: Bearer <access-token>`.
  - fallback for MCP clients that cannot set custom headers: `POST /mcp?token=<access-token>`.
    The query parameter name defaults to `token` and can be changed with `CONSOLE_MCP_TOKEN_QUERY_PARAM`.
    Prefer the header when available because URL query tokens can be captured by logs, browser history, or intermediaries; use HTTPS in production.
  - accepted bearer token types:
    - trusted token managed by dashboard `GET/POST/DELETE /api/v1/console/tokens`
    - JIT token (`obx_jit_v1.<payload>.<signature>`) signed with `CONSOLE_JIT_SIGNING_KEY`
  - if the trusted token list is empty, trusted-token auth for `/mcp` is unavailable; valid JIT tokens can still authenticate when `CONSOLE_JIT_SIGNING_KEY` is configured.
  - `GET /mcp` is intentionally unsupported and returns `405` with `Allow: POST`.
  - stream behavior is JSON response only (`application/json`), no SSE streaming channel.
  - tool argument validation is strict (`additionalProperties=false`): unknown input fields are rejected with JSON-RPC `invalid params (-32602)`.
  - exposed tools:
    - `echo`
      - input: `{"message":"...","timeout_ms":5000}`
      - `message` is required (whitespace-only is rejected).
      - `timeout_ms` is optional, range `1..60000`, default `5000`.
      - output: `{"message":"..."}`
    - `pythonExec`
      - input: `{"code":"print(1)","timeout_ms":60000}`
      - `code` is required (whitespace-only is rejected).
      - `timeout_ms` is optional, range `1..600000`, default `60000`.
      - output: `{"output":"...","stderr":"...","exit_code":0}`
      - non-zero `exit_code` is returned as normal tool output, not as MCP protocol error.
    - `terminalExec`
      - input: `{"command":"pwd","session_id":"optional","create_if_missing":false,"lease_ttl_sec":60,"timeout_ms":60000}`
      - `command` is required (whitespace-only is rejected).
      - `session_id` is optional; omit to create a new terminal session/container.
      - `create_if_missing` controls behavior when `session_id` does not exist.
      - session isolation is account-scoped: same-account tokens can reuse `session_id`; cross-account use returns `session_not_found`.
      - `lease_ttl_sec` is optional and validated by worker-side lease bounds.
      - `timeout_ms` is optional, range `1..600000`, default `60000`.
      - output: `{"session_id":"...","created":true,"stdout":"...","stderr":"...","exit_code":0,"stdout_truncated":false,"stderr_truncated":false,"lease_expires_unix_ms":...}`
    - `computerUse`
      - input: `{"command":"pwd","timeout_ms":60000,"request_id":"optional"}`
      - `command` is required (whitespace-only is rejected).
      - legacy `lease_ttl_sec` is ignored when provided.
      - payload excludes terminal session fields (`session_id`, `create_if_missing`, `created`).
      - routed only to caller-owned `worker-sys` and account-scoped capacity is single-flight.
      - worker-side concurrency is also enforced per capability; dispatch beyond the worker's declared `computerUse` limit returns `session_busy` (HTTP `409` in command API). The limit defaults to `1`, so concurrent dispatch is rejected unless the worker raises `WORKER_COMPUTER_USE_MAX_INFLIGHT`.
      - MCP tool readiness failures use JSON-RPC application errors:
        - `-32010` with `data.error_code="WORKER_SYS_REQUIRED"` when the account has no `worker-sys`.
        - `-32011` with `data.error_code="WORKER_SYS_OFFLINE"` when a `worker-sys` is registered but offline.
      - output: `{"stdout":"...","stderr":"...","exit_code":0,"stdout_truncated":false,"stderr_truncated":false}`
    - `readImage`
      - input: `{"session_id":"required","file_path":"required","timeout_ms":60000}`
      - `session_id` and `file_path` are required (whitespace-only is rejected).
      - `session_id=="computerUse"` routes to caller-owned `worker-sys` via `readImage` capability.
      - other `session_id` values route via worker `terminalResource` capability.
      - `worker-sys` accepts only `session_id=="computerUse"` for this capability.
      - validates file existence; directories are rejected.
      - output is content-only (no structured output fields).
      - image files (`image/*`) return exactly one `image` content item.
      - non-image files return exactly one `text` content item:
        - `unsupported mime type: <mime>; expected image/*`
      - non-format failures (session/file missing, busy, timeout, read failure) are returned as tool errors.
    - `exportFile`
      - input: `{"session_id":"required","file_path":"required","timeout_ms":60000}`
      - `session_id` and `file_path` are required (whitespace-only is rejected).
      - only available when all export-file objectstore env vars are configured.
      - `session_id=="computerUse"` routes to the caller-owned `worker-sys` via `readImage` capability with `action="export"`.
      - other `session_id` values route via worker `terminalResource` capability with `action="export"` (Docker-backed terminal sessions).
      - console generates a presigned upload URL, dispatches the appropriate export action based on routing, then returns a presigned download URL.
      - output fields depend on `CONSOLE_EXPORT_RETURN_SCHEMA`:
        - `ALL` (default): `{"signed_url":"...","object_key":"...","filename":"..."}`
        - `SIGNED_URL`: `{"signed_url":"..."}`
        - `OBJECTKEY`: `{"object_key":"...","filename":"..."}`
      - when `OBJECTKEY` is configured, the console skips generating a presigned download URL entirely.
      - non-format failures (session/file missing, busy, timeout, upload failure) are returned as tool errors.
- dashboard authentication APIs:
  - `POST /api/v1/console/login` with `{"username":"...","password":"..."}`.
  - login response includes `authenticated`, `account`, `registration_enabled`, `console_version`, `console_repo_url`.
  - `POST /api/v1/console/logout`.
  - `GET /api/v1/console/session` returns current session account payload with `console_version` and `console_repo_url`.
  - `POST /api/v1/console/password` changes current account password (requires `current_password` + `new_password`; successful update rotates account sessions).
  - `POST /api/v1/console/register` creates non-admin account (admin-only, and only when `CONSOLE_ENABLE_REGISTRATION=true`).
  - account management (admin only):
    - `GET /api/v1/console/accounts` lists accounts with pagination (`page`, `page_size`).
    - `DELETE /api/v1/console/accounts/:account_id` deletes a non-admin account.
    - deleting self and deleting admin accounts are both rejected with `403`.
  - token management (requires dashboard cookie session auth):
    - `GET /api/v1/console/tokens` list current account token metadata (`id`, `name`, masked token).
    - `POST /api/v1/console/tokens` create token bound to current account (manual token or auto-generated, plaintext returned only in create response).
    - `GET /api/v1/console/tokens/:token_id/value` always returns `410 Gone`.
    - token plaintext is delivered in `POST /api/v1/console/tokens` response only.
    - `DELETE /api/v1/console/tokens/:token_id` delete token (current account only, cross-account returns `404`).
    - console API keys and dashboard JIT tokens are rejected for these endpoints so dashboard bearer credentials cannot mint MCP trusted tokens.
  - console API key management (dashboard auth):
    - `GET /api/v1/console/api-keys` lists current account API key metadata (`id`, `name`, masked key).
    - `POST /api/v1/console/api-keys` creates an auto-generated API key bound to current account; plaintext is returned only in the create response.
    - `DELETE /api/v1/console/api-keys/:api_key_id` deletes current account API key; cross-account delete returns `404`.
  - dashboard auth accepts cookie session, console API key via `Authorization: Bearer <api-key>`, or dashboard JIT bearer token when configured.
  - dashboard JIT bearer token format is `obx_dashboard_jit_v1.<payload>.<signature>` with `CONSOLE_DASHBOARD_JIT_SIGNING_KEY`.
  - dashboard JIT tokens require payload `iss`, `sub`, `scope:"dashboard"`, optional `exp` (Unix milliseconds), and use the same `(iss, sub) -> account` derivation as MCP JIT.
  - dashboard JIT accounts are non-admin, cannot log in with a password, and cannot authenticate `/mcp`.
  - bearer precedence is strict for dashboard auth: if `Authorization: Bearer <api-key>` is present, cookie session is not used as fallback.
  - non-Bearer `Authorization` headers do not participate in dashboard API key auth and do not block cookie-session auth.
  - sensitive account actions require cookie session only:
    - `POST /api/v1/console/password`
    - `POST /api/v1/console/api-keys`
    - `DELETE /api/v1/console/api-keys/:api_key_id`
    - `GET /api/v1/console/tokens`
    - `POST /api/v1/console/tokens`
    - `DELETE /api/v1/console/tokens/:token_id`
    - `GET /api/v1/console/tokens/:token_id/value`

Hidden tools (`CONSOLE_HIDDEN_TOOLS`):
- comma-separated list of tool names to hide from MCP `tools/list`.
- valid tool names: `echo`, `pythonExec`, `terminalExec`, `computerUse`, `readImage`, `exportFile`.
- hidden tools are omitted from MCP `tools/list`.
- hidden tools remain callable via MCP `tools/call` if the caller already knows the tool name.
- console currently has no separate HTTP tool-list endpoint, so there is no HTTP list filtering to apply here.
- default: empty (all tools visible).
- example: `CONSOLE_HIDDEN_TOOLS=echo,computerUse`
- this list always uses the **internal capability ID** (e.g. `echo`), even when the tool has been renamed via `CONSOLE_MCP_TOOL_<TOOL>_NAME`.

MCP tool description / parameter overrides:
- `CONSOLE_MCP_TOOL_<TOOL>_NAME` — override the `name` exposed via `tools/list` (and used as the `tools/call` routing key). Must match `^[a-zA-Z0-9_-]{1,64}$`; empty / invalid / collides-with-another-tool's-default values fall back with a warn. After changing this, MCP clients must refresh their cached `tools/list`.
- `CONSOLE_MCP_TOOL_<TOOL>_TITLE` — override the tool's human-readable title (empty string = fallback + warn).
- `CONSOLE_MCP_TOOL_<TOOL>_DESCRIPTION` — override the tool's `description` (empty string = fallback + warn).
- `CONSOLE_MCP_TOOL_<TOOL>_PARAM_<PARAM>_DESCRIPTION` — override one parameter's `description`; empty string hides the parameter from `tools/list` (removed from `properties` + `required`, `additionalProperties` flipped to `true`) while `tools/call` still accepts the field. Every hidden parameter emits `WARN hiding MCP tool parameter ... required=<bool>` on startup; hiding a required parameter means the model cannot construct a valid call.
- `<TOOL>` uses `UPPER_SNAKE_CASE` (e.g. `pythonExec` → `PYTHON_EXEC`); `<PARAM>` is the uppercased snake_case JSON key (e.g. `session_id` → `SESSION_ID`).
- unset vars keep the built-in defaults; `os.LookupEnv` is used so an explicitly empty string is distinguishable.
- catalog: `ECHO` (`MESSAGE`, `TIMEOUT_MS`), `PYTHON_EXEC` (`CODE`, `TIMEOUT_MS`), `TERMINAL_EXEC` (`COMMAND`, `SESSION_ID`, `CREATE_IF_MISSING`, `LEASE_TTL_SEC`, `TIMEOUT_MS`), `COMPUTER_USE` (`COMMAND`, `TIMEOUT_MS`, `REQUEST_ID`), `READ_IMAGE` (`SESSION_ID`, `FILE_PATH`, `TIMEOUT_MS`), `EXPORT_FILE` (`SESSION_ID`, `FILE_PATH`, `TIMEOUT_MS`).
- example: `CONSOLE_MCP_TOOL_ECHO_NAME="ping"`, `CONSOLE_MCP_TOOL_ECHO_DESCRIPTION="ping-only echo"`, `CONSOLE_MCP_TOOL_TERMINAL_EXEC_PARAM_SESSION_ID_DESCRIPTION=""`.

Export file objectstore config:
- `CONSOLE_EXPORT_FILE_ENDPOINT`: S3-compatible endpoint URL for presigned upload/download.
- `CONSOLE_EXPORT_FILE_REGION`: explicit signing region for the S3-compatible endpoint.
- `CONSOLE_EXPORT_FILE_BUCKET_NAME`: destination bucket for exported files.
- `CONSOLE_EXPORT_FILE_EXPORT_PREFIX`: object key prefix prepended to every export.
- `CONSOLE_EXPORT_FILE_AK`: access key used for presigning.
- `CONSOLE_EXPORT_FILE_SK`: secret key used for presigning.
- `CONSOLE_EXPORT_FILE_UPLOAD_PRESIGN_TTL_SEC`: upload presign TTL in seconds (default `900`).
- `CONSOLE_EXPORT_FILE_DOWNLOAD_PRESIGN_TTL_SEC`: download presign TTL in seconds (default `3600`).
- `CONSOLE_EXPORT_RETURN_SCHEMA`: controls which fields `exportFile` returns. Values: `ALL` (default, returns `signed_url` + `object_key` + `filename`), `SIGNED_URL` (returns only `signed_url`), `OBJECTKEY` (returns only `object_key` + `filename`, skips presigned download URL generation).
- `exportFile` is registered only when the 6 core objectstore variables above (`ENDPOINT/REGION/BUCKET_NAME/EXPORT_PREFIX/AK/SK`) are non-empty.

Security warning (high risk):
- console gRPC currently has no built-in TLS/mTLS.
- `worker-docker` rejects insecure console endpoints by default; plaintext is allowed only with `WORKER_CONSOLE_INSECURE=true`.
- place console HTTP (`:8089`) and gRPC (`:50051`) behind a reverse proxy/gateway and enforce TLS for all external traffic.
- `worker_secret` is sent in `ConnectHello`; on untrusted networks it can still be observed in transit when plaintext is enabled.
- deploy only on trusted private networks or encrypted tunnels; do not expose gRPC directly to the public internet.
- when public preview is enabled, allow Nginx alone to reach Worker proxy ports and keep `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN` secret.
- every `obx.proxy_endpoint` IP must match `CONSOLE_PROXY_ALLOWED_WORKER_CIDRS`; Console rejects out-of-policy Worker registration.
- fully mitigating this risk requires TLS/mTLS support (not implemented in this release).

Credential behavior:
- `console` starts with `0` workers.
- worker credentials are generated on demand by dashboard/API `POST /api/v1/workers` with explicit `type`.
- credentials are persisted in SQLite as HMAC-SHA256 hashes only (no plaintext storage).
- deleting a provisioned worker revokes the credential immediately; if the worker is online, its current session is closed.
- worker secret is returned only once when creating worker; recovery path is delete + recreate.
- each account can own at most one `worker-sys`.

Defaults:
- HTTP: `:8089`
- gRPC: `:50051`
- Heartbeat interval: `5s`
- SQLite DB path: `./db/onlyboxes-console.db`
- SQLite busy timeout: `5000ms`
- Task retention: `30 days`
- Registration enabled: `false` (`CONSOLE_ENABLE_REGISTRATION`)
- Public preview proxy enabled: `false` (`CONSOLE_PROXY_ENABLED`)
- Public preview route TTL: `86400s`

Dashboard account behavior:
- dashboard accounts are persisted in SQLite table `accounts`.
- account password is hashed with `bcrypt` before persistence (no plaintext storage).
- initial admin username env: `CONSOLE_DASHBOARD_USERNAME`
- initial admin password env: `CONSOLE_DASHBOARD_PASSWORD`
- initial admin API key bootstrap env: `CONSOLE_INITIAL_ADMIN_API_KEY`
- if no account exists at startup, console initializes one admin account from env (missing values are randomly generated).
- if `CONSOLE_INITIAL_ADMIN_API_KEY` is non-empty on first initialization, console creates one dashboard API key for the first admin account.
- the initial admin API key name is fixed to `initial-admin`; its plaintext value is exactly the env value provided via `CONSOLE_INITIAL_ADMIN_API_KEY`.
- if account already exists, the above env credentials are ignored.
- if account already exists, `CONSOLE_INITIAL_ADMIN_API_KEY` is also ignored; startup never backfills an initial API key for persisted accounts.
- initial admin plaintext password is logged only when initialized for the first time.
- if initialized during startup, the initial admin API key plaintext is logged only when `console admin account initialized` is emitted for the first time.
- dashboard session is in-memory only; restarting `console` invalidates all dashboard login sessions.
- changing account password rotates (invalidates + recreates) current account sessions.
- admin can create non-admin accounts via `POST /api/v1/console/register` when `CONSOLE_ENABLE_REGISTRATION=true`.
- admin can list all accounts and delete non-admin accounts; deleting self/admin accounts is blocked.
- dashboard API keys are persisted in SQLite table `api_keys`.
- API key value is stored as HMAC-SHA256 hash only; plaintext is returned once at creation time.
- API keys are bound to `account_id`.
- API key metadata includes `name` (case-insensitive unique within the same account) and masked key (`key_masked`).

Trusted token behavior:
- tokens are persisted in SQLite and managed by dashboard APIs.
- token value is stored as HMAC-SHA256 hash only; plaintext is returned once at creation time.
- tokens are bound to `account_id`.
- token metadata includes `name` (case-insensitive unique within the same account) and masked token (`token_masked`).
- if token list is empty, trusted-token auth for MCP and execution APIs is effectively disabled (`401`); configured JIT bearer tokens can still authenticate.
- task and terminal-session ownership is account-scoped.
- same-account tokens share task/session resources; cross-account access returns `task not found` / `session_not_found`.
- `request_id` idempotency keys are account-scoped.

JIT token behavior:
- JIT tokens are an alternative bearer credential for MCP and execution APIs; they do not need an entry in `trusted_tokens`.
- token format is `obx_jit_v1.<payload>.<signature>`, where `<signature>` is HMAC-SHA256 over `obx_jit_v1.<payload>` using `CONSOLE_JIT_SIGNING_KEY`.
- payload JSON currently requires `iss` and `sub`.
- a valid JIT token deterministically derives an account-scoped owner identity from `iss` + `sub`.
- on first use, the derived account is auto-created as a non-admin account with disabled dashboard credentials and reused on later requests.
- JIT-created accounts own execution resources but cannot log in through dashboard password authentication.
- dashboard routes under `/api/v1/console/*` do not accept JIT tokens as session or API key credentials.
- `CONSOLE_JIT_SIGNING_KEY` should be treated as a high-privilege signing secret: its holder can mint bearer tokens for any `iss`/`sub` identity.
- MCP JIT and Dashboard JIT payloads may include `exp` in Unix milliseconds; expired tokens are rejected.
- Dashboard JIT tokens use sibling format `obx_dashboard_jit_v1.<payload>.<signature>`, are signed with `CONSOLE_DASHBOARD_JIT_SIGNING_KEY`, and additionally require `scope:"dashboard"`.
- `CONSOLE_DASHBOARD_JIT_SIGNING_KEY` must differ from `CONSOLE_JIT_SIGNING_KEY`.
- Dashboard JIT tokens are for dashboard automation such as worker-sys provisioning; they are rejected by `/mcp` and cannot access cookie-only token management endpoints.

Task persistence behavior:
- task input/result/status lifecycle is persisted in SQLite.
- startup recovery marks all non-terminal tasks as `failed` with `error_code=console_restarted`.
- non-expired terminal tasks are retained for `CONSOLE_TASK_RETENTION_DAYS` (default `30`) and cleaned by periodic pruner.
- internal terminal capacity retries keep one task/request identity; `command_id` is updated to the current or last worker attempt.
- terminal session capacity snapshots are connection-local and are not persisted to SQLite; reconnect Hello initializes a fresh snapshot.

Persistence config:
- `CONSOLE_DB_PATH`: SQLite file path (default `./db/onlyboxes-console.db`)
- `CONSOLE_DB_BUSY_TIMEOUT_MS`: SQLite busy timeout in milliseconds (default `5000`)
- `CONSOLE_TASK_RETENTION_DAYS`: terminal task retention days (default `30`)
- `CONSOLE_HASH_KEY`: required HMAC key for hashing worker secret and trusted token; missing value fails startup
- `CONSOLE_JIT_SIGNING_KEY`: optional HMAC key for JIT bearer tokens; when configured, valid JIT tokens can authenticate MCP and execution APIs without a `trusted_tokens` entry
- `CONSOLE_DASHBOARD_JIT_SIGNING_KEY`: optional HMAC key for dashboard JIT bearer tokens; when configured, valid dashboard JIT tokens can authenticate selected dashboard APIs without a cookie session or console API key
- `CONSOLE_MCP_TOKEN_QUERY_PARAM`: query parameter name for `/mcp` URL token fallback (default `token`)
- `CONSOLE_PROXY_ENABLED`: enables route management and Nginx resolve endpoints (default `false`)
- `CONSOLE_PROXY_PUBLIC_BASE_DOMAIN`: wildcard preview base domain
- `CONSOLE_PROXY_PUBLIC_SCHEME`: route URL scheme, `http` or `https` (default `https`)
- `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN`: Nginx-to-Console shared secret
- `CONSOLE_PROXY_ALLOWED_WORKER_CIDRS`: CIDR allowlist for advertised Worker proxy IPs
- `CONSOLE_PROXY_ALLOWED_WORKER_PORTS`: port allowlist for advertised Worker proxy endpoints (default `8091`)
- `CONSOLE_PROXY_ALLOWED_DIRECT_DOMAINS`: domain suffix allowlist for E2B direct origins (default `e2b.app`)
- `CONSOLE_PROXY_ROUTE_TTL_SEC`: in-memory preview route TTL (default `86400`, maximum `604800`)

Logging config:
- `CONSOLE_LOG_LEVEL`: `debug|info|warn|error` (default `info`)
- `CONSOLE_LOG_FORMAT`: `json|text` (default `json`)
- `CONSOLE_LOG_ADD_SOURCE`: include source file/line in logs (default `false`)

MCP minimal call sequence (initialize + tools/list + tools/call):

```bash
curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer <access-token>" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual-client","version":"0.1.0"}}}'

curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer <access-token>" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'

curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer <access-token>" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pythonExec","arguments":{"code":"print(1)"}}}'
```

For MCP clients that only accept a server URL and cannot send custom headers, put the same access token in the MCP endpoint URL:

```bash
curl -X POST "http://127.0.0.1:8089/mcp?token=<access-token>" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```
