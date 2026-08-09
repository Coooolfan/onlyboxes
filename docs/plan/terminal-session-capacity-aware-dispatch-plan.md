# Terminal Session 容量感知调度

本文描述 Console 对新 terminal session 的现行容量感知调度与安全重试语义。容量数据用于候选排序，Worker 本地 session manager 始终是最终准入边界。

## 容量数据

Sandbox Worker 在 `ConnectHello.terminal_session_capacity` 中报告配置上限与连接时的 reservation 数，并在 heartbeat 中更新 `active_session_count`。

Console 为每个 active Worker connection 保存一份容量快照：

- `known`：当前连接是否声明容量；
- `max_active_sessions`：`0` 表示不限；
- `active_session_count`：最近一次 Hello 或 heartbeat 报告的 reservation 数；
- `observed_at`：快照时间。

容量快照不持久化。Worker 重连时由 Hello 重新建立；缺少容量声明的旧 Worker 视为 unknown。

## 适用请求

只有 Console 明确知道会创建新 session 的 `terminalExec` 请求启用派发前容量分组。目前该条件对应请求未提供 `session_id`，由 Console 生成新 ID。

以下请求不做容量硬过滤：

- 已有 confirmed route；
- 用户提供 `session_id`，但 Console 无法证明该 session 一定是新建；
- 跟随另一个请求已建立的 provisional route；
- `terminalResource` 和非 terminal capability。

这样可以保持 session 的 Worker 粘性，并避免把可能已存在的同名后端资源分配到其他 Worker。

## 候选顺序

Console 先执行通用过滤：Worker 必须在线、连接已完成 recovery handshake、声明目标 capability，且 capability inflight 尚未达到 `max_inflight`。

对容量感知请求，候选按以下分组依次选择：

1. 已知有容量：`max_active_sessions == 0`，或 `active_session_count < max_active_sessions`。
2. 容量未知：当前连接未声明 `terminal_session_capacity`。
3. 已报告满载：正数上限且 `active_session_count >= max_active_sessions`。

每组内优先选择 capability inflight 较少的 Worker，并保留轮询起点以分散同负载候选。已报告满载的 Worker 不会被永久排除；它们作为最后探测，以容忍 heartbeat 滞后和刚完成的异步清理。

## Existing route

Confirmed terminal route 固定绑定原 Worker：

- Worker 已满时，已有 session 仍派发到原 Worker。
- Worker 离线或正在恢复时返回 `session_unavailable`，不改派。
- Console 重启后，从 SQLite 加载的 route 先保持 unavailable；原 Worker 完成 candidate/report/ack 恢复握手后才重新变为 ready。
- `session_not_found`、恢复失败或 lease 到期会删除持久化 route，后续请求再按正常缺失 session 语义处理。

## 安全容量重试

Console 只在同时满足以下条件时改派：

1. 请求是 `terminalExec`。
2. 当前 attempt 持有本次新 session 的 provisional route reservation。
3. Worker 明确返回执行前错误 `session_capacity_exceeded`。
4. Console 成功移除该 provisional route，且没有其他并发请求仍在使用它。

重试遵循以下约束：

- 整个逻辑请求共享原 deadline、task ID 和输入；
- 每个 Worker node 最多尝试一次；
- 每次实际派发使用新的 command ID，并把 task 更新到当前或最后一次 attempt；
- transport error、timeout、取消、`session_busy`、`session_not_found` 和一般执行错误不触发容量改派；
- 无法证明命令未执行时保留 route，避免重复创建 sandbox 或重复执行用户命令；
- 所有候选均返回容量错误时，最终结果为 `session_capacity_exceeded`。

## 错误语义

| 场景 | 结果 |
| --- | --- |
| 没有声明 capability 的在线 Worker | no compatible worker |
| 所有 compatible Worker 的 capability inflight 已满 | `no_capacity` |
| 新 session 被所有实际尝试的 Worker 拒绝 | `session_capacity_exceeded` |
| 已有 route 的 Worker 离线或恢复中 | `session_unavailable` |
| 单 session 命令并发已满 | `session_busy` |

`session_capacity_exceeded` 在 REST 中映射为 HTTP `429`。容量快照只影响候选顺序，不改变 Worker 对创建请求的原子准入判断。

## 不变量

- 已有 session 不因容量满载迁移。
- Provisional route 只属于当前创建流程，不写入 SQLite。
- Confirmed route 只有在 Worker 成功且 SQLite 提交完成后才对调用方生效。
- 容量错误重试不会同时保留两个 Worker route。
- 一个逻辑请求不会在同一 Worker 上重复 attempt。
- Task 持久化失败会终止派发流程，不继续容量重试。
- Console 不根据本地估算修改 Worker 报告的 active session 数。

## 实现入口

- [候选选择与安全重试](../../console/internal/grpcserver/service_dispatch.go)
- [容量快照](../../console/internal/grpcserver/session_runtime.go)
- [新 session intent](../../console/internal/grpcserver/task_owner_scope.go)
- [Worker 连接与 heartbeat](../../console/internal/grpcserver/service_connect.go)
- [Terminal route 状态机](../../console/internal/grpcserver/terminal_session_routes.go)
- [gRPC 容量协议](../../api/proto/registry/v1/registry.proto)
