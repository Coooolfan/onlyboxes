# Onlyboxes API Reference

[简体中文](API.zh-CN.md)

This document is the unified reference for all public APIs exposed by Onlyboxes.

- Console HTTP base URL: `http://<console-host>:8089`
- Console worker gRPC endpoint: `<console-host>:50051`
- API version prefix: `/api/v1`

## 1. Authentication Model

Onlyboxes has these auth paths:

1. Dashboard session (cookie): for web/admin APIs
2. Dashboard bearer credentials: for selected dashboard automation APIs
3. Access token (Bearer): for execution APIs and MCP

### 1.1 Dashboard Session (Cookie)

- Cookie name: `onlyboxes_console_session`
- Created by: `POST /api/v1/console/login`
- Used by:
  - `/api/v1/console/session`
  - `/api/v1/console/logout`
  - `/api/v1/console/password`
  - `/api/v1/console/register`
  - `/api/v1/console/accounts*`
  - `/api/v1/console/tokens*`
  - `/api/v1/workers*` (role-scoped worker routes)
  - `/api/v1/proxy-routes*` (account-scoped public preview routes)
- Session TTL is 12 hours in-memory; console restart invalidates all sessions.

### 1.2 Access Token (Bearer)

- Header format: `Authorization: Bearer <access-token>`
- Used by:
  - `/api/v1/commands/*`
  - `/api/v1/tasks*`
  - `/mcp`
- Accepted token types:
  - trusted token managed by cookie-authenticated dashboard token APIs
  - MCP JIT token (`obx_jit_v1.<payload>.<signature>`) when `CONSOLE_JIT_SIGNING_KEY` is configured
- If no trusted token exists in console, trusted-token auth returns `401`; valid MCP JIT tokens can still authenticate when configured.

### 1.3 Dashboard Bearer Credentials

- Header format: `Authorization: Bearer <credential>`
- Console API keys can authenticate dashboard APIs, except endpoints that require a cookie session.
- Dashboard JIT tokens can authenticate selected dashboard APIs for server-to-server worker-sys provisioning:
  - Format: `obx_dashboard_jit_v1.<payload>.<signature>`
  - Signature: HMAC-SHA256 over `obx_dashboard_jit_v1.<payload>` with `CONSOLE_DASHBOARD_JIT_SIGNING_KEY`
  - Payload requires `iss`, `sub`, `scope:"dashboard"`, and optional `exp` in Unix milliseconds.
  - `CONSOLE_DASHBOARD_JIT_SIGNING_KEY` must differ from `CONSOLE_JIT_SIGNING_KEY`.
- Dashboard JIT uses the same `(iss, sub) -> account` derivation as MCP JIT, creates a non-admin account on first use, and cannot authenticate `/mcp`.
- MCP JIT payloads also accept optional `exp` in Unix milliseconds.
- MCP JIT tokens (`obx_jit_v1.*`) are rejected by dashboard auth.
- Cookie-only endpoints reject dashboard bearer credentials.

## 2. Common REST Conventions

- Content type: `application/json`
- Error body (most APIs):

```json
{ "error": "message" }
```

- Time fields are RFC3339 timestamps.
- IDs are opaque strings (for example `acc_*`, `tok_*`, worker UUIDs, task IDs).

## 3. Dashboard Authentication APIs

### 3.1 Login

`POST /api/v1/console/login`

Request:

```json
{
  "username": "admin",
  "password": "secret"
}
```

Success `200`:

```json
{
  "authenticated": true,
  "account": {
    "account_id": "acc_xxx",
    "username": "admin",
    "is_admin": true
  },
  "registration_enabled": false,
  "console_version": "v0.0.0",
  "console_repo_url": "https://..."
}
```

Errors:

- `400` invalid JSON body
- `401` invalid username/password
- `500` session creation failure

### 3.2 Session Info

`GET /api/v1/console/session`

- Requires dashboard cookie auth.
- Success body format is same as login response.

Errors:

- `401` not authenticated

### 3.3 Logout

`POST /api/v1/console/logout`

- Clears cookie and removes in-memory session if present.

Response:

- `204 No Content`

### 3.4 Register Non-Admin Account (Admin Only)

`POST /api/v1/console/register`

Request:

```json
{
  "username": "dev-user",
  "password": "strong-password"
}
```

Success `201`:

```json
{
  "account": {
    "account_id": "acc_xxx",
    "username": "dev-user",
    "is_admin": false
  },
  "created_at": "2026-02-21T00:00:00Z",
  "updated_at": "2026-02-21T00:00:00Z"
}
```

Validation and errors:

- `403` registration disabled (`CONSOLE_ENABLE_REGISTRATION=false`)
- `403` caller is not admin
- `400` username empty, username length > 64, or password empty
- `409` username already exists (case-insensitive)
- `500` database/internal failure

### 3.5 Change Current Account Password

`POST /api/v1/console/password`

Request:

```json
{
  "current_password": "old-password",
  "new_password": "new-password"
}
```

Responses:

- `204` password updated
- `400` invalid JSON body, missing `current_password`, or missing `new_password`
- `401` current password is incorrect
- `500` internal failure

Notes:

- This endpoint requires dashboard session auth.
- Password update rotates active sessions for the account.

### 3.6 List Accounts (Admin Only)

`GET /api/v1/console/accounts?page=1&page_size=20`

Query:

- `page`: positive integer, default `1`
- `page_size`: positive integer, default `20`, max `100`

Success `200`:

```json
{
  "items": [
    {
      "account_id": "acc_xxx",
      "username": "admin",
      "is_admin": true,
      "created_at": "2026-02-21T00:00:00Z",
      "updated_at": "2026-02-21T00:00:00Z"
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 20
}
```

Errors:

- `400` invalid query values
- `403` caller is not admin
- `500` database/internal failure

### 3.7 Delete Account (Admin Only)

`DELETE /api/v1/console/accounts/:account_id`

Responses:

- `204` deleted
- `403` deleting current account is forbidden
- `403` deleting admin account is forbidden
- `404` account not found
- `500` internal failure

## 4. Token Management APIs (Dashboard Cookie Session Auth)

Tokens are account-scoped. A user can manage only their own tokens.

All endpoints in this section require a dashboard cookie session. Console API keys and dashboard JIT tokens are intentionally rejected so dashboard bearer credentials cannot mint or manage MCP trusted tokens.

### 4.1 List Tokens

`GET /api/v1/console/tokens`

Success `200`:

```json
{
  "items": [
    {
      "id": "tok_xxx",
      "name": "default-token",
      "token_masked": "obx_******abcd",
      "created_at": "2026-02-21T00:00:00Z",
      "updated_at": "2026-02-21T00:00:00Z"
    }
  ],
  "total": 1
}
```

### 4.2 Create Token

`POST /api/v1/console/tokens`

Request:

```json
{
  "name": "ci-prod",
  "token": "optional-manual-token"
}
```

- `name` required, trimmed, length <= 64, unique per account (case-insensitive)
- `token` optional:
  - omitted => auto-generate (`obx_<hex>`)
  - provided => required non-empty after trim, no whitespace, max length 256

Success `201`:

```json
{
  "id": "tok_xxx",
  "name": "ci-prod",
  "token": "obx_plaintext_or_manual",
  "token_masked": "obx_******abcd",
  "generated": true,
  "created_at": "2026-02-21T00:00:00Z",
  "updated_at": "2026-02-21T00:00:00Z"
}
```

Errors:

- `400` validation error
- `409` token name conflict or token value conflict
- `500` internal failure

### 4.3 Delete Token

`DELETE /api/v1/console/tokens/:token_id`

Responses:

- `204` deleted
- `404` token not found (or not owned by current account)

### 4.4 Get Token Value

`GET /api/v1/console/tokens/:token_id/value`

Always returns `410 Gone`:

```json
{
  "error": "token value is only returned at creation time; delete and recreate the token to obtain a new value"
}
```

## 5. Worker Management APIs (Dashboard Auth, Role-Scoped)

Worker types:

- `normal` (maps to `worker-docker`)
- `worker-sys` (maps to `worker-sys`)

Permission matrix:

- admin:
  - list/stats/inflight: all workers
  - delete: any worker
  - create: `normal` and `worker-sys`
- non-admin:
  - list/stats/inflight: only own `worker-sys`
  - delete: only own `worker-sys` (other targets return `404`)
  - create: only `worker-sys`, max one per account

### 5.1 List Workers

`GET /api/v1/workers?page=1&page_size=20&status=all`

Query:

- `page`: positive integer, default `1`
- `page_size`: positive integer, default `20`, max `100`
- `status`: `all|online|offline`, default `all`

Success `200`:

```json
{
  "items": [
    {
      "node_id": "worker-1",
      "node_name": "node-a",
      "executor_kind": "docker",
      "capabilities": [
        { "name": "echo", "max_inflight": 4 }
      ],
      "labels": {
        "region": "us",
        "obx.owner_id": "acc_xxx",
        "obx.worker_type": "normal"
      },
      "version": "v0.1.0",
      "status": "online",
      "registered_at": "2026-02-21T00:00:00Z",
      "last_seen_at": "2026-02-21T00:00:00Z"
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 20
}
```

Errors:

- `400` invalid query values

### 5.2 Worker Stats

`GET /api/v1/workers/stats?stale_after_sec=30`

Query:

- `stale_after_sec`: positive integer, default `30`

Success `200`:

```json
{
  "total": 5,
  "online": 4,
  "offline": 1,
  "stale": 1,
  "stale_after_sec": 30,
  "generated_at": "2026-02-21T00:00:00Z"
}
```

Note: non-admin responses are scoped to the caller-owned `worker-sys`.

### 5.3 Worker Inflight Stats

`GET /api/v1/workers/inflight`

Success `200`:

```json
{
  "workers": [
    {
      "node_id": "worker-1",
      "active_session_count": 3,
      "terminal_session_capacity": {
        "known": true,
        "max_active_sessions": 4
      },
      "capabilities": [
        { "name": "pythonExec", "inflight": 1, "max_inflight": 4 }
      ]
    }
  ],
  "generated_at": "2026-02-21T00:00:00Z"
}
```

Notes:

- non-admin responses are scoped to the caller-owned `worker-sys`.
- `active_session_count` is the worker's latest terminal reservation count.
- `terminal_session_capacity.known=false` means a legacy worker did not declare a maximum; it must not be interpreted as unlimited.
- `known=true,max_active_sessions=0` explicitly means unlimited active terminal sessions.

### 5.4 Create Worker Credential

`POST /api/v1/workers`

Request body:

```json
{
  "type": "normal"
}
```

Rules:

- `type` is required, value must be `normal|worker-sys`.
- only admin can create `normal`.
- every account can create at most one `worker-sys`.

Success `201`:

```json
{
  "node_id": "2f51f8f9-77f2-4c1a-a4f5-2036fc9fcb9e",
  "type": "normal",
  "command": "WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=... WORKER_SECRET=... WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 ./path-to-binary"
}
```

Notes:

- `WORKER_SECRET` appears only here (one-time return).
- `./path-to-binary` is a placeholder and must be replaced by your real worker executable command.

Errors:

- `400` invalid request body / invalid `type`
- `403` non-admin creating `normal`
- `409` caller already owns a `worker-sys`
- `503` provisioning unavailable
- `500` create failure

### 5.5 Delete Worker

`DELETE /api/v1/workers/:node_id`

Responses:

- `204` deleted
- `404` worker not found (also returned for unauthorized non-admin targets)
- `400` missing `node_id`
- `503` provisioning unavailable

### 5.6 Get Startup Command

`GET /api/v1/workers/:node_id/startup-command`

Always returns `410 Gone`:

```json
{
  "error": "worker secret is returned only when creating the worker; delete and recreate to get a new startup command"
}
```

### 5.7 Public Preview Routes

These management APIs accept Dashboard cookie, Console API key, or Dashboard JIT authentication. Routes are account-scoped and persisted in SQLite.

`POST /api/v1/proxy-routes`

```json
{
  "session_id": "sess_xxx",
  "port": 8080
}
```

Success `201`:

```json
{
  "route_key": "ceirceirceirceirceirceirce",
  "session_id": "sess_xxx",
  "port": 8080,
  "url": "https://ceirceirceirceirceirceirce.public-preview.example.com",
  "created_at": "2026-02-21T00:00:00Z",
  "expires_at": "2026-02-22T00:00:00Z"
}
```

Rules and errors:

- `session_id` must belong to the current account and have a confirmed route to an online proxy-enabled Docker, Boxlite, or E2B Worker.
- `port` must be `1..65535`.
- route URLs use `CONSOLE_PROXY_PUBLIC_SCHEME` (`https` by default) and `CONSOLE_PROXY_PUBLIC_BASE_DOMAIN`; use `http` only for trusted local development.
- route keys use lowercase Base32 and default to 26 characters. `CONSOLE_PROXY_ROUTE_KEY_LENGTH` controls the length of newly generated keys in the range `8..26`; existing routes remain valid after it changes. Values below `16` reduce resistance to URL guessing and are intended only for trusted local or low-risk deployments.
- each account can hold at most 16 active routes by default; configure it with `CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT`.
- each session can hold at most 2 active routes by default; configure it with `CONSOLE_PROXY_ROUTE_MAX_PER_SESSION`.
- default route TTL is `86400` seconds, can be changed with `CONSOLE_PROXY_ROUTE_TTL_SEC`, and cannot exceed `604800` seconds (7 days).
- `400` invalid body, session ID, or port.
- `404` session route not found.
- `429` account or session route limit reached.
- `503` the session's Worker has no available proxy endpoint.

`GET /api/v1/proxy-routes`

Success `200` returns the current account's routes:

```json
{
  "items": [],
  "total": 0
}
```

`DELETE /api/v1/proxy-routes/:route_key`

- `204` deleted.
- `404` missing, expired, or owned by another account.

The returned preview URL is anonymous: anyone holding it can access the Sandbox service without an Onlyboxes Cookie or Bearer token. Nginx resolves every new HTTP request through the protected internal Console endpoint. Docker and Boxlite receive a fresh 15-second Route Token for the Worker hop; E2B returns the current sandbox origin and an internal traffic token so the data path is browser → Nginx → E2B. Token expiry does not end an accepted HTTP/SSE/WebSocket connection. Route access does not renew the Sandbox lease. Active routes and their original URLs survive a Console restart; new requests remain unavailable until the owning Worker reconnects and the terminal session finishes recovery.

## 6. Execution Command APIs (Bearer Token)

### 6.1 Echo Command

`POST /api/v1/commands/echo`

Request:

```json
{
  "message": "hello",
  "timeout_ms": 5000
}
```

Rules:

- `message`: required, non-empty after trim
- `timeout_ms`: optional, range `1..60000`, default `5000`

Success `200`:

```json
{ "message": "hello" }
```

Errors:

- `400` invalid body / missing message / timeout out of range
- `429` no worker capacity
- `503` no online worker supports echo
- `504` timeout
- `502` execution/internal error

### 6.2 Terminal Command

`POST /api/v1/commands/terminal`

Request:

```json
{
  "command": "pwd",
  "session_id": "optional-session",
  "create_if_missing": false,
  "lease_ttl_sec": 60,
  "timeout_ms": 60000,
  "request_id": "optional-idempotency-key"
}
```

Rules:

- `command`: required, non-empty
- `timeout_ms`: optional, range `1..600000`, default `60000`
- `request_id`: optional, idempotency key scoped per account

Success `200`:

```json
{
  "session_id": "sess_xxx",
  "created": true,
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false,
  "lease_expires_unix_ms": 1770000000000
}
```

Confirmed terminal session route bindings and absolute leases are persisted in SQLite across Console restarts. A restored route remains pinned to its original Worker and returns `session_unavailable` until that Worker reconnects and completes recovery; it is not reassigned to another Worker.

Errors:

- `400` invalid body/params or `invalid_payload`
- `404` `session_not_found`
- `503` `session_unavailable` while the session's bound worker is offline or reconciling after restart; retry the same `session_id` without rerouting it.
- `409` `session_busy` or canceled
  - `session_busy` means the request exceeded the per-session concurrency limit. Workers default to one command per session; the worker must raise `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` to allow concurrent commands on one `session_id`.
  - `terminalExec` and `terminalResource` share that per-session limit.
- `429` no worker capacity or terminal session capacity
  - `no_capacity` means the worker-level quota for the capability is exhausted, not a single session's.
  - `session_capacity_exceeded` means every dispatchable sandbox worker attempted for this new session rejected creation because it had reached `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS`; existing sessions remain usable on their bound worker.
  - for a console-generated new `session_id`, capacity-aware dispatch prefers workers that report available session capacity, then legacy/unknown workers, and treats reported-full workers as last-resort probes.
  - a worker-side capacity rejection is retried only when the worker explicitly confirms this pre-execution error and the provisional route can be removed safely; transport, timeout, and other execution errors are not retried.
  - these are separate from `session_busy`, which means the per-session command limit was reached.
- `503` no compatible worker
- `504` timeout
- `502` unexpected execution failure

### 6.3 Computer Use Command

`POST /api/v1/commands/computer-use`

Request:

```json
{
  "command": "pwd",
  "timeout_ms": 60000,
  "request_id": "optional-idempotency-key"
}
```

Rules:

- `command`: required, non-empty
- `timeout_ms`: optional, range `1..600000`, default `60000`
- `request_id`: optional, idempotency key scoped per account
- `lease_ttl_sec` is ignored if provided by legacy clients
- routing is account-scoped: requests are dispatched only to caller-owned `worker-sys`
- account-scoped concurrency is single-flight (`max_inflight=1`)

Success `200`:

```json
{
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false
}
```

Errors:

- `400` invalid body/params or `invalid_payload`
- `409` worker `session_busy` or task canceled
  - `session_busy` means the request exceeded the worker's per-capability concurrency limit, which defaults to `1` (`WORKER_COMPUTER_USE_MAX_INFLIGHT`).
- `429` no worker capacity (`no_capacity`)
- `503` no caller-owned online `worker-sys` (`no_worker`)
- `504` timeout
- `502` unexpected execution failure

## 7. Sandbox Metadata API (Bearer Token)

This endpoint requires an execution token. Trusted tokens and MCP JIT tokens can call it. Account-scoped fields are limited to caller-owned `worker-sys` capabilities such as `computerUse` and `readImage`; shared sandbox capabilities and the worker summary are reported from the global worker pool.

### 7.1 Get Sandbox Metadata

`GET /api/v1/sandbox/metadata`

Success `200`:

```json
{
  "provider": "onlyboxes",
  "api_version": "2026-05-25",
  "console": { "version": "0.6.1" },
  "limits": {
    "max_task_timeout_ms": 600000,
    "max_task_wait_ms": 60000,
    "max_terminal_timeout_ms": 600000,
    "default_terminal_lease_sec": 60,
    "max_terminal_lease_sec": 86400
  },
  "capabilities": [
    {
      "name": "terminalExec",
      "available": true,
      "online_nodes": 1,
      "max_inflight": 4
    }
  ],
  "workers": {
    "total": 1,
    "online": 1,
    "offline": 0,
    "stale": 0
  }
}
```

Notes:

- `computerUse` and `readImage` availability is scoped to caller-owned `worker-sys`.
- `terminalExec`, `terminalResource`, `pythonExec`, and `echo` report global online worker availability.
- Missing/invalid token returns `401`.

## 8. Task APIs (Bearer Token)

Task ownership is account-scoped by token.

### 8.1 Submit Task

`POST /api/v1/tasks`

Request:

```json
{
  "capability": "pythonExec",
  "input": { "code": "print(1)" },
  "mode": "auto",
  "wait_ms": 1500,
  "timeout_ms": 60000,
  "request_id": "optional-idempotency-key"
}
```

Rules:

- `capability`: required, non-empty
- `input`: must be valid JSON (defaults to `{}` when omitted)
- `mode`: `sync|async|auto`, default `auto`
- `wait_ms`: `1..60000`, default `1500`
- `timeout_ms`: `1..600000`, default `60000`
- `request_id`: optional dedupe key (scoped per account)
- for `terminalResource` export payloads, `input.headers` is filtered before dispatch; only `x-amz-*`, `Content-Type`, and `Content-MD5` upload headers are forwarded to workers.
- one task keeps the same `task_id` and `request_id` across internal terminal capacity retries; `command_id` is the current or last worker dispatch attempt, so it can change while the task is running.

Possible responses:

- `202` task still running (contains `status_url`)
- `200` completed succeeded
- `409` completed canceled
- `504` completed timeout
- `429` completed failed with `error.code=no_capacity` or `error.code=session_capacity_exceeded`
- `503` completed failed with `error.code=no_worker`
- `502` completed failed (other error codes)

`202` example:

```json
{
  "task_id": "task_xxx",
  "request_id": "req-1",
  "command_id": "cmd_xxx",
  "capability": "pythonexec",
  "status": "running",
  "created_at": "2026-02-21T00:00:00Z",
  "updated_at": "2026-02-21T00:00:01Z",
  "deadline_at": "2026-02-21T00:01:00Z",
  "status_url": "/api/v1/tasks/task_xxx"
}
```

Completed example:

```json
{
  "task_id": "task_xxx",
  "capability": "pythonexec",
  "status": "succeeded",
  "result": {
    "output": "1\n",
    "stderr": "",
    "exit_code": 0
  },
  "created_at": "2026-02-21T00:00:00Z",
  "updated_at": "2026-02-21T00:00:01Z",
  "deadline_at": "2026-02-21T00:01:00Z",
  "completed_at": "2026-02-21T00:00:01Z"
}
```

Error body inside task payload:

```json
"error": {
  "code": "execution_failed",
  "message": "..."
}
```

Submit-time errors:

- `400` invalid request/mode/wait/timeout/body
- `409` request_id already in progress
- `429` no worker capacity
- `503` no compatible worker
- `504` deadline exceeded
- `502` submit failure

### 8.2 Get Task

`GET /api/v1/tasks/:task_id`

Responses:

- `200` task snapshot
- `404` task not found (including cross-account access)

### 8.3 Cancel Task

`POST /api/v1/tasks/:task_id/cancel`

Responses:

- `200` canceled (or best-effort cancel accepted)
- `404` task not found (including cross-account access)
- `409` task already terminal (returns task snapshot)
- `500` cancel failure

## 9. MCP API (Bearer Token)

Endpoint: `POST /mcp`

- Transport: MCP Streamable HTTP
- Server mode: stateless JSON response
- `GET /mcp` returns `405` with `Allow: POST`
- Requires `Authorization: Bearer <access-token>`
- Recommended headers:
  - `Content-Type: application/json`
  - `Accept: application/json, text/event-stream`

### 9.1 Core MCP Methods

Supported MCP flow includes standard methods such as:

- `initialize`
- `tools/list`
- `tools/call`

### 9.2 Tool Definitions

Tool argument schemas use `additionalProperties=false`.
Unknown arguments are rejected with JSON-RPC `-32602 invalid params`.

> Tools listed in `CONSOLE_HIDDEN_TOOLS` are omitted from `tools/list`. They remain callable via `tools/call` if the client already knows the tool name.

> Each tool's `name` / `title` / `description` and each parameter's `description` can be overridden at runtime via `CONSOLE_MCP_TOOL_<TOOL>_NAME`, `CONSOLE_MCP_TOOL_<TOOL>_TITLE`, `CONSOLE_MCP_TOOL_<TOOL>_DESCRIPTION`, and `CONSOLE_MCP_TOOL_<TOOL>_PARAM_<PARAM>_DESCRIPTION`. The `_NAME` override changes the value clients see in `tools/list` and use as the `tools/call` routing key (must match `^[a-zA-Z0-9_-]{1,64}$`; conflicts with another tool's built-in name fall back); `CONSOLE_HIDDEN_TOOLS` always uses the internal capability ID, never the renamed value. Setting a parameter description to an empty string removes that parameter from `inputSchema.properties` and `required` (and flips `additionalProperties` to `true` on that schema), while `tools/call` still accepts the field. See the Console Configuration doc for the full `<TOOL>` / `<PARAM>` mapping.

#### Tool: `echo`

Input:

```json
{ "message": "hello", "timeout_ms": 5000 }
```

- `message` required
- `timeout_ms` optional, `1..60000`, default `5000`

Output:

```json
{ "message": "hello" }
```

#### Tool: `pythonExec`

Input:

```json
{ "code": "print(1)", "timeout_ms": 60000 }
```

- `code` required
- `timeout_ms` optional, `1..600000`, default `60000`

Output:

```json
{ "output": "1\n", "stderr": "", "exit_code": 0 }
```

Note: non-zero `exit_code` is returned as normal tool output.

#### Tool: `terminalExec`

Input:

```json
{
  "command": "pwd",
  "session_id": "optional",
  "create_if_missing": false,
  "lease_ttl_sec": 60,
  "timeout_ms": 60000
}
```

- `command` required
- `session_id` optional
- `create_if_missing` optional, default `false`
- `lease_ttl_sec` optional
- `timeout_ms` optional, `1..600000`, default `60000`

Output:

```json
{
  "session_id": "sess_xxx",
  "created": true,
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false,
  "lease_expires_unix_ms": 1770000000000
}
```

#### Tool: `computerUse`

Input:

```json
{
  "command": "pwd",
  "timeout_ms": 60000,
  "request_id": "optional-idempotency-key"
}
```

- `command` required
- `timeout_ms` optional, `1..600000`, default `60000`
- `request_id` optional, idempotency key scoped per account
- routed only to caller-owned `worker-sys`
- no terminal session fields (`session_id`, `create_if_missing`, `created`)

Output:

```json
{
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false
}
```

#### Tool: `readImage`

Input:

```json
{ "session_id": "sess_xxx", "file_path": "/workspace/a.png", "timeout_ms": 60000 }
```

- `session_id` required
- `file_path` required
- `timeout_ms` optional, `1..600000`, default `60000`
- when `session_id` is exactly `computerUse`, routing uses caller-owned `worker-sys` `readImage` capability
- for other `session_id` values, routing uses `terminalResource` capability

Behavior:

- If target MIME is `image/*`: returns one image content item.
- If non-image MIME: returns one text content item:
  - `unsupported mime type: <mime>; expected image/*`

### 9.3 MCP Errors

- Missing/invalid token: HTTP `401`
- Invalid tool params: JSON-RPC error `-32602`
- `computerUse` without a caller-owned `worker-sys`: JSON-RPC error `-32010` with `data.error_code="WORKER_SYS_REQUIRED"`
- `computerUse` with a registered but offline caller-owned `worker-sys`: JSON-RPC error `-32011` with `data.error_code="WORKER_SYS_OFFLINE"`
- Execution failures: returned as MCP tool error content (`isError=true`)

## 10. Worker gRPC API (`api/proto/registry/v1/registry.proto`)

Service:

```proto
service WorkerRegistryService {
  rpc Connect(stream ConnectRequest) returns (stream ConnectResponse);
}
```

### 10.1 Stream Flow

Worker establishes a bidirectional stream and typically sends:

1. `ConnectRequest.hello` (`ConnectHello`)
2. Periodic `ConnectRequest.heartbeat` (`HeartbeatFrame`)
3. `ConnectRequest.command_result` (`CommandResult`) for dispatched commands

Console responds with:

1. `ConnectResponse.connect_ack` (`ConnectAck`)
2. `ConnectResponse.heartbeat_ack` (`HeartbeatAck`)
3. `ConnectResponse.command_dispatch` (`CommandDispatch`)

### 10.2 Key Messages

- `ConnectHello` includes worker identity, capabilities, labels, version, `worker_secret`, and optional `terminal_session_capacity`.
  - a missing `terminal_session_capacity` means legacy/unknown capacity.
  - a present declaration carries `max_active_sessions` (`0` means unlimited) and the reservation count in `active_session_count` at Hello construction time.
  - sandbox workers send the declaration on every connection, including reconnects; `worker-sys` does not declare terminal capacity.
- `HeartbeatFrame.active_session_count` refreshes the reservation count after Hello. Negative Hello or heartbeat capacity values are rejected.
- `CommandDispatch` carries:
  - `command_id`
  - `capability`
  - `payload_json`
  - `deadline_unix_ms`
- `CommandResult` carries:
  - `command_id`
  - optional `error { code, message }`
  - `payload_json`
  - `completed_unix_ms`

## 11. Security Notes

- Console gRPC has no built-in TLS/mTLS in this release.
- `worker-docker` rejects insecure console endpoints by default, and allows plaintext only when `WORKER_CONSOLE_INSECURE=true`.
- `worker-sys` executes `computerUse` directly on host shell (`/bin/sh -lc`) without container isolation.
- `worker-sys` `readImage` reads host files directly and accepts only `session_id=computerUse`.
- deploy `worker-sys` only on dedicated hosts with strict OS-level access controls.
- Put console HTTP (`:8089`) and gRPC (`:50051`) behind a reverse proxy/gateway and enforce TLS for external access.
- Keep gRPC endpoint private and tunnel/encrypt traffic in production.
- Public preview requires wildcard DNS/TLS, the deployment guide in `docs/nginx/README.md`, the Nginx configuration in `docs/nginx/public-preview.conf.example`, and network ACLs that allow only Nginx to reach Worker proxy ports.
- Keep `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN` secret and restrict `CONSOLE_PROXY_ALLOWED_WORKER_CIDRS` / `CONSOLE_PROXY_ALLOWED_WORKER_PORTS` to actual Worker ingress endpoints.
- Restrict `CONSOLE_PROXY_ALLOWED_DIRECT_DOMAINS` to the E2B domains used by the deployment (default `e2b.app`).
- Token plaintext and `WORKER_SECRET` are one-time return values.
- `GET /api/v1/console/tokens/:token_id/value` and `GET /api/v1/workers/:node_id/startup-command` are intentionally `410 Gone`.
