# Worker BoxLite 实现说明

`worker-boxlite` 使用 Rust、Tonic 与 BoxLite SDK 实现 sandbox Worker。它通过 Console 的双向 gRPC 流接收命令，并用 microVM 后端提供一次性 Python 执行、有状态终端 session、文件资源操作和公开预览代理。

## 组件

| 组件 | 职责 |
| --- | --- |
| [`main.rs`](../src/main.rs) | 加载配置、初始化日志、处理进程信号并启动 runner |
| [`config.rs`](../src/config.rs) | 环境变量与 `config.toml` 映射、默认值和范围校验 |
| [`proto.rs`](../src/proto.rs) | 引入构建期生成的 registry protobuf 类型 |
| [`runner/mod.rs`](../src/runner/mod.rs) | 初始化共享 manager、校验启动条件并管理 Worker 生命周期 |
| [`runner/session_client.rs`](../src/runner/session_client.rs) | gRPC 连接、恢复握手、heartbeat、命令收发和重连 |
| [`runner/hello_builder.rs`](../src/runner/hello_builder.rs) | 构建 capability、版本、标签和 terminal capacity 声明 |
| [`runner/capability_executor.rs`](../src/runner/capability_executor.rs) | 参数解析、并发准入、错误映射和 capability 调度 |
| [`boxlite_runtime.rs`](../src/boxlite_runtime.rs) | BoxLite runtime、Box 创建、命令执行、复制和清理封装 |
| [`runner/terminal_session_manager.rs`](../src/runner/terminal_session_manager.rs) | Terminal session、lease、并发、容量、恢复和资源访问状态机 |
| [`runner/sandbox_proxy.rs`](../src/runner/sandbox_proxy.rs) | Route Token 校验与 HTTP/SSE/WebSocket 到 guest 端口的代理 |

## 启动与连接

Worker 按以下顺序启动：

1. 加载环境变量与 `config.toml`，校验 identity、超时、容量和 BoxLite 路径。
2. 初始化 BoxLite runtime 与共享 terminal session manager。
3. 启动可选的 sandbox proxy。
4. 连接 `WorkerRegistryService.Connect`，发送 Hello。
5. 根据 Console candidates 恢复 terminal session，并等待 recovery ack。
6. 启动 heartbeat 与命令处理循环。

连接断开不会销毁共享 manager。重连 Hello 使用 manager 的实时 reservation 数，恢复握手完成前 Worker 不接收命令。

## Capability

### `echo`

解析 `message` 并原样返回，用于验证 Console 到 Worker 的命令链路。它只受 capability inflight 限制，不创建 Box。

### `pythonExec`

每次请求创建独立 Box，在 guest 中执行 `python -c <code>`，收集 stdout、stderr 和 exit code，随后删除 Box。Deadline 或取消会终止本次执行并进入强制清理；一次性 Box 不加入 terminal session manager。

### `terminalExec`

Terminal session 使用稳定的 session ID 复用同一 Box：

- 首次请求预留容量并创建 Box；
- 后续请求在原 Box 内启动独立 shell 进程；
- lease 只延长、不缩短；
- `terminalExec` 与 `terminalResource` 共享单 session inflight；
- lease 到期、显式销毁或不可安全继续的超时会停止接收新命令并清理 Box；
- 创建中、ready、destroying 和 backend cleanup 状态均占用 active-session reservation。

### `terminalResource`

资源操作只接受已存在的 terminal session：

- `validate` 返回 MIME 与大小；
- `read` 返回受输出上限约束的文件内容；
- `export` 将文件复制到宿主机临时路径并上传到 Console 提供的签名 URL；
- 目录、缺失文件、超限文件和 guest 探测失败使用稳定的领域错误码。

## Session 恢复

Terminal Box 使用确定性标识和持久化的 BoxLite home。Worker 重连时按 Console candidate 执行：

1. 根据 session metadata 查找对应 Box。
2. 校验资源归属与运行状态，必要时重新启动。
3. 恢复原绝对 lease、代理目标和 manager reservation。
4. 报告 `RECOVERED`、`MISSING` 或 `INVALID`。
5. 清理不在 Console candidate 集合中的 Onlyboxes terminal orphan。

恢复不会创建替代 session，也不会改变 Console 保存的 lease。

## 并发与容量

Capability inflight、单 session inflight 和 active session capacity 分别检查：

- capability semaphore 限制每类命令的 Worker 级并发；
- session manager 限制同一 session 的 `terminalExec` 与 `terminalResource` 总并发；
- `terminal_max_active_sessions` 限制 manager reservation 数，`0` 表示不限；
- 已有 session 在容量满时仍可执行命令；
- 新建 session 在后端创建前原子预留容量，失败与清理完成后释放。

Hello 报告配置上限与当前 reservation 数，heartbeat 持续更新 active 数。

## Proxy

启用 proxy 后，Worker 只接受 Console 签发的短期 Route Token。代理会校验 Worker、session、guest port 与过期时间，移除内部 header，并为每个 HTTP/1.1、SSE 或 WebSocket 连接创建一条直达 Box guest port 的 BoxLite network tunnel。Terminal Box 不预先发布宿主机端口，Worker 重启恢复后也不需要重建进程内端口映射。

Session lease 到期或 Worker shutdown 会取消现有代理请求。白名单外端口、tunnel 建立失败、无效 token 和缺失 session 使用不同 HTTP 状态，便于 Nginx 与调用方诊断。

## 安全边界

- Console gRPC 默认要求 TLS；明文模式只用于可信本地网络。
- `worker_secret`、Route Token、原始命令、代码、路径和文件内容不得写入日志。
- Guest 镜像必须提供 `/bin/sh` 与 Python。
- Guest 服务只通过受 Route Token 和端口白名单保护的 BoxLite tunnel 访问，宿主机仅暴露固定 Worker proxy 入口。
- 所有超时与取消路径都必须保持 session 状态机和容量计数一致。

## 构建与验证

```bash
cargo build --release
cargo test
cargo run --example boxlite_smoke
```

完整运行参数和默认值见[配置参考](config-file.md)，对外能力与运维说明见[项目 README](../README.md)。
