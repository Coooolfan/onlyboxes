# Console Overview

The console service hosts:
- a gRPC registry endpoint with bidirectional stream `Connect` for worker registration + heartbeat + command dispatch/result.
- embedded web dashboard static hosting:
  - `GET /` serves embedded `web` frontend.
  - `GET /assets/*` serves bundled static assets.
  - non-API `GET/HEAD` routes use SPA fallback (`index.html`).
  - `/api/*` and `/mcp` are excluded from SPA fallback.
- REST APIs for worker data (dashboard, authentication required):
  - `GET /api/v1/workers` for paginated worker listing.
  - `GET /api/v1/workers/stats` for aggregated worker status metrics.
  - `POST /api/v1/workers` for creating a provisioned worker (`worker_id` + `worker_secret`) and returning its startup command.
  - `DELETE /api/v1/workers/:node_id` for deleting a provisioned worker and revoking its credential (online worker is disconnected immediately).
  - `GET /api/v1/workers/:node_id/startup-command` returns `410 Gone` (worker secret is no longer retrievable after creation; delete and recreate worker to get a new command).
- command APIs (execution, token whitelist required):
  - `POST /api/v1/commands/echo` for blocking echo command execution.
  - `POST /api/v1/commands/terminal` for blocking terminal command execution over `terminalExec` capability.
  - `POST /api/v1/tasks` for sync/async/auto task submission.
  - `GET /api/v1/tasks/:task_id` for task status and result lookup.
  - `POST /api/v1/tasks/:task_id/cancel` for best-effort task cancellation.
  - request header: `X-Onlyboxes-Token: <token>` (must be in whitelist).
  - token isolation: each token is treated as an isolated user boundary for task/session resources.
  - task visibility: task lookup/cancel is owner-scoped by token; cross-token access returns `404`.
  - task idempotency: `request_id` de-duplication is scoped per token (same `request_id` across different tokens does not conflict).
- MCP Streamable HTTP API (token whitelist required):
  - `POST /mcp` for JSON-RPC requests over Streamable HTTP transport.
  - request header: `X-Onlyboxes-Token: <token>` (must be in whitelist).
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
      - session isolation: returned `session_id` can only be reused by the same token; cross-token use returns `session_not_found`.
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
  - `POST /api/v1/console/logout`.
  - token management (requires dashboard auth):
    - `GET /api/v1/console/tokens` list token metadata (`id`, `name`, masked token).
    - `POST /api/v1/console/tokens` create token (manual token or auto-generated, plaintext returned only in create response).
    - `GET /api/v1/console/tokens/:token_id/value` returns `410 Gone` (token plaintext is no longer retrievable after creation).
    - `DELETE /api/v1/console/tokens/:token_id` delete token.

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
- SQLite DB path: `./onlyboxes-console.db`
- SQLite busy timeout: `5000ms`
- Task retention: `30 days`

Dashboard credential behavior:
- dashboard username/password are persisted in SQLite table `dashboard_credentials`.
- dashboard password is hashed with `bcrypt` before persistence (no plaintext storage).
- username env: `CONSOLE_DASHBOARD_USERNAME`
- password env: `CONSOLE_DASHBOARD_PASSWORD`
- if persisted dashboard credential exists, env username/password are ignored.
- if no persisted dashboard credential exists, startup resolves credentials from env; missing values are randomly generated.
- password plaintext is logged only when dashboard credential is initialized for the first time.
- on subsequent restarts, startup logs username only and does not reprint password.
- dashboard session is in-memory only; restarting `console` invalidates all dashboard login sessions.
- recovery path for lost dashboard password:
  - stop console
  - run SQL: `DELETE FROM dashboard_credentials WHERE singleton_id = 1;`
  - restart console to reinitialize credential and print password once

Trusted token behavior:
- tokens are persisted in SQLite and managed by dashboard APIs.
- token value is stored as HMAC-SHA256 hash only; plaintext is returned once at creation time.
- token metadata includes `name` (case-insensitive unique) and masked token (`token_masked`).
- if token list is empty, MCP and execution APIs are effectively disabled (`401`).
- every accepted token is treated as a distinct user for task and terminal-session ownership.
- task and session resources are isolated across tokens (`task not found` / `session_not_found` on cross-token access).
- `request_id` idempotency keys are isolated per token.

Task persistence behavior:
- task input/result/status lifecycle is persisted in SQLite.
- startup recovery marks all non-terminal tasks as `failed` with `error_code=console_restarted`.
- non-expired terminal tasks are retained for `CONSOLE_TASK_RETENTION_DAYS` (default `30`) and cleaned by periodic pruner.

Persistence config:
- `CONSOLE_DB_PATH`: SQLite file path (default `./onlyboxes-console.db`)
- `CONSOLE_DB_BUSY_TIMEOUT_MS`: SQLite busy timeout in milliseconds (default `5000`)
- `CONSOLE_TASK_RETENTION_DAYS`: terminal task retention days (default `30`)
- `CONSOLE_HASH_KEY`: required HMAC key for hashing worker secret and trusted token; missing value fails startup

MCP minimal call sequence (initialize + tools/list + tools/call):

```bash
curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "X-Onlyboxes-Token: <trusted_token>" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual-client","version":"0.1.0"}}}'

curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "X-Onlyboxes-Token: <trusted_token>" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'

curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "X-Onlyboxes-Token: <trusted_token>" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pythonExec","arguments":{"code":"print(1)"}}}'
```
