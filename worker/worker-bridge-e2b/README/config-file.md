# E2B Bridge 配置参数

worker 按以下顺序查找 `config.toml`：

1. `WORKER_CONFIG_FILE` 指定的路径；文件缺失或格式错误时启动失败。
2. worker 可执行文件旁的 `config.toml`。
3. 当前工作目录的 `config.toml`。

单项配置的取值优先级为：

1. `WORKER_*` 环境变量。
2. 对应的 E2B 标准环境变量别名。
3. `config.toml`。
4. 内置默认值。

环境变量只要存在就会覆盖下一层，包括显式设置为空字符串。配置键通常等于环境变量去掉 `WORKER_` 前缀并转为小写，例如 `WORKER_E2B_API_KEY` 对应 `e2b_api_key`。

## 身份与 console

标记为“必填”的参数没有默认值，缺失时 worker 启动失败。

| 环境变量 | `config.toml` 键 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `WORKER_CONFIG_FILE` | — | 自动查找 | 显式指定配置文件路径；文件不存在或格式错误时启动失败 |
| `WORKER_ID` | `id` | 必填 | console 创建 worker 后分配的 ID |
| `WORKER_SECRET` | `secret` | 必填 | hello 认证使用的一次性 worker secret |
| `WORKER_CONSOLE_GRPC_TARGET` | `console_grpc_target` | `127.0.0.1:50051` | console gRPC 地址 |
| `WORKER_CONSOLE_INSECURE` | `console_insecure` | `false` | `true` 时关闭 TLS，仅用于可信开发环境 |
| `WORKER_NODE_NAME` | `node_name` | `worker-bridge-e2b-<ID 前缀>` | hello 中报告的节点名；空值时自动生成 |
| `WORKER_LABELS` | `[labels]` 或 `labels` | 空 | worker labels；环境变量接受 JSON 对象或逗号分隔的 `key=value` |

`WORKER_LABELS` 示例：

```bash
WORKER_LABELS='{"region":"cn","owner":"team-a"}'
WORKER_LABELS='region=cn,owner=team-a'
```

对应的 TOML 推荐写法：

```toml
[labels]
region = "cn"
owner = "team-a"
```

## 心跳与调用

| 环境变量 | `config.toml` 键 | 默认值 | 约束与说明 |
| --- | --- | --- | --- |
| `WORKER_HEARTBEAT_INTERVAL_SEC` | `heartbeat_interval_sec` | `5` | 正整数；发送 heartbeat 的基础间隔 |
| `WORKER_HEARTBEAT_JITTER_PCT` | `heartbeat_jitter_pct` | `20` | `0`–`100`；heartbeat 间隔的抖动比例 |
| `WORKER_CALL_TIMEOUT_SEC` | `call_timeout_sec` | `ceil(2.5 × heartbeat_interval_sec)` | 正整数；等待 console hello 和 heartbeat ack 的超时，默认配置下为 `13` 秒 |

## E2B

| 环境变量 | E2B 环境变量别名 | `config.toml` 键 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| `WORKER_E2B_API_KEY` | `E2B_API_KEY` | `e2b_api_key` | 必填 | E2B Control API Key |
| `WORKER_E2B_API_URL` | `E2B_API_URL` | `e2b_api_url` | `https://api.e2b.app` | Control API 基础 URL |
| `WORKER_E2B_DOMAIN` | `E2B_DOMAIN` | `e2b_domain` | `e2b.app` | envd 沙箱域名后缀 |
| `WORKER_E2B_SANDBOX_URL` | `E2B_SANDBOX_URL` | `e2b_sandbox_url` | 空 | 覆盖 envd 沙箱基础 URL，用于自托管、调试或集成测试 |
| `WORKER_E2B_PYTHON_TEMPLATE` | `E2B_PYTHON_EXEC_TEMPLATE` | `e2b_python_template` | 必填 | `pythonExec` 使用的模板 ID 或别名 |
| `WORKER_E2B_TERMINAL_TEMPLATE` | `E2B_TERMINAL_EXEC_TEMPLATE` | `e2b_terminal_template` | 必填 | `terminalExec` 使用的模板 ID 或别名 |
| `WORKER_E2B_REQUEST_TIMEOUT_SEC` | — | `e2b_request_timeout_sec` | `60` | 正整数；E2B Control API 请求以及 envd 建连、TLS 握手和响应头等待的超时秒数 |
| `WORKER_E2B_PYTHON_TIMEOUT_SEC` | `E2B_SANDBOX_TIMEOUT_SEC` | `e2b_python_timeout_sec` | `300` | 正整数；一次性 Python 沙箱的 E2B timeout 秒数 |

`WORKER_*` 变量始终优先于同一行的 E2B 别名。API URL 和 sandbox URL 末尾的 `/` 会被移除。

envd 成功返回响应头后，命令执行与文件流遵循 console 下发的 command deadline，避免长时间运行的命令或文件导出被建连超时提前中断。

## 公开预览

`WORKER_PROXY_ENABLED` / `proxy_enabled` 默认为 `false`。启用后，worker 在 Hello 中声明 E2B direct proxy 模式和内部 `terminalProxy` 能力。Console 的 Nginx 鉴权子请求会通过该能力取得当前 session 的 `https://<port>-<sandboxID>.<domain>` origin 与 traffic access token；业务流量直接走“用户 → Nginx → E2B”，不经过 worker。worker 创建 E2B sandbox 时同时设置 `network.allowPublicTraffic=false`，Nginx 使用内部 traffic token 访问，用户自己的 `Authorization` 不会被覆盖。

## 终端 session 与文件

| 环境变量 | `config.toml` 键 | 默认值 | 约束与说明 |
| --- | --- | --- | --- |
| `WORKER_TERMINAL_LEASE_MIN_SEC` | `terminal_lease_min_sec` | `60` | 正整数；请求可指定的最短 lease |
| `WORKER_TERMINAL_LEASE_MAX_SEC` | `terminal_lease_max_sec` | `1800` | 正整数；小于最小值时自动提高到最小值 |
| `WORKER_TERMINAL_LEASE_DEFAULT_SEC` | `terminal_lease_default_sec` | `300` | 正整数；自动限制在最小值与最大值之间 |
| `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` | `terminal_output_limit_bytes` | `1048576` | 正整数；分别限制 stdout、stderr 和 `terminalResource.read` |
| `WORKER_TERMINAL_EXPORT_MAX_BYTES` | `terminal_export_max_bytes` | `0` | 非负整数；`0` 表示不限制导出大小 |
| `WORKER_TERMINAL_EXPORT_MODE` | `terminal_export_mode` | `sandbox` | `worker`：文件流经 worker；`sandbox`：E2B 沙箱使用 `python3` 直接上传 |
| `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` | `terminal_session_max_inflight` | `128` | 正整数；同一 session 内 `terminalExec` 与 `terminalResource` 共享的并发上限 |
| `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` | `terminal_max_active_sessions` | `0` | 非负整数且不超过 `2147483647`；`0` 表示不限当前 worker 进程管理的 session 数量；创建中、销毁中和 E2B cleanup 中的 session 也计入；超出协议范围会在重连循环前终止启动 |

## 能力并发

以下参数限制整个 worker 同时处理某项能力的数量，并通过 hello 的 `max_inflight` 报告给 console。它们与单 session 并发限制同时生效。

| 环境变量 | `config.toml` 键 | 默认值 | 约束 |
| --- | --- | --- | --- |
| `WORKER_ECHO_MAX_INFLIGHT` | `echo_max_inflight` | `128` | 正整数 |
| `WORKER_PYTHON_EXEC_MAX_INFLIGHT` | `python_exec_max_inflight` | `32` | 正整数 |
| `WORKER_TERMINAL_EXEC_MAX_INFLIGHT` | `terminal_exec_max_inflight` | `64` | 正整数 |
| `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT` | `terminal_resource_max_inflight` | `128` | 正整数 |

## 日志

| 环境变量 | `config.toml` 键 | 默认值 | 可选值与说明 |
| --- | --- | --- | --- |
| `WORKER_LOG_LEVEL` | `log_level` | `info` | `debug`、`info`、`warn`、`error` |
| `WORKER_LOG_FORMAT` | `log_format` | `json` | `json`、`text` |
| `WORKER_LOG_ADD_SOURCE` | `log_add_source` | `false` | 是否记录源码位置；布尔值接受 `1/0`、`true/false`、`yes/no`、`on/off` |

正整数、百分比、枚举或布尔值无效时使用该项默认值。`WORKER_TERMINAL_EXPORT_MAX_BYTES` 和 `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` 均接受非负整数；负数或非法值分别回退为 `0`，但 `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` 超过 `2147483647` 时会因无法编码到协议字段而终止启动。无效的 `WORKER_TERMINAL_EXPORT_MODE` 回退为 `sandbox`。

配置文件中包含 worker secret 或 E2B API Key 时，应设置为仅运行用户可读，并且不要提交到版本库。可复制根目录的 [`config.example.toml`](../config.example.toml) 作为起点。
