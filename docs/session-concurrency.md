# Session 并发模型

Onlyboxes 通过 Console 将命令路由到 Worker。终端会话允许多个命令共享同一个沙箱文件系统，同时使用分层容量限制控制 Worker 和单个会话的负载。

```text
客户端 -> Console -> Worker -> 容器 / microVM / 远程沙箱 / 宿主机进程
```

## 容量层级

| 层级 | 作用范围 | 配置 | 容量耗尽后的结果 |
| --- | --- | --- | --- |
| 能力并发 | 单个 Worker 的单项 capability | 对应 capability 的 `WORKER_*_MAX_INFLIGHT` | `no_capacity`，HTTP `429` |
| 会话并发 | 单个 terminal session | `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` | `session_busy`，HTTP `409` |
| 活动会话数 | 单个 Worker 管理的 terminal session 总数 | `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` | `session_capacity_exceeded`，HTTP `429` |

Console 根据 Worker 声明的 capability 与 `max_inflight` 选择节点，并将已有 `session_id` 固定路由到原 Worker。单会话并发和活动会话数由 Worker 原子检查，Console 不维护单会话命令计数。

三个层级相互独立。提高单会话并发上限不会绕过 capability 配额；提高 capability 配额也不会绕过活动会话数限制。

## Terminal session 语义

`terminalExec` 在指定 session 的沙箱内启动独立 shell 进程。并发命令共享文件系统和沙箱资源，但不共享当前目录、环境变量、shell 变量或进程内状态。需要跨命令保存状态时，应写入文件或在单条命令内完成相关操作。

`terminalExec` 与 `terminalResource` 共用单会话并发计数。读取或导出文件也会占用 `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` 的一个名额。

新 session 创建期间，后续并发请求会等待同一个创建结果。创建成功后请求继续执行；创建失败时等待者收到失败结果，不会对未就绪的后端发起命令。

## 生命周期

命令超时或取消时，Worker 停止接收该 session 的新请求，并在其他在途命令退出后清理后端。这样，一条命令结束不会直接终止同 session 中仍在运行的其他命令。

| Worker | 会话后端 | 回收行为 |
| --- | --- | --- |
| worker-docker | Docker 容器 | 待销毁状态拒绝新请求；普通销毁等待在途命令结束。租约到期是硬边界，可强制移除容器 |
| worker-boxlite | BoxLite microVM | 待销毁状态拒绝新请求；Box 在在途命令结束后移除，空闲租约回收不处理仍有在途命令的 session |
| worker-bridge-e2b | E2B sandbox | 待销毁状态拒绝新请求；sandbox 在最后一条命令退出后销毁，空闲 session 由租约回收 |

创建中、可用、待销毁和后端清理中的 session 都占用活动会话容量。`WORKER_TERMINAL_MAX_ACTIVE_SESSIONS=0` 表示不限制当前 Worker 进程管理的 session 数量。

## Worker 差异

| Worker | 并发隔离边界 | 单会话默认并发 | 资源注意事项 |
| --- | --- | --- | --- |
| worker-docker | 同一容器中的独立 `docker exec` 进程 | `1` | 命令共享容器的 CPU、内存和进程数限制 |
| worker-boxlite | 同一 microVM 中的独立 guest 执行 | `1` | 命令共享 microVM 的 CPU、内存和进程数限制 |
| worker-bridge-e2b | 同一 E2B sandbox 中的独立执行 | `128` | 命令共享远程 sandbox 的资源预算 |
| worker-sys | 宿主机上的独立进程 | 不适用 | 没有容器或 cgroup 资源边界，应由宿主机提供限制 |

worker-sys 没有 terminal session 状态机。`computerUse` 与 `readImage` 分别使用独立的 capability 并发槽，默认上限均为 `1`；超限返回 `session_busy`。提高上限会让更多命令直接并发使用宿主机资源，只应在已配置 cgroup、ulimit 或等效限制的环境中使用。

## 配置

### Sandbox Worker

worker-docker、worker-boxlite 和 worker-bridge-e2b 使用以下配置：

| 环境变量 | 含义 | Docker / BoxLite 默认值 | E2B 默认值 |
| --- | --- | --- | --- |
| `WORKER_ECHO_MAX_INFLIGHT` | `echo` 能力的 Worker 级并发上限 | `4` | `128` |
| `WORKER_PYTHON_EXEC_MAX_INFLIGHT` | `pythonExec` 能力的 Worker 级并发上限 | `4` | `32` |
| `WORKER_TERMINAL_EXEC_MAX_INFLIGHT` | `terminalExec` 能力的 Worker 级并发上限 | `4` | `64` |
| `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT` | `terminalResource` 能力的 Worker 级并发上限 | `4` | `128` |
| `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` | 单 session 的 `terminalExec` 与 `terminalResource` 共享并发上限 | `1` | `128` |
| `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` | Worker 管理的活动 session 上限，`0` 表示不限 | `0` | `0` |

单 session 的有效吞吐量同时受 `terminalExec`、`terminalResource` 各自的 capability 配额约束。调整并发时，应同时评估单会话上限、两项能力上限和后端资源预算。

### System Worker

| 环境变量 | 含义 | 默认值 |
| --- | --- | --- |
| `WORKER_COMPUTER_USE_MAX_INFLIGHT` | `computerUse` 并发上限 | `1` |
| `WORKER_READ_IMAGE_MAX_INFLIGHT` | `readImage` 并发上限 | `1` |

## 错误处理

| 错误码 | HTTP | 客户端处理建议 |
| --- | --- | --- |
| `session_busy` | `409` | 等待当前 session 的命令结束后重试，或使用其他 session |
| `no_capacity` | `429` | 等待 Worker capability 容量释放后重试 |
| `session_capacity_exceeded` | `429` | 复用已有 session，等待活动 session 被回收，或增加可用 Worker 容量 |

`session_capacity_exceeded` 只用于新 session 的准入失败。已有 session 在 Worker 达到活动会话上限后仍可继续使用，并继续受 capability 和单会话并发限制。

## 设计约束

- 同一 session 的并发命令只能依赖共享文件系统，不能依赖共享 shell 状态。
- 待销毁 session 必须拒绝新命令，并在符合对应后端回收条件时释放资源。
- 创建失败必须传递给所有等待该 session 就绪的请求，且不能留下占用容量的残余 session。
- `terminalExec` 与 `terminalResource` 必须使用同一个单会话并发计数。
- Worker 上报的 capability 容量、活动会话容量与实际准入判断必须保持一致。

## 相关文档

- [Console](../console/README.md)
- [HTTP API](API.zh-CN.md)
- [worker-docker](../worker/worker-docker/README.md)
- [worker-boxlite](../worker/worker-boxlite/README.md)
- [worker-bridge-e2b](../worker/worker-bridge-e2b/README.md)
- [worker-sys](../worker/worker-sys/README.md)
