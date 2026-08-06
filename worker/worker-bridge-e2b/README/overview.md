# E2B Bridge Worker

`worker-bridge-e2b` 是 Onlyboxes 的远程 E2B 执行节点。worker 连接 console 的 `WorkerRegistryService.Connect` 双向流，发送身份、能力和心跳，并在收到任务后调用 E2B 控制 API 与沙箱内的 envd。

## 配置入口

启动至少需要 `WORKER_ID`、`WORKER_SECRET`、`WORKER_E2B_API_KEY`、`WORKER_E2B_PYTHON_TEMPLATE` 和 `WORKER_E2B_TERMINAL_TEMPLATE`。E2B 标准环境变量别名、全部可选参数、默认值和 `config.toml` 键见[完整配置参数参考](config-file.md)，可直接复制根目录的 [`config.example.toml`](../config.example.toml)。

console gRPC 默认启用 TLS。只有在可信内网开发环境中才应设置 `WORKER_CONSOLE_INSECURE=true`；明文连接会暴露 hello 中的一次性 worker secret。

## E2B 通信

- 沙箱创建、TTL 更新和销毁使用 E2B Control API。
- 命令执行使用 envd 的 Connect RPC `process.Process/Start`，端口为 E2B 官方协议固定的 `49983`。
- 文件读取使用 envd HTTP `GET /files`。
- E2B API Key 只发送给 Control API；envd 使用创建响应里的独立 access token。
- 日志不记录 API Key、envd access token、原始代码、命令或文件内容。

Go 客户端根据 E2B 的公开 OpenAPI 与 envd protobuf 实现。E2B 当前没有官方 Go SDK。

## 能力

worker 在 hello 中声明以下四项能力：

| 工具 | 用途 | 沙箱生命周期 |
| --- | --- | --- |
| `echo` | 检查 console 与 worker 的调用链路 | 不创建沙箱 |
| `pythonExec` | 在 Python 模板中执行一次代码 | 每次调用创建并销毁 |
| `terminalExec` | 创建或复用终端 session 执行命令 | 按 lease 复用 |
| `terminalResource` | 校验、读取或导出终端 session 中的文件 | 复用已有 session |

请求和返回值都是 JSON 对象。未知字段会被忽略；缺少必填字段时返回 `invalid_payload`。

### echo

请求与返回字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `message` | string | 是 | 要原样返回的非空文本 |

示例：`{"message":"ping"}` 返回 `{"message":"ping"}`。

### pythonExec

每次请求创建独立的 Python 模板沙箱，将代码写入 `/tmp/onlyboxes-pythonexec.py`，通过 `uv run` 执行，随后销毁沙箱。模板必须提供 `uv` 与 Python，并可使用 PEP 723 内联依赖。

请求字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `code` | string | 是 | 要执行的非空 Python 源码 |

返回字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `output` | string | 标准输出 |
| `stderr` | string | 标准错误 |
| `exit_code` | number | 进程退出码 |

```json
{"output":"...","stderr":"...","exit_code":0}
```

非零退出码属于执行结果，不会被改写为 worker 传输错误。命令 deadline 取消后，worker 仍会使用独立的短超时请求销毁沙箱。

### terminalExec

请求字段：

| 字段 | 类型 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `command` | string | 是 | — | 由 `/bin/bash -l -c` 执行的非空命令 |
| `session_id` | string | 否 | 空 | 空值时创建新 session；非空时复用指定 session |
| `create_if_missing` | boolean | 否 | `false` | 指定的 session 不存在时是否创建 |
| `lease_ttl_sec` | number | 否 | `WORKER_TERMINAL_LEASE_DEFAULT_SEC` | session lease，必须位于配置的最小值和最大值之间 |

返回字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | string | Onlyboxes session ID |
| `created` | boolean | 本次调用是否创建了 session |
| `stdout` | string | 标准输出 |
| `stderr` | string | 标准错误 |
| `exit_code` | number | 进程退出码 |
| `stdout_truncated` | boolean | 标准输出是否因大小限制被截断 |
| `stderr_truncated` | boolean | 标准错误是否因大小限制被截断 |
| `lease_expires_unix_ms` | number | 当前 lease 到期时间，Unix 毫秒 |

请求示例：

```json
{"command":"pwd","session_id":"optional","create_if_missing":false,"lease_ttl_sec":60}
```

- 未提供 `session_id` 时创建新 E2B 沙箱和 Onlyboxes session。
- 相同 `session_id` 复用沙箱及其文件系统。
- 未知 session 返回 `session_not_found`，除非 `create_if_missing=true`。
- lease 只会延长，不会被较短的 TTL 缩短；worker 同步调用 E2B timeout API，避免本地 lease 长于 E2B 沙箱寿命。
- 单 session 并发由 `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` 控制，并与 `terminalResource` 共用。
- 达到并发上限返回 `session_busy`。
- 新建 terminal session 受 `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` 限制（`0` 表示不限），超出正数上限返回 `session_capacity_exceeded`；容量已满时已有 session 仍可执行。创建中、可用、销毁中和 E2B cleanup 进行中的 session 都计入容量；该限制只适用于当前 worker 进程。
- 创建中的 session 会阻塞后续调用，所有等待者共享创建结果。
- 某个命令超时会把 session 标记为待销毁；已有并发命令继续完成，最后一个调用退出后才销毁沙箱。
- 空闲 session 到期后由 janitor 销毁；worker 正常退出时销毁所有仍管理的沙箱。

每个命令由独立的 `/bin/bash -l -c` 进程执行，因此共享文件系统，但不共享 cwd、shell 变量或当前进程环境。

### terminalResource

请求字段：

| 字段 | 类型 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `session_id` | string | 是 | — | 已存在的终端 session ID |
| `file_path` | string | 是 | — | session 沙箱内的文件路径 |
| `action` | string | 否 | `validate` | `validate`、`read` 或 `export`，不区分大小写 |
| `signed_url` | string | 仅 `export` | 空 | 接收文件的 HTTP 预签名 URL |
| `headers` | object<string,string> | 否 | 空 | `export` 上传时附加的 HTTP 请求头 |

返回字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | string | 终端 session ID |
| `file_path` | string | 被处理的文件路径 |
| `mime_type` | string | 推断出的 MIME 类型；无法识别时为 `application/octet-stream` |
| `size_bytes` | number | 文件大小 |
| `blob` | string | 仅 `read` 返回；文件字节经过 JSON base64 编码 |

请求示例：

```json
{"session_id":"required","file_path":"required","action":"validate|read|export","signed_url":"export required"}
```

- `validate` 返回 MIME 类型与大小。
- `read` 返回 `blob`；JSON 编码后表现为 base64。
- `read` 大小上限为 `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES`。
- `export` 通过 HTTP `PUT` 上传到 console 提供的预签名 URL。
- `WORKER_TERMINAL_EXPORT_MODE=sandbox` 时，终端模板中的 `python3` 直接从 E2B 沙箱上传，文件内容不经过 worker；这是默认模式。
- `WORKER_TERMINAL_EXPORT_MODE=worker` 时，worker 从 E2B 流式下载并转发文件。
- `WORKER_TERMINAL_EXPORT_MAX_BYTES=0` 表示不限制导出大小。
- 终端模板必须提供 `python3`，用于安全地探测文件类型、大小和目录状态，并在 `sandbox` 导出模式下执行流式上传。

领域错误为 `file_not_found`、`path_is_directory` 和 `file_too_large`。

## 会话与心跳

- hello 声明 `echo`、`pythonExec`、`terminalExec`、`terminalResource`，每项都有独立的 `max_inflight`。
- heartbeat 报告 `active_session_count`。
- worker 容忍一次 heartbeat ack 超时，连续两次超时后重连。
- session 被 console 替换并返回 `FailedPrecondition` 时立即进入重连流程。
- 其余断线使用最长 `15` 秒的指数退避。
- 版本只来自构建期注入，开发构建报告 `dev`，运行时不能覆盖。

## 本地验证

普通测试不访问 E2B：

```bash
go test -race ./...
```

真实 E2B 冒烟会创建并自动销毁一个终端模板沙箱：

```bash
E2B_INTEGRATION=1 \
E2B_API_KEY=... \
E2B_TERMINAL_TEMPLATE=... \
go test ./internal/e2b -run TestIntegrationSandboxCommandAndFile -count=1 -v
```
