# Onlyboxes API 统一参考

[English](API.md)

本文档统一收录 Onlyboxes 当前公开的全部 API。

- Console HTTP 基地址：`http://<console-host>:8089`
- Console Worker gRPC 地址：`<console-host>:50051`
- REST API 前缀：`/api/v1`

## 1. 鉴权模型

Onlyboxes 有两套鉴权路径：

1. 控制台会话（Cookie）：用于管理类 API
2. 访问令牌（Bearer Token）：用于执行类 API 与 MCP

### 1.1 控制台会话（Cookie）

- Cookie 名称：`onlyboxes_console_session`
- 由 `POST /api/v1/console/login` 创建
- 用于：
  - `/api/v1/console/session`
  - `/api/v1/console/logout`
  - `/api/v1/console/register`
  - `/api/v1/console/tokens*`
  - `/api/v1/workers*`（管理员路由）
- 会话有效期为 12 小时（内存态）；console 重启后会话全部失效。

### 1.2 访问令牌（Bearer）

- 请求头格式：`Authorization: Bearer <access-token>`
- 用于：
  - `/api/v1/commands/*`
  - `/api/v1/tasks*`
  - `/mcp`
- 若系统中没有 token，所有 token 鉴权接口会返回 `401`。

## 2. REST 通用约定

- 内容类型：`application/json`
- 常见错误结构：

```json
{ "error": "message" }
```

- 时间字段使用 RFC3339。
- ID 字段均为不透明字符串（如 `acc_*`、`tok_*`、worker UUID、task ID）。

## 3. 控制台认证 API

### 3.1 登录

`POST /api/v1/console/login`

请求：

```json
{
  "username": "admin",
  "password": "secret"
}
```

成功 `200`：

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

错误：

- `400` JSON 结构非法
- `401` 用户名或密码错误
- `500` 会话创建失败

### 3.2 会话信息

`GET /api/v1/console/session`

- 需要控制台 Cookie 会话。
- 成功返回结构与登录响应一致。

错误：

- `401` 未登录

### 3.3 登出

`POST /api/v1/console/logout`

- 清理 Cookie，并删除内存会话（若存在）。

响应：

- `204 No Content`

### 3.4 注册普通账号（仅管理员）

`POST /api/v1/console/register`

请求：

```json
{
  "username": "dev-user",
  "password": "strong-password"
}
```

成功 `201`：

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

校验与错误：

- `403` 注册开关关闭（`CONSOLE_ENABLE_REGISTRATION=false`）
- `403` 当前账号不是管理员
- `400` 用户名为空、用户名长度 > 64、或密码为空
- `409` 用户名冲突（不区分大小写）
- `500` 数据库或内部错误

## 4. Token 管理 API（控制台会话鉴权）

Token 按账号隔离；每个账号只能管理自己的 token。

### 4.1 查询 Token 列表

`GET /api/v1/console/tokens`

成功 `200`：

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

### 4.2 创建 Token

`POST /api/v1/console/tokens`

请求：

```json
{
  "name": "ci-prod",
  "token": "optional-manual-token"
}
```

约束：

- `name` 必填，trim 后长度 <= 64，同账号内大小写不敏感唯一
- `token` 可选：
  - 省略时自动生成（`obx_<hex>`）
  - 手动提供时：trim 后不能为空、不能含空白字符、长度 <= 256

成功 `201`：

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

错误：

- `400` 参数校验失败
- `409` token 名称冲突或 token 值冲突
- `500` 内部错误

### 4.3 删除 Token

`DELETE /api/v1/console/tokens/:token_id`

响应：

- `204` 删除成功
- `404` token 不存在（或不属于当前账号）

### 4.4 查询 Token 明文

`GET /api/v1/console/tokens/:token_id/value`

固定返回 `410 Gone`：

```json
{
  "error": "token value is only returned at creation time; delete and recreate the token to obtain a new value"
}
```

## 5. Worker 管理 API（管理员 + 控制台会话鉴权）

### 5.1 查询 Worker 列表

`GET /api/v1/workers?page=1&page_size=20&status=all`

查询参数：

- `page`：正整数，默认 `1`
- `page_size`：正整数，默认 `20`，最大 `100`
- `status`：`all|online|offline`，默认 `all`

成功 `200`：

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

错误：

- `400` 查询参数非法

### 5.2 Worker 统计

`GET /api/v1/workers/stats?stale_after_sec=30`

查询参数：

- `stale_after_sec`：正整数，默认 `30`

成功 `200`：

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

### 5.3 Worker 并发占用统计

`GET /api/v1/workers/inflight`

成功 `200`：

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

### 5.4 创建 Worker 凭据

`POST /api/v1/workers`

请求体：无。

成功 `201`：

```json
{
  "node_id": "2f51f8f9-77f2-4c1a-a4f5-2036fc9fcb9e",
  "command": "WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=... WORKER_SECRET=... WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 ./path-to-binary"
}
```

说明：

- `WORKER_SECRET` 仅在该接口创建时返回一次。
- `./path-to-binary` 为占位符，需要替换为实际 worker 启动命令。

错误：

- `503` provisioning 不可用
- `500` 创建失败

### 5.5 删除 Worker

`DELETE /api/v1/workers/:node_id`

响应：

- `204` 删除成功
- `404` worker 不存在
- `400` 缺少 `node_id`
- `503` provisioning 不可用

### 5.6 获取 Worker 启动命令

`GET /api/v1/workers/:node_id/startup-command`

固定返回 `410 Gone`：

```json
{
  "error": "worker secret is returned only when creating the worker; delete and recreate to get a new startup command"
}
```

## 6. 命令执行 API（Bearer Token 鉴权）

### 6.1 Echo 命令

`POST /api/v1/commands/echo`

请求：

```json
{
  "message": "hello",
  "timeout_ms": 5000
}
```

约束：

- `message` 必填，trim 后不能为空
- `timeout_ms` 可选，范围 `1..60000`，默认 `5000`

成功 `200`：

```json
{ "message": "hello" }
```

错误：

- `400` 请求体错误 / message 缺失 / timeout 超范围
- `429` 无可用并发容量
- `503` 无在线 echo worker
- `504` 超时
- `502` 执行或内部异常

### 6.2 Terminal 命令

`POST /api/v1/commands/terminal`

请求：

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

约束：

- `command` 必填，trim 后不能为空
- `timeout_ms` 可选，范围 `1..600000`，默认 `60000`
- `request_id` 可选，幂等键（按账号隔离）

成功 `200`：

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

错误：

- `400` 请求参数非法或 `invalid_payload`
- `404` `session_not_found`
- `409` `session_busy` 或任务被取消
- `429` 无可用并发容量
- `503` 无可用 worker
- `504` 超时
- `502` 其他执行失败

## 7. 任务 API（Bearer Token 鉴权）

Task 所有权按账号隔离（由 token 对应账号决定）。

### 7.1 提交任务

`POST /api/v1/tasks`

请求：

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

约束：

- `capability` 必填且非空
- `input` 必须是合法 JSON（省略时默认为 `{}`）
- `mode`：`sync|async|auto`，默认 `auto`
- `wait_ms`：`1..60000`，默认 `1500`
- `timeout_ms`：`1..600000`，默认 `60000`
- `request_id`：可选幂等键（账号维度去重）

可能响应：

- `202` 任务未完成（包含 `status_url`）
- `200` 任务完成且成功
- `409` 任务完成且被取消
- `504` 任务完成且超时
- `429` 任务完成失败且 `error.code=no_capacity`
- `503` 任务完成失败且 `error.code=no_worker`
- `502` 任务完成失败（其他错误码）

`202` 示例：

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

已完成示例：

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

任务错误字段：

```json
"error": {
  "code": "execution_failed",
  "message": "..."
}
```

提交阶段错误：

- `400` 参数/模式/时间范围/请求体非法
- `409` request_id 已在处理中
- `429` 无可用并发容量
- `503` 无匹配能力 worker
- `504` 请求超时
- `502` 提交失败

### 7.2 查询任务

`GET /api/v1/tasks/:task_id`

响应：

- `200` 返回任务快照
- `404` 任务不存在（包含跨账号访问）

### 7.3 取消任务

`POST /api/v1/tasks/:task_id/cancel`

响应：

- `200` 取消成功（或已受理 best-effort 取消）
- `404` 任务不存在（包含跨账号访问）
- `409` 任务已终态（返回任务快照）
- `500` 取消失败

## 8. MCP API（Bearer Token 鉴权）

端点：`POST /mcp`

- 传输：MCP Streamable HTTP
- 服务模式：无状态 JSON 响应
- `GET /mcp` 返回 `405`，`Allow: POST`
- 需要请求头：`Authorization: Bearer <access-token>`
- 建议请求头：
  - `Content-Type: application/json`
  - `Accept: application/json, text/event-stream`

### 8.1 MCP 基础方法

支持标准 MCP 调用流程，包括：

- `initialize`
- `tools/list`
- `tools/call`

### 8.2 工具定义

所有工具参数 schema 都是 `additionalProperties=false`。
传入未定义参数会返回 JSON-RPC `-32602 invalid params`。

#### 工具：`echo`

输入：

```json
{ "message": "hello", "timeout_ms": 5000 }
```

- `message` 必填
- `timeout_ms` 可选，`1..60000`，默认 `5000`

输出：

```json
{ "message": "hello" }
```

#### 工具：`pythonExec`

输入：

```json
{ "code": "print(1)", "timeout_ms": 60000 }
```

- `code` 必填
- `timeout_ms` 可选，`1..600000`，默认 `60000`

输出：

```json
{ "output": "1\n", "stderr": "", "exit_code": 0 }
```

说明：`exit_code` 非 0 也按正常工具输出返回，不是协议错误。

#### 工具：`terminalExec`

输入：

```json
{
  "command": "pwd",
  "session_id": "optional",
  "create_if_missing": false,
  "lease_ttl_sec": 60,
  "timeout_ms": 60000
}
```

- `command` 必填
- `session_id` 可选
- `create_if_missing` 可选，默认 `false`
- `lease_ttl_sec` 可选
- `timeout_ms` 可选，`1..600000`，默认 `60000`

输出：

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

#### 工具：`readImage`

输入：

```json
{ "session_id": "sess_xxx", "file_path": "/workspace/a.png", "timeout_ms": 60000 }
```

- `session_id` 必填
- `file_path` 必填
- `timeout_ms` 可选，`1..600000`，默认 `60000`

行为：

- 若目标 MIME 为 `image/*`：返回一个图片内容项。
- 若目标 MIME 非图片：返回一个文本内容项：
  - `unsupported mime type: <mime>; expected image/*`

### 8.3 MCP 错误行为

- Token 缺失或无效：HTTP `401`
- 参数校验失败：JSON-RPC `-32602`
- 执行异常：作为 MCP tool error 内容返回（`isError=true`）

## 9. Worker gRPC API（`api/proto/registry/v1/registry.proto`）

服务定义：

```proto
service WorkerRegistryService {
  rpc Connect(stream ConnectRequest) returns (stream ConnectResponse);
}
```

### 9.1 流程

Worker 建立双向流后，通常会发送：

1. `ConnectRequest.hello`（`ConnectHello`）
2. 周期性 `ConnectRequest.heartbeat`（`HeartbeatFrame`）
3. 对调度任务回传 `ConnectRequest.command_result`（`CommandResult`）

Console 回包：

1. `ConnectResponse.connect_ack`（`ConnectAck`）
2. `ConnectResponse.heartbeat_ack`（`HeartbeatAck`）
3. 下发执行任务 `ConnectResponse.command_dispatch`（`CommandDispatch`）

### 9.2 核心消息

- `ConnectHello` 包含 worker 标识、能力声明、labels、version、`worker_secret`。
- `CommandDispatch` 包含：
  - `command_id`
  - `capability`
  - `payload_json`
  - `deadline_unix_ms`
- `CommandResult` 包含：
  - `command_id`
  - 可选 `error { code, message }`
  - `payload_json`
  - `completed_unix_ms`

## 10. 安全说明

- 当前版本 console gRPC 不提供内建 TLS/mTLS。
- `worker-docker` 默认会拒绝不安全 console 端点，只有显式设置 `WORKER_CONSOLE_INSECURE=true` 才允许明文连接。
- 请将 console HTTP（`:8089`）和 gRPC（`:50051`）端点放在反向代理/网关之后，并对外访问强制 TLS。
- 生产环境应将 gRPC 端口保持内网并通过隧道/链路加密。
- Token 明文与 `WORKER_SECRET` 仅在创建时返回一次。
- `GET /api/v1/console/tokens/:token_id/value` 与 `GET /api/v1/workers/:node_id/startup-command` 设计为永久 `410 Gone`。
