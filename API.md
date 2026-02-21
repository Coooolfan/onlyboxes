# Onlyboxes API Reference

[简体中文](API.zh-CN.md)

This document is the unified reference for all public APIs exposed by Onlyboxes.

- Console HTTP base URL: `http://<console-host>:8089`
- Console worker gRPC endpoint: `<console-host>:50051`
- API version prefix: `/api/v1`

## 1. Authentication Model

Onlyboxes has two auth paths:

1. Dashboard session (cookie): for web/admin APIs
2. Access token (Bearer): for execution APIs and MCP

### 1.1 Dashboard Session (Cookie)

- Cookie name: `onlyboxes_console_session`
- Created by: `POST /api/v1/console/login`
- Used by:
  - `/api/v1/console/session`
  - `/api/v1/console/logout`
  - `/api/v1/console/register`
  - `/api/v1/console/tokens*`
  - `/api/v1/workers*` (admin routes)
- Session TTL is 12 hours in-memory; console restart invalidates all sessions.

### 1.2 Access Token (Bearer)

- Header format: `Authorization: Bearer <access-token>`
- Used by:
  - `/api/v1/commands/*`
  - `/api/v1/tasks*`
  - `/mcp`
- If no token exists in console, token-protected APIs return `401`.

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

## 4. Token Management APIs (Dashboard Auth)

Tokens are account-scoped. A user can manage only their own tokens.

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

## 5. Worker Management APIs (Admin Dashboard Auth)

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
      "labels": { "region": "us" },
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

### 5.3 Worker Inflight Stats

`GET /api/v1/workers/inflight`

Success `200`:

```json
{
  "workers": [
    {
      "node_id": "worker-1",
      "capabilities": [
        { "name": "pythonExec", "inflight": 1, "max_inflight": 4 }
      ]
    }
  ],
  "generated_at": "2026-02-21T00:00:00Z"
}
```

### 5.4 Create Worker Credential

`POST /api/v1/workers`

Request body: none.

Success `201`:

```json
{
  "node_id": "2f51f8f9-77f2-4c1a-a4f5-2036fc9fcb9e",
  "command": "WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=... WORKER_SECRET=... WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 ./path-to-binary"
}
```

Notes:

- `WORKER_SECRET` appears only here (one-time return).
- `./path-to-binary` is a placeholder and must be replaced by your real worker executable command.

Errors:

- `503` provisioning unavailable
- `500` create failure

### 5.5 Delete Worker

`DELETE /api/v1/workers/:node_id`

Responses:

- `204` deleted
- `404` worker not found
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

Errors:

- `400` invalid body/params or `invalid_payload`
- `404` `session_not_found`
- `409` `session_busy` or canceled
- `429` no worker capacity
- `503` no compatible worker
- `504` timeout
- `502` unexpected execution failure

## 7. Task APIs (Bearer Token)

Task ownership is account-scoped by token.

### 7.1 Submit Task

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

Possible responses:

- `202` task still running (contains `status_url`)
- `200` completed succeeded
- `409` completed canceled
- `504` completed timeout
- `429` completed failed with `error.code=no_capacity`
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

### 7.2 Get Task

`GET /api/v1/tasks/:task_id`

Responses:

- `200` task snapshot
- `404` task not found (including cross-account access)

### 7.3 Cancel Task

`POST /api/v1/tasks/:task_id/cancel`

Responses:

- `200` canceled (or best-effort cancel accepted)
- `404` task not found (including cross-account access)
- `409` task already terminal (returns task snapshot)
- `500` cancel failure

## 8. MCP API (Bearer Token)

Endpoint: `POST /mcp`

- Transport: MCP Streamable HTTP
- Server mode: stateless JSON response
- `GET /mcp` returns `405` with `Allow: POST`
- Requires `Authorization: Bearer <access-token>`
- Recommended headers:
  - `Content-Type: application/json`
  - `Accept: application/json, text/event-stream`

### 8.1 Core MCP Methods

Supported MCP flow includes standard methods such as:

- `initialize`
- `tools/list`
- `tools/call`

### 8.2 Tool Definitions

Tool argument schemas use `additionalProperties=false`.
Unknown arguments are rejected with JSON-RPC `-32602 invalid params`.

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

#### Tool: `readImage`

Input:

```json
{ "session_id": "sess_xxx", "file_path": "/workspace/a.png", "timeout_ms": 60000 }
```

- `session_id` required
- `file_path` required
- `timeout_ms` optional, `1..600000`, default `60000`

Behavior:

- If target MIME is `image/*`: returns one image content item.
- If non-image MIME: returns one text content item:
  - `unsupported mime type: <mime>; expected image/*`

### 8.3 MCP Errors

- Missing/invalid token: HTTP `401`
- Invalid tool params: JSON-RPC error `-32602`
- Execution failures: returned as MCP tool error content (`isError=true`)

## 9. Worker gRPC API (`api/proto/registry/v1/registry.proto`)

Service:

```proto
service WorkerRegistryService {
  rpc Connect(stream ConnectRequest) returns (stream ConnectResponse);
}
```

### 9.1 Stream Flow

Worker establishes a bidirectional stream and typically sends:

1. `ConnectRequest.hello` (`ConnectHello`)
2. Periodic `ConnectRequest.heartbeat` (`HeartbeatFrame`)
3. `ConnectRequest.command_result` (`CommandResult`) for dispatched commands

Console responds with:

1. `ConnectResponse.connect_ack` (`ConnectAck`)
2. `ConnectResponse.heartbeat_ack` (`HeartbeatAck`)
3. `ConnectResponse.command_dispatch` (`CommandDispatch`)

### 9.2 Key Messages

- `ConnectHello` includes worker identity, capabilities, labels, version, and `worker_secret`.
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

## 10. Security Notes

- Console gRPC has no built-in TLS/mTLS in this release.
- `worker-docker` rejects insecure console endpoints by default, and allows plaintext only when `WORKER_CONSOLE_INSECURE=true`.
- Put console HTTP (`:8089`) and gRPC (`:50051`) behind a reverse proxy/gateway and enforce TLS for external access.
- Keep gRPC endpoint private and tunnel/encrypt traffic in production.
- Token plaintext and `WORKER_SECRET` are one-time return values.
- `GET /api/v1/console/tokens/:token_id/value` and `GET /api/v1/workers/:node_id/startup-command` are intentionally `410 Gone`.
