# Terminal 最大活跃 Session 限制

本文描述 sandbox Worker 对 terminal session 数量的现行限制。该限制独立于 capability inflight 和单 session 命令并发，用于约束 lease 期间持续占用的容器、microVM 或远程 sandbox 数量。

适用 Worker：

- `worker-docker`
- `worker-boxlite`
- `worker-bridge-e2b`

`worker-sys` 不管理 terminal sandbox session，不声明该容量。

## 配置

环境变量：

```text
WORKER_TERMINAL_MAX_ACTIVE_SESSIONS
```

配置文件键：

```toml
terminal_max_active_sessions = 0
```

语义：

- `0` 表示不限制当前 Worker 进程管理的 terminal session 数量，也是默认值。
- 正整数表示 Worker 可持有的最大 session reservation 数。
- 配置值必须能编码为非负 `int32`；超过 `2147483647` 时 Worker 在进入重连循环前终止启动。
- 非法或负数配置按各 Worker 的非负整数配置规则回退为 `0`。

各 Worker 的完整配置说明：

- [worker-docker](../../worker/worker-docker/docs/config-file.md)
- [worker-boxlite](../../worker/worker-boxlite/docs/config-file.md)
- [worker-bridge-e2b](../../worker/worker-bridge-e2b/docs/config-file.md)

## 计数语义

一个 terminal session 从创建预留容量开始计数，直到 manager 完成对应的销毁与后端清理路径后释放。以下状态均占用一个 reservation：

- 创建中；
- 已就绪；
- 等待并发命令结束后销毁；
- Docker container、Box 或 E2B sandbox 清理中；
- Worker 重连后成功恢复并重新纳入 manager 的 session。

容量检查与 reservation 变更由 Worker 本地 session manager 在同一同步边界内完成，并发创建不能突破正数上限。

已有 session 不需要新的容量 reservation。即使 Worker 已满，已有 session 仍可执行 `terminalExec` 和 `terminalResource`，但仍受 capability inflight 与单 session inflight 限制。

## 与并发限制的关系

| 限制 | 约束对象 |
| --- | --- |
| `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` | lease 期间由 Worker 管理的 session 总数 |
| `WORKER_TERMINAL_EXEC_MAX_INFLIGHT` | Worker 上同时执行的 `terminalExec` 命令数 |
| `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT` | Worker 上同时执行的 `terminalResource` 命令数 |
| `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` | 单个 session 内两类命令共享的并发数 |

这些限制分别检查。提高单 session 并发不会增加最大 session 数，提高最大 session 数也不会扩大命令并发配额。

## 协议上报

Sandbox Worker 在每次 `ConnectHello` 中发送：

```proto
message TerminalSessionCapacity {
  int32 max_active_sessions = 1;
  int32 active_session_count = 2;
}
```

`active_session_count` 是构建 Hello 时 manager 的 reservation 数，关闭重连后第一条 heartbeat 前的容量信息窗口。后续 heartbeat 持续报告最新计数。

Console 把该数据视为调度快照。快照属于当前 Worker connection，不写入 SQLite；Worker 重连时由新 Hello 重新初始化。

## 容量耗尽

新 session 达到上限时，Worker 在创建后端资源和执行用户命令前返回：

```text
session_capacity_exceeded
```

该错误不会创建 sandbox，也不会保留新的 route。Console 可在确认 provisional route 已安全移除后尝试其他 Worker。所有候选均拒绝时：

- REST 返回 HTTP `429`；
- Task 和 MCP 保留 `session_capacity_exceeded` 错误码；
- 已有 session 的 route 和可用性不受影响。

## 清理与恢复

- 创建失败会释放本次 reservation。
- Lease 到期、显式销毁、不可安全继续的命令超时和 Worker shutdown 按后端清理语义释放 reservation。
- 清理进行期间继续计数，防止慢清理形成无上限资源堆积。
- Worker 恢复 candidate 时，成功接管的资源重新计入 reservation；无效、缺失或孤立资源不进入 active session 集合。

## 实现入口

- [容量协议](../../api/proto/registry/v1/registry.proto)
- [Console 容量快照](../../console/internal/grpcserver/session_runtime.go)
- [Docker session manager](../../worker/worker-docker/internal/runner/terminal_exec.go)
- [BoxLite session manager](../../worker/worker-boxlite/src/runner/terminal_session_manager.rs)
- [E2B session manager](../../worker/worker-bridge-e2b/internal/runner/terminal_exec.go)
