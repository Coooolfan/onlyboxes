# Onlyboxes API 统一参考

[English](API.md)

本文档统一收录 Onlyboxes 当前公开的全部 API。

- Console HTTP 基地址：`http://<console-host>:8089`
- Console Worker gRPC 地址：`<console-host>:50051`
- REST API 前缀：`/api/v1`

## 1. 鉴权模型

Onlyboxes 有以下鉴权路径：

1. 控制台会话（Cookie）：用于管理类 API
2. 控制台 Bearer 凭据：用于部分控制台自动化 API
3. 访问令牌（Bearer Token）：用于执行类 API 与 MCP

### 1.1 控制台会话（Cookie）

- Cookie 名称：`onlyboxes_console_session`
- 由 `POST /api/v1/console/login` 创建
- 用于：
  - `/api/v1/console/session`
  - `/api/v1/console/logout`
  - `/api/v1/console/password`
  - `/api/v1/console/register`
  - `/api/v1/console/accounts*`
  - `/api/v1/console/tokens*`
  - `/api/v1/workers*`（按角色作用域）
- 会话有效期为 12 小时（内存态）；console 重启后会话全部失效。

### 1.2 访问令牌（Bearer）

- 请求头格式：`Authorization: Bearer <access-token>`
- 用于：
  - `/api/v1/commands/*`
  - `/api/v1/tasks*`
  - `/mcp`
- 支持的 token 类型：
  - 通过控制台 Cookie 会话管理的 trusted token
  - 配置 `CONSOLE_JIT_SIGNING_KEY` 后可用的 MCP JIT token（`obx_jit_v1.<payload>.<signature>`）
- 若系统中没有 trusted token，trusted-token 鉴权会返回 `401`；配置后，有效 MCP JIT token 仍可鉴权。

### 1.3 控制台 Bearer 凭据

- 请求头格式：`Authorization: Bearer <credential>`
- Console API key 可用于控制台 API，但不能访问要求 Cookie 会话的接口。
- Dashboard JIT token 可用于服务端到服务端的 worker-sys 配置场景：
  - 格式：`obx_dashboard_jit_v1.<payload>.<signature>`
  - 签名：使用 `CONSOLE_DASHBOARD_JIT_SIGNING_KEY` 对 `obx_dashboard_jit_v1.<payload>` 做 HMAC-SHA256
  - Payload 要求 `iss`、`sub`、`scope:"dashboard"`，可选 `exp`（Unix 毫秒）
  - `CONSOLE_DASHBOARD_JIT_SIGNING_KEY` 必须与 `CONSOLE_JIT_SIGNING_KEY` 不同。
- Dashboard JIT 与 MCP JIT 使用相同的 `(iss, sub) -> account` 派生逻辑，首次使用会创建非管理员账号，但不能鉴权 `/mcp`。
- MCP JIT payload 同样支持可选 `exp`（Unix 毫秒）。
- MCP JIT token（`obx_jit_v1.*`）会被控制台鉴权拒绝。
- Cookie-only 接口会拒绝控制台 Bearer 凭据。

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

### 3.5 修改当前账号密码

`POST /api/v1/console/password`

请求：

```json
{
  "current_password": "old-password",
  "new_password": "new-password"
}
```

响应：

- `204` 修改成功
- `400` JSON 结构非法、`current_password` 缺失或 `new_password` 缺失
- `401` 当前密码错误
- `500` 内部错误

说明：

- 该接口需要控制台会话鉴权。
- 密码更新后会轮换该账号的活动会话。

### 3.6 查询账号列表（仅管理员）

`GET /api/v1/console/accounts?page=1&page_size=20`

查询参数：

- `page`：正整数，默认 `1`
- `page_size`：正整数，默认 `20`，最大 `100`

成功 `200`：

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

错误：

- `400` 查询参数非法
- `403` 当前账号不是管理员
- `500` 数据库或内部错误

### 3.7 删除账号（仅管理员）

`DELETE /api/v1/console/accounts/:account_id`

响应：

- `204` 删除成功
- `403` 禁止删除当前登录账号
- `403` 禁止删除管理员账号
- `404` 账号不存在
- `500` 内部错误

## 4. Token 管理 API（控制台 Cookie 会话鉴权）

Token 按账号隔离；每个账号只能管理自己的 token。

本节所有接口都要求控制台 Cookie 会话。Console API key 与 Dashboard JIT token 会被拒绝，避免控制台 Bearer 凭据创建或管理 MCP trusted token。

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

## 5. Worker 管理 API（控制台会话鉴权，按角色作用域）

Worker 类型：

- `normal`（对应 `worker-docker`）
- `worker-sys`（对应 `worker-sys`）

权限矩阵：

- 管理员：
  - list/stats/inflight：查看全部
  - delete：可删任意 worker
  - create：可创建 `normal` 与 `worker-sys`
- 普通用户：
  - list/stats/inflight：仅本人 `worker-sys`
  - delete：仅本人 `worker-sys`（其他目标返回 `404`）
  - create：仅可创建 `worker-sys`，且每账号最多一个

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

说明：普通用户响应仅统计本人 `worker-sys`。

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

说明：普通用户响应仅包含本人 `worker-sys`。

### 5.4 创建 Worker 凭据

`POST /api/v1/workers`

请求体：

```json
{
  "type": "normal"
}
```

规则：

- `type` 必填，取值 `normal|worker-sys`
- 仅管理员可创建 `normal`
- 每个账号最多创建一个 `worker-sys`

成功 `201`：

```json
{
  "node_id": "2f51f8f9-77f2-4c1a-a4f5-2036fc9fcb9e",
  "type": "normal",
  "command": "WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=... WORKER_SECRET=... WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 ./path-to-binary"
}
```

说明：

- `WORKER_SECRET` 仅在该接口创建时返回一次。
- `./path-to-binary` 为占位符，需要替换为实际 worker 启动命令。

错误：

- `400` 请求体不合法 / `type` 非法
- `403` 普通用户创建 `normal`
- `409` 当前账号已存在 `worker-sys`
- `503` provisioning 不可用
- `500` 创建失败

### 5.5 删除 Worker

`DELETE /api/v1/workers/:node_id`

响应：

- `204` 删除成功
- `404` worker 不存在（普通用户删除越权目标也返回 `404`）
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

### 6.3 Computer Use 命令

`POST /api/v1/commands/computer-use`

请求：

```json
{
  "command": "pwd",
  "timeout_ms": 60000,
  "request_id": "optional-idempotency-key"
}
```

约束：

- `command` 必填，trim 后不能为空
- `timeout_ms` 可选，范围 `1..600000`，默认 `60000`
- `request_id` 可选，幂等键（按账号隔离）
- 兼容旧客户端时，传入 `lease_ttl_sec` 会被忽略
- 调度只会路由到调用账号自己的 `worker-sys`
- 单账号并发固定为 1（`max_inflight=1`）

成功 `200`：

```json
{
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false
}
```

错误：

- `400` 请求参数非法或 `invalid_payload`
- `409` worker `session_busy` 或任务被取消
- `429` 无可用并发容量（`no_capacity`）
- `503` 当前账号无在线 `worker-sys`（`no_worker`）
- `504` 超时
- `502` 其他执行失败

## 7. Sandbox 元数据 API（Bearer Token 鉴权）

Sandbox 元数据按 token 对应账号隔离。Trusted token 与 MCP JIT token 均可调用。

### 7.1 获取 Sandbox 元数据

`GET /api/v1/sandbox/metadata`

成功 `200`：

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

说明：

- `computerUse` 与 `readImage` 可用性按调用账号自有 `worker-sys` 统计。
- `terminalExec`、`terminalResource`、`pythonExec`、`echo` 返回全局在线 worker 可用性。
- Token 缺失或无效返回 `401`。

## 8. 任务 API（Bearer Token 鉴权）

Task 所有权按账号隔离（由 token 对应账号决定）。

### 8.1 提交任务

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

### 8.2 查询任务

`GET /api/v1/tasks/:task_id`

响应：

- `200` 返回任务快照
- `404` 任务不存在（包含跨账号访问）

### 8.3 取消任务

`POST /api/v1/tasks/:task_id/cancel`

响应：

- `200` 取消成功（或已受理 best-effort 取消）
- `404` 任务不存在（包含跨账号访问）
- `409` 任务已终态（返回任务快照）
- `500` 取消失败

## 9. MCP API（Bearer Token 鉴权）

端点：`POST /mcp`

- 传输：MCP Streamable HTTP
- 服务模式：无状态 JSON 响应
- `GET /mcp` 返回 `405`，`Allow: POST`
- 需要请求头：`Authorization: Bearer <access-token>`
- 建议请求头：
  - `Content-Type: application/json`
  - `Accept: application/json, text/event-stream`

### 9.1 MCP 基础方法

支持标准 MCP 调用流程，包括：

- `initialize`
- `tools/list`
- `tools/call`

### 9.2 工具定义

所有工具参数 schema 都是 `additionalProperties=false`。
传入未定义参数会返回 JSON-RPC `-32602 invalid params`。

> 在 `CONSOLE_HIDDEN_TOOLS` 中列出的工具不会出现在 `tools/list` 中；如果客户端已知工具名，仍可继续通过 `tools/call` 调用。

> 每个工具的 `name` / `title` / `description`、以及每个参数的 `description` 可以通过 `CONSOLE_MCP_TOOL_<TOOL>_NAME`、`CONSOLE_MCP_TOOL_<TOOL>_TITLE`、`CONSOLE_MCP_TOOL_<TOOL>_DESCRIPTION`、`CONSOLE_MCP_TOOL_<TOOL>_PARAM_<PARAM>_DESCRIPTION` 在运行时覆盖。`_NAME` 会改变客户端在 `tools/list` 中看到、`tools/call` 用作路由键的值（必须匹配 `^[a-zA-Z0-9_-]{1,64}$`，若与其他工具的内置默认名冲突会回退）；`CONSOLE_HIDDEN_TOOLS` 仍然填内部 capability ID，不能填改名后的值。将参数描述设置为空字符串会把该参数从 `inputSchema.properties` 与 `required` 中移除（并把对应 schema 的 `additionalProperties` 翻转为 `true`），但 `tools/call` 依然会接受该字段。完整的 `<TOOL>` / `<PARAM>` 映射详见 Console 配置文档。

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

#### 工具：`computerUse`

输入：

```json
{
  "command": "pwd",
  "timeout_ms": 60000,
  "request_id": "optional-idempotency-key"
}
```

- `command` 必填
- `timeout_ms` 可选，`1..600000`，默认 `60000`
- `request_id` 可选，幂等键（账号维度）
- 只会路由到调用账号自己的 `worker-sys`
- 不包含终端会话字段（`session_id`、`create_if_missing`、`created`）

输出：

```json
{
  "stdout": "...",
  "stderr": "...",
  "exit_code": 0,
  "stdout_truncated": false,
  "stderr_truncated": false
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
- 当 `session_id` 精确等于 `computerUse` 时，路由到调用账号自有 `worker-sys` 的 `readImage` capability
- 其他 `session_id` 仍路由到 `terminalResource` capability

行为：

- 若目标 MIME 为 `image/*`：返回一个图片内容项。
- 若目标 MIME 非图片：返回一个文本内容项：
  - `unsupported mime type: <mime>; expected image/*`

### 9.3 MCP 错误行为

- Token 缺失或无效：HTTP `401`
- 参数校验失败：JSON-RPC `-32602`
- `computerUse` 调用账号未创建 `worker-sys`：JSON-RPC `-32010`，`data.error_code="WORKER_SYS_REQUIRED"`
- `computerUse` 调用账号已注册 `worker-sys` 但不在线：JSON-RPC `-32011`，`data.error_code="WORKER_SYS_OFFLINE"`
- 执行异常：作为 MCP tool error 内容返回（`isError=true`）

## 10. Worker gRPC API（`api/proto/registry/v1/registry.proto`）

服务定义：

```proto
service WorkerRegistryService {
  rpc Connect(stream ConnectRequest) returns (stream ConnectResponse);
}
```

### 10.1 流程

Worker 建立双向流后，通常会发送：

1. `ConnectRequest.hello`（`ConnectHello`）
2. 周期性 `ConnectRequest.heartbeat`（`HeartbeatFrame`）
3. 对调度任务回传 `ConnectRequest.command_result`（`CommandResult`）

Console 回包：

1. `ConnectResponse.connect_ack`（`ConnectAck`）
2. `ConnectResponse.heartbeat_ack`（`HeartbeatAck`）
3. 下发执行任务 `ConnectResponse.command_dispatch`（`CommandDispatch`）

### 10.2 核心消息

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

## 11. 安全说明

- 当前版本 console gRPC 不提供内建 TLS/mTLS。
- `worker-docker` 默认会拒绝不安全 console 端点，只有显式设置 `WORKER_CONSOLE_INSECURE=true` 才允许明文连接。
- `worker-sys` 的 `computerUse` 在宿主机直接执行 `/bin/sh -lc`，不提供容器隔离。
- `worker-sys` 的 `readImage` 直接读取宿主机文件，且仅接受 `session_id=computerUse`。
- `worker-sys` 必须部署在独立主机并配合严格的操作系统权限控制。
- 请将 console HTTP（`:8089`）和 gRPC（`:50051`）端点放在反向代理/网关之后，并对外访问强制 TLS。
- 生产环境应将 gRPC 端口保持内网并通过隧道/链路加密。
- Token 明文与 `WORKER_SECRET` 仅在创建时返回一次。
- `GET /api/v1/console/tokens/:token_id/value` 与 `GET /api/v1/workers/:node_id/startup-command` 设计为永久 `410 Gone`。
