# Console Overview

The console service hosts:
- a gRPC registry endpoint with bidirectional stream `Connect` for worker registration + heartbeat + command dispatch/result.
- embedded web dashboard static hosting:
  - `GET /` serves embedded `web` frontend.
  - `GET /assets/*` serves bundled static assets.
  - unknown `GET/HEAD` routes return `404 Not Found`.
  - `/api/*` and `/mcp` are reserved for backend handlers and are not served as frontend pages.
- REST APIs for worker data (dashboard authentication + admin required):
  - `GET /api/v1/workers` for paginated worker listing.
  - `GET /api/v1/workers/stats` for aggregated worker status metrics.
  - `POST /api/v1/workers` for creating a provisioned worker (`worker_id` + `worker_secret`) and returning its startup command.
  - `DELETE /api/v1/workers/:node_id` for deleting a provisioned worker and revoking its credential (online worker is disconnected immediately).
  - `GET /api/v1/workers/:node_id/startup-command` always returns `410 Gone`.
  - `worker_secret` is returned once in `POST /api/v1/workers` response and is not queryable from read APIs.
- command APIs (execution, token whitelist required):
  - `POST /api/v1/commands/echo` for blocking echo command execution.
  - `POST /api/v1/commands/terminal` for blocking terminal command execution over `terminalExec` capability.
  - `POST /api/v1/tasks` for sync/async/auto task submission.
  - `GET /api/v1/tasks/:task_id` for task status and result lookup.
  - `POST /api/v1/tasks/:task_id/cancel` for best-effort task cancellation.
  - request header: `Authorization: Bearer <access-token>` (must be in whitelist).
  - owner isolation is account-scoped: token resolves to `account_id`, and task/session ownership uses `account_id`.
  - task visibility: task lookup/cancel is owner-scoped by account; same-account tokens can access shared tasks, cross-account access returns `404`.
  - task idempotency: `request_id` de-duplication is scoped per account.
- MCP Streamable HTTP API (token whitelist required):
  - `POST /mcp` for JSON-RPC requests over Streamable HTTP transport.
  - request header: `Authorization: Bearer <access-token>` (must be in whitelist).
  - if whitelist is empty (no tokens configured in dashboard), all `/mcp` requests are rejected with `401`.
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
    - `readImage`
      - input: `{"session_id":"required","file_path":"required","timeout_ms":60000}`
      - `session_id` and `file_path` are required (whitespace-only is rejected).
      - validates file existence via worker `terminalResource` capability; directories are rejected.
      - output is content-only (no structured output fields).
      - image files (`image/*`) return exactly one `image` content item.
      - non-image files return exactly one `text` content item:
        - `unsupported mime type: <mime>; expected image/*`
      - non-format failures (session/file missing, busy, timeout, read failure) are returned as tool errors.
- dashboard authentication APIs:
  - `POST /api/v1/console/login` with `{"username":"...","password":"..."}`.
  - login response includes `authenticated`, `account`, `registration_enabled`, `console_version`, `console_repo_url`.
  - `POST /api/v1/console/logout`.
  - `GET /api/v1/console/session` returns current session account payload with `console_version` and `console_repo_url`.
  - `POST /api/v1/console/register` creates non-admin account (admin-only, and only when `CONSOLE_ENABLE_REGISTRATION=true`).
  - token management (requires dashboard auth):
    - `GET /api/v1/console/tokens` list current account token metadata (`id`, `name`, masked token).
    - `POST /api/v1/console/tokens` create token bound to current account (manual token or auto-generated, plaintext returned only in create response).
    - `GET /api/v1/console/tokens/:token_id/value` always returns `410 Gone`.
    - token plaintext is delivered in `POST /api/v1/console/tokens` response only.
    - `DELETE /api/v1/console/tokens/:token_id` delete token (current account only, cross-account returns `404`).

Security warning (high risk):
- worker-to-console gRPC is currently plaintext by default (no TLS/mTLS).
- `worker_secret` is sent in `ConnectHello`; on untrusted networks it can be observed in transit.
- deploy only on trusted private networks or encrypted tunnels; do not expose gRPC directly to the public internet.
- fully mitigating this risk requires TLS/mTLS support (not implemented in this release).

Credential behavior:
- `console` starts with `0` workers.
- worker credentials are generated on demand by dashboard/API `POST /api/v1/workers`.
- credentials are persisted in SQLite as HMAC-SHA256 hashes only (no plaintext storage).
- deleting a provisioned worker revokes the credential immediately; if the worker is online, its current session is closed.
- worker secret is returned only once when creating worker; recovery path is delete + recreate.

Defaults:
- HTTP: `:8089`
- gRPC: `:50051`
- Heartbeat interval: `5s`
- SQLite DB path: `./db/onlyboxes-console.db`
- SQLite busy timeout: `5000ms`
- Task retention: `30 days`
- Registration enabled: `false` (`CONSOLE_ENABLE_REGISTRATION`)

Dashboard account behavior:
- dashboard accounts are persisted in SQLite table `accounts`.
- account password is hashed with `bcrypt` before persistence (no plaintext storage).
- initial admin username env: `CONSOLE_DASHBOARD_USERNAME`
- initial admin password env: `CONSOLE_DASHBOARD_PASSWORD`
- if no account exists at startup, console initializes one admin account from env (missing values are randomly generated).
- if account already exists, the above env credentials are ignored.
- initial admin plaintext password is logged only when initialized for the first time.
- dashboard session is in-memory only; restarting `console` invalidates all dashboard login sessions.
- admin can create non-admin accounts via `POST /api/v1/console/register` when `CONSOLE_ENABLE_REGISTRATION=true`.

Trusted token behavior:
- tokens are persisted in SQLite and managed by dashboard APIs.
- token value is stored as HMAC-SHA256 hash only; plaintext is returned once at creation time.
- tokens are bound to `account_id`.
- token metadata includes `name` (case-insensitive unique within the same account) and masked token (`token_masked`).
- if token list is empty, MCP and execution APIs are effectively disabled (`401`).
- task and terminal-session ownership is account-scoped.
- same-account tokens share task/session resources; cross-account access returns `task not found` / `session_not_found`.
- `request_id` idempotency keys are account-scoped.

Task persistence behavior:
- task input/result/status lifecycle is persisted in SQLite.
- startup recovery marks all non-terminal tasks as `failed` with `error_code=console_restarted`.
- non-expired terminal tasks are retained for `CONSOLE_TASK_RETENTION_DAYS` (default `30`) and cleaned by periodic pruner.

Persistence config:
- `CONSOLE_DB_PATH`: SQLite file path (default `./db/onlyboxes-console.db`)
- `CONSOLE_DB_BUSY_TIMEOUT_MS`: SQLite busy timeout in milliseconds (default `5000`)
- `CONSOLE_TASK_RETENTION_DAYS`: terminal task retention days (default `30`)
- `CONSOLE_HASH_KEY`: required HMAC key for hashing worker secret and trusted token; missing value fails startup

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
