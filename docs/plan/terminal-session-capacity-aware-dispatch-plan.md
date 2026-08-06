# Terminal Session 容量感知调度改造方案

## 1. 文档定位

本文是 [terminal-session-capacity-plan.md](./terminal-session-capacity-plan.md) 的后续阶段，聚焦以下问题：

```text
worker-A 已达到 WORKER_TERMINAL_MAX_ACTIVE_SESSIONS
worker-B 仍有 terminal session 空位
console 是否应在派发前跳过 worker-A，并在容量竞争时自动尝试 worker-B
```

前置方案已经完成三个 sandbox worker 的本地容量准入、`active_session_count` 心跳上报、`session_capacity_exceeded` 错误语义，以及 console provisional route 回滚。本方案不重复修改 worker 的容量计数规则，而是在这些能力之上增加跨 worker 的容量感知调度。

涉及的 sandbox worker：

- `worker-docker`
- `worker-boxlite`
- `worker-bridge-e2b`

`worker-sys` 没有 terminal sandbox session，不参与本方案。

## 2. 评估结论

应当增加派发前跳过，但不能只增加一条 `active_session_count >= max` 判断。推荐采用以下组合方案：

1. worker 在 Hello 中显式声明 terminal session 最大值和连接时的初始 active 数。
2. console 将容量声明视为调度提示，不作为最终一致的准入器。
3. 对 console 明确知道将创建新 session 的请求，优先选择未报告满载的 worker。
4. 已绑定的 session 继续固定路由到原 worker，不因 active 容量满载迁移。
5. worker 本地容量检查继续作为最终准入依据。
6. worker 返回 `session_capacity_exceeded` 后，仅在能够证明本次命令未执行且 provisional route 已安全回滚时，排除当前节点并有限重试其他节点。
7. 整个逻辑任务共享原始 deadline，每个 node 最多尝试一次，不对超时、断流或一般执行错误做自动重试。

这是一种“调度提示 + worker 最终裁决 + 安全失败重试”的方案。它提高集群利用率，但不试图把 console 变成 terminal session 生命周期的强一致控制面。

## 3. 当前实现评估

### 3.1 Worker 已具备最终容量裁决

三个 sandbox worker 都在创建新 session 前原子检查本地 reservation 数：

```text
max_active_sessions > 0
&& active_session_reservations >= max_active_sessions
=> session_capacity_exceeded
```

当前实现已经保证：

- 容量错误发生在 backend create 和用户命令执行之前。
- 已存在 session 不受 active session 总数上限影响。
- 创建中、ready、destroying 和 backend cleanup 中的 session 都占用 reservation。
- Heartbeat 的 `active_session_count` 返回 reservation 数。

相关实现：

- [worker-docker terminal manager](../../worker/worker-docker/internal/runner/terminal_exec.go)
- [worker-boxlite terminal manager](../../worker/worker-boxlite/src/runner/terminal_session_manager.rs)
- [worker-bridge-e2b terminal manager](../../worker/worker-bridge-e2b/internal/runner/terminal_exec.go)

因此，`session_capacity_exceeded` 是少数可以确认“没有创建 sandbox、没有执行用户命令”的 worker 错误，适合作为受限重试条件。

### 3.2 Console 只感知 capability inflight

当前 Hello 的能力声明只有：

```proto
message CapabilityDeclaration {
  string name = 1;
  int32 max_inflight = 2;
}
```

`WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` 没有进入协议。console 的选择逻辑只比较每个 capability 的：

```text
inflight < max_inflight
```

在所有可用候选中，当前算法优先选择 capability inflight 最小的 worker，同值时使用轮询。它不会读取 `active_session_count`，也不知道多少 active session 算满。

相关实现：

- [registry.proto](../../api/proto/registry/v1/registry.proto)
- [service_dispatch.go](../../console/internal/grpcserver/service_dispatch.go)
- [session_runtime.go](../../console/internal/grpcserver/session_runtime.go)

### 3.3 Heartbeat active 数目前只用于观测

Heartbeat 已包含：

```proto
int32 active_session_count = 3;
```

console 将其保存在 active worker session 中，并通过 `/api/v1/workers/inflight` 返回，但候选选择不使用该值。由于协议没有最大值，`active_session_count=4` 无法区分：

- `max=4`，已经满载；
- `max=20`，仍有容量；
- `max=0`，配置为无限制。

### 3.4 Terminal route 已具备安全回滚基础

console 会为 terminal `session_id` 建立内存路由：

```text
session_id -> node_id
```

首次派发使用带 reservation ID 的 provisional route。成功后确认路由；`session_capacity_exceeded` 会回滚新建 route，但不会清理已确认 route。现有 ABA token 和并发 provisional use 保护可以作为安全重试的基础。

相关实现：

- [terminal_session_routes.go](../../console/internal/grpcserver/terminal_session_routes.go)
- [service_dispatch.go](../../console/internal/grpcserver/service_dispatch.go)

### 3.5 Task 状态允许更新派发 command ID

一个 task 可以经历多次 worker 派发尝试。当前 `MarkTaskRunning` 允许 `running -> running`，并更新 `command_id`，因此重试不需要新增 task，也不需要新增数据库状态。

本方案定义：

- `task_id` 是逻辑请求 ID，在所有尝试中保持不变。
- 每次 worker 派发使用新的 `command_id`。
- Task API 最终暴露当前或最后一次实际派发的 `command_id`。
- 重试只会在前一次容量错误结果已经完整返回后发生，不存在两个重试 attempt 同时等待结果的情况。

相关实现：

- [tasks.sql](../../console/db/queries/tasks.sql)
- [task.go](../../console/internal/grpcserver/task.go)

## 4. 目标

1. 新建 terminal session 时，不把请求优先派发给已报告满载的 worker。
2. 当一个 worker 因容量竞争返回 `session_capacity_exceeded` 时，在同一请求 deadline 内尝试其他未尝试 worker。
3. 保持 terminal session 节点粘性，已有 session 不迁移。
4. 保持 worker 本地容量限制为最终一致性边界。
5. 支持新旧 worker 和 console 的滚动升级。
6. 保持 `max_inflight`、单 session inflight 和 active session 容量三个限制彼此独立。
7. 重试不重复执行用户命令，不产生重复 sandbox，不形成无限循环。
8. 提供足够的调度观测信息，能够区分预先跳过、容量重试和所有节点满载。

## 5. 非目标

本方案不处理：

- 不实现跨 worker 的既有 session 迁移。
- 不持久化完整 terminal session 列表。
- 不在 console 中实现强一致的 session reservation 账本。
- 不增加账号级 active session quota。
- 不增加跨进程或 E2B API Key 级全局配额。
- 不根据 CPU、内存、Docker 容器数或 E2B 账号剩余额度调度。
- 不对 transport error、timeout、`session_busy`、`session_not_found` 或一般执行错误自动重试。
- 不改变 worker 本地 `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` 的默认值和计数语义。
- 第一阶段不实现按 `active/max` 比例加权的 session 均衡。

## 6. 必须保持的调度不变量

### 6.1 已有 session 优先于容量过滤

Active session 上限只阻止创建新 session，不阻止访问已有 session。因此：

```text
已确认 session route 存在
=> 固定选择 route.node_id
=> 只检查该 worker 的 capability max_inflight
=> 不因 active_session_count >= max_active_sessions 改派
```

将已有 session 改派到其他 worker 会丢失文件系统和进程状态，属于错误行为。

### 6.2 Worker 始终是最终准入器

Heartbeat 是周期快照，存在以下竞态：

```text
heartbeat: active=3, max=4
request-A 和 request-B 同时选择该 worker
request-A 成功预留最后一个 slot
request-B 到达 worker 时已经满载
```

console 不能仅靠快照保证不超额。worker 的原子 reservation 检查必须保留，且 `session_capacity_exceeded` 必须继续发生在 backend create 之前。

### 6.3 一个节点在一次逻辑请求中最多尝试一次

重试必须维护 `attempted_node_ids`：

```text
worker-A capacity error
-> 回滚 provisional route
-> attempted += worker-A
-> 后续选择排除 worker-A
```

即使 worker 在重试期间重连，同一个 `node_id` 也不应在本次逻辑请求中再次尝试。

如果 route rollback 与下一次选择之间有其他请求重新建立了 route，选择逻辑也必须检查 `attempted_node_ids`。新 route 若再次指向已尝试 node，当前请求应停止重试并返回已经保存的容量错误，不能为了满足 route 粘性再次派发到同一 node，也不能清除其他请求建立的新 route。

### 6.4 所有 attempt 共享一个 deadline

不得为每次重试重新创建完整 timeout。应在进入 dispatch loop 前创建一次 `commandCtx`，每个 attempt 都继承同一个 deadline。

### 6.5 只有确定无副作用的结果可以重试

自动重试条件必须同时满足：

- capability 是 `terminalExec`；
- worker 明确返回 `CommandError.code=session_capacity_exceeded`；
- route 是本次新建或参与的 provisional route；
- route rollback 证明当前 reservation 已被安全移除，且没有并发请求已经确认该 route；
- context 尚未取消或超时；
- 仍有未尝试节点。

任一条件不满足时，返回当前结果，不重试。

## 7. 方案选择评估

### 7.1 方案 A：只使用 heartbeat 派发前过滤

优点：

- 实现相对直接。
- 正常情况下可以减少无效 worker dispatch。

不足：

- 当前协议没有 max，无法判断是否满载。
- Heartbeat 存在延迟，并发创建仍会竞争最后一个 slot。
- 无法覆盖旧 worker 或容量声明缺失。
- 若将快照当成强准入，可能在 worker 已释放容量但下一次 heartbeat 尚未到达时错误拒绝请求。

结论：不能单独使用。

### 7.2 方案 B：只在容量错误后尝试其他 worker

优点：

- 不需要 max 上报。
- worker 的容量错误是最终事实，正确性边界清晰。
- 可以立即改善“选中 A 失败但 B 可用”的问题。

不足：

- 已知满载节点仍会收到无意义请求。
- 每次误派增加一次 gRPC 往返和 task command ID 更新。
- 并发压力下可能反复把不同请求先派到同一个满载节点。

结论：可以作为兼容和兜底机制，但不应是唯一方案。

### 7.3 方案 C：console 维护强一致 session slot reservation

要做到强一致，console 必须知道：

- 每个 session 创建成功的时刻；
- lease 到期和 cleanup 完成的时刻；
- worker 崩溃、重连和 session 恢复状态；
- backend 残留资源和 worker 本地 reservation 的对账结果。

当前协议没有 session event/snapshot，console route 也有 TTL 且不持久化。仅在派发时对 `active_session_count` 做本地 `+1/-1` 无法正确处理长期 lease 和异步 cleanup，最终一定会漂移。

结论：不采用。

### 7.4 方案 D：容量提示 + 安全重试

该方案同时使用：

- Hello 容量声明；
- Heartbeat active 快照；
- 派发前候选分组；
- worker 本地最终检查；
- `session_capacity_exceeded` 有限重试。

它不能消除所有容量竞态，但可以避免已知误派，并在竞态发生后自动恢复，同时不要求 console 掌握完整 session 生命周期。

结论：采用方案 D。

## 8. 协议设计

### 8.1 使用带 presence 的独立消息

在 [registry.proto](../../api/proto/registry/v1/registry.proto) 中新增：

```proto
message TerminalSessionCapacity {
  // 0 means unlimited.
  int32 max_active_sessions = 1;

  // Reservation count at the moment ConnectHello is built.
  int32 active_session_count = 2;
}
```

在 `ConnectHello` 增加：

```proto
TerminalSessionCapacity terminal_session_capacity = 12;
```

不建议直接增加普通标量：

```proto
int32 terminal_max_active_sessions = 12;
```

原因是 proto3 普通标量无法区分：

- 新 worker 明确上报 `0`，表示无限制；
- 旧 worker 未实现该字段，反序列化后得到默认 `0`。

独立 message 自带 presence：

| Hello 状态 | Console 解释 |
| --- | --- |
| `terminal_session_capacity` 缺失 | 未知或旧 worker，不做硬判断 |
| message 存在且 `max_active_sessions=0` | 已知无限制 |
| message 存在且 `max_active_sessions>0` | 已知有限容量 |

### 8.2 Hello 同时携带初始 active 数

只在 Hello 上报 max、继续等待 heartbeat 提供 active 数会产生重连窗口。

三个 sandbox manager 都位于 gRPC 重连循环外。worker 与 console 断线重连时，本地 session 可以继续存在。如果 Hello 默认 active 为 0，console 在第一条 heartbeat 到达前会把满载 worker 误判为空闲。

因此 Hello 应同时携带构建时的 `active_session_count`。Heartbeat 继续作为后续更新源。

### 8.3 协议校验

新 worker 发送 Hello 前必须保证：

```text
0 <= max_active_sessions <= MaxInt32
0 <= active_session_count <= MaxInt32
```

禁止通过普通类型转换静默截断超大配置值。三个 worker 都应在进入 gRPC 重连循环前校验 `terminal_max_active_sessions <= i32::MAX`，无效配置直接使 worker 启动失败。Hello builder 仍保留防御性检查：Go 的 `buildHello` 返回 error，BoxLite 的 `build_hello` 改为返回 `Result`。这样不会把永久配置错误退化为无限重连。

console 对 message 存在但值为负数的 Hello 返回 `InvalidArgument`。后续 heartbeat 的 `active_session_count<0` 也返回 `InvalidArgument`，不能再归一化为 0 后参与容量调度。若 `max>0 && active>max`：

- 不拒绝连接，避免观测偏差导致 worker 离线；
- 将该节点视为满载；
- 输出不含敏感请求数据的结构化 warning。

`active>max` 不应在正常 worker 中出现，但可能由版本错误、计数异常或配置转换溢出导致。

### 8.4 协议兼容

新增 message field 对 protobuf wire format 是向前兼容的：

- 旧 console 会忽略新 worker 的未知字段。
- 新 console 将旧 worker 的字段缺失解释为容量未知。
- `worker-sys` 不发送该 message。

项目仍应一次性更新共享 proto 和仓库内所有生成代码，避免编译期类型不一致。

## 9. Worker 改造

Worker 本地 session manager 和容量检查不需要重构，只需把现有配置与计数接入 Hello。

### 9.1 worker-docker

修改：

- `internal/runner/runner.go`，在创建 manager 和进入重连循环前校验协议数值范围
- `internal/runner/hello_builder.go`
- `internal/runner/session_client.go` 或 Hello 构建调用参数
- 对应 runner/hello/session client 测试

Hello 值：

```text
max_active_sessions = cfg.TerminalMaxActiveSessions
active_session_count = terminalManager.ActiveSessionCount()
```

`terminalManager` 已在重连循环外创建，重连 Hello 可以读取现有 reservation 数。

### 9.2 worker-boxlite

修改：

- `src/runner/mod.rs`，在进入重连循环前校验协议数值范围
- `src/runner/hello_builder.rs`
- `src/runner/session_client.rs`
- 对应 runner/hello/session client Rust 测试

`run_session` 已是 async，可以在构建 Hello 前调用：

```rust
shared_active_session_count().await
```

然后将结果传给 `build_hello`。不得为了读取 active 数长期持有 session map mutex。

### 9.3 worker-bridge-e2b

修改：

- `internal/runner/runner.go`，在创建 manager 和进入重连循环前校验协议数值范围
- `internal/runner/hello_builder.go`
- `internal/runner/session_client.go` 或 Hello 构建调用参数
- 对应 runner/hello/session client 测试

与 Docker worker 相同，E2B manager 位于重连循环外。重连时初始 active 数尤其重要，因为远端 sandbox 在 gRPC 断线期间仍然存活。

### 9.4 worker-sys

`worker-sys` 不声明 terminal capacity。console 只对 `terminalExec` 新建 session 使用该信息，因此 worker-sys 的 `computerUse` 和 `readImage` 调度不变。

## 10. Console 运行时容量模型

### 10.1 activeSession 增加容量快照

在 [session_runtime.go](../../console/internal/grpcserver/session_runtime.go) 中增加独立快照，概念结构如下：

```go
type terminalSessionCapacitySnapshot struct {
    Known              bool
    MaxActiveSessions  int
    ActiveSessionCount int
    ObservedAt         time.Time
}
```

建议使用独立 mutex 保护 max、active 和 observedAt 的一致快照，不把该状态混入 capability inflight map。

初始化规则：

- Hello message 缺失：`Known=false`。
- Hello message 存在：`Known=true`，使用 Hello 中的 max 和 initial active。
- 每次合法 heartbeat：只更新 active 和 observedAt，不改变 max。
- 新连接替换旧连接：以新 Hello 为准，旧 snapshot 随旧 activeSession 一起销毁。

### 10.2 不持久化容量快照

不需要修改 SQLite schema：

- 调度只使用当前在线 gRPC session。
- worker 重连会重新发送 Hello。
- offline worker 不应参与候选选择。
- 持久化过期 max/active 反而容易被误用于调度。

`registry.Store` 继续持久化 node、capabilities 和 labels，不持久化动态 terminal capacity。

### 10.3 不做长期 console 本地加减计数

console 不应在派发成功后长期对 heartbeat active 数做本地 `+1`，因为它不知道 session 何时完成 cleanup。第一阶段接受 heartbeat 延迟，并依赖 worker 容量错误重试解决并发竞态。

如需减少同一 heartbeat 周期内的集中误派，可以后续增加短期 `pending terminal create attempts` 指标，但该指标只能参与排序，不能成为长期 reservation。

## 11. Session 创建意图识别

当前 `scopeTaskInputByOwner` 会在用户省略 `session_id` 时生成 UUID，并设置 `create_if_missing=true`。到达 `dispatchCommand` 时，所有 terminal 请求都已经带有 session ID，仅解析 payload 无法判断 ID 是否由 console 生成。

应在 owner scope 阶段产生内部 dispatch metadata：

```go
type terminalSessionIntent int

const (
    terminalSessionIntentUnknown terminalSessionIntent = iota
    terminalSessionIntentKnownNew
)
```

`scopeTaskInputByOwner` 或新的包装 helper 返回：

```go
type scopedTaskInput struct {
    PayloadJSON           []byte
    TerminalSessionIntent terminalSessionIntent
}
```

调用链再通过明确的内部参数传递 task 上下文，避免继续扩大位置参数：

```go
type dispatchOptions struct {
    TaskID                string
    TerminalSessionIntent terminalSessionIntent
}
```

`TaskID` 只用于 attempt 观测和日志；echo 等非 task 调用传零值 options。`attempted_node_ids` 由单次 `dispatchCommand` 循环内部维护，不暴露给 HTTP 或 worker payload。

判定规则：

| 请求状态 | Intent | 派发前容量过滤 |
| --- | --- | --- |
| 用户省略 `session_id`，console 生成新 ID | `known_new` | 是 |
| 用户提供 ID，route 已确认 | existing route | 否 |
| 用户提供 ID，route 缺失，`create_if_missing=true` | `unknown` | 不做硬过滤 |
| 用户提供 ID，route 缺失，`create_if_missing=false` | `unknown` | 否 |
| route 已是其他请求创建的 provisional route | follow provisional route | 否，继续同节点 |

用户提供 ID 但 route 缺失时，session 可能因为 console 重启或 route TTL 已不在内存中，却仍真实存在于某个 worker。此时基于 active 满载直接排除节点，可能错过真正持有 session 的 worker，甚至在另一个 worker 创建同名 session。因此只对 `known_new` 做明确的派发前容量分组。

该 metadata 只存在于当前 task runtime，不需要数据库字段。console 重启时本来就会把非终态 task 标记为 `console_restarted`。

## 12. 候选选择算法

### 12.1 保留现有基础过滤

候选仍必须满足：

1. worker 在线；
2. worker 声明 `terminalExec`；
3. active gRPC session 存在；
4. `terminalExec.inflight < terminalExec.max_inflight`；
5. node 不在本次请求 `attempted_node_ids` 中。

如果 capability slot 全满，继续返回现有 `ErrNoWorkerCapacity` / `no_capacity`，不与 session 数量容量混淆。

### 12.2 对 known-new 请求分三组

对没有既有 route 的 `known_new` 请求，将通过基础过滤的候选分组：

1. `known_available`
   - capacity message 存在；
   - `max=0`，或 `active < max`。
2. `unknown`
   - 旧 worker 或没有 capacity message。
3. `reported_full`
   - capacity message 存在；
   - `max>0 && active>=max`。

选择顺序：

```text
known_available -> unknown -> reported_full
```

`reported_full` 不是永久黑名单，而是最后兜底组。只有前两组没有可派发候选时才探测该组。该组内仍遵守“每个 node 最多一次”，而不是整个组只允许一次 attempt。这样同时满足：

- 有其他容量候选时跳过已知满载 worker；
- 所有快照都显示满载时，仍允许 worker 侧探测，避免刚释放容量但 heartbeat 尚未更新造成错误拒绝；
- 旧 worker 在滚动升级期间仍可工作。

在每个组内部继续使用现有策略：

```text
capability inflight 最小
-> 同 inflight 时 round-robin
-> tryAcquireCapability 原子获取 slot
```

Snapshot 后的 `tryAcquireCapability` 可能因并发竞争失败。实现必须继续尝试同组其他候选，再降级到下一组；只有三个组内所有未排除候选都无法原子获取 capability slot 时，才返回 `ErrNoWorkerCapacity`。

第一阶段不按 `active/max` 比例改变权重，避免同时引入新的长期 session 均衡策略。容量不同的 worker 仍会在满载后自然退出首选组。

### 12.3 已有 route 不进入容量分组

如果 `session_id` 已映射到 node：

- 直接调用 `pickSessionForNodeAndCapability`；
- active session 满载不影响选择；
- capability inflight 满载仍返回 `no_capacity`；
- node 不在线时沿用现有 stale route 清理逻辑。

## 13. 容量错误后的有限重试

### 13.1 拆分单次 attempt

建议把当前 `dispatchCommand` 拆成两层：

```go
func (s *RegistryService) dispatchCommand(...) (commandOutcome, error) {
    // 创建一次总 deadline，维护 attempted nodes，并执行有限循环。
}

func (s *RegistryService) dispatchCommandAttempt(...) (dispatchAttemptResult, error) {
    // 选择一个 worker，注册 pending command，enqueue，并等待一次结果。
}
```

单次 attempt 负责完整释放：

- capability inflight slot；
- pending command；
- provisional route use；
- attempt 级 command ID。

前一次 attempt 完整结束后才能开始下一次。

### 13.2 Retry 判定

概念流程：

```text
attempt worker-A
-> 收到 session_capacity_exceeded
-> 判断 route 是否是本次 provisional reservation
-> 回滚 route reservation
-> 只有 rollback 返回 removed 时允许重试
-> attempted_node_ids 加入 worker-A
-> 选择下一个 node
-> 使用同一 payload、task_id 和总 deadline 派发新 command_id
```

`clearTerminalSessionRouteReservation` 当前无返回值，应改为返回明确结果，例如：

```go
type routeReservationReleaseResult int

const (
    routeReservationNotOwned routeReservationReleaseResult = iota
    routeReservationStillInUse
    routeReservationRemoved
)
```

只有 `routeReservationRemoved` 允许自动重试。

Rollback 与下一次 pick 之间仍可能有其他请求为同一 session 建立新 route。下一次 pick 的处理规则是：

- 新 route 指向未尝试 node：可以加入该 route，继续正常 attempt。
- 新 route 指向已尝试 node：停止重试并返回已保存的容量错误。
- 不得为了当前 retry 清除或改写其他请求刚建立的 route。

这项检查必须位于 route claim 和 node capability acquire 的同一决策路径中，不能只在普通候选列表中过滤 node。

### 13.3 并发 provisional route

两个请求可能同时使用同一个尚未确认的 session ID：

```text
request-A reserves session-X -> worker-1
request-B joins the same provisional route
```

如果 request-A 收到容量错误，但 request-B 仍在执行：

- A 的 rollback 只能减少 `ProvisionalUses`；
- A 不得立刻在 worker-2 创建同名 session；
- A 返回当前容量错误；
- 如果 B 成功，它确认 worker-1 route；
- 如果 B 也失败，最后一个 rollback 可以移除 route，最后一个请求才有资格重试。

该规则优先保证不会跨 worker 重复创建 session。它不保证所有并发调用都共享最终重试结果；route 级 singleflight 属于后续优化。

### 13.4 不重试的情况

以下结果直接结束当前逻辑请求：

- worker 成功返回；
- `session_busy`；
- `session_not_found`；
- `invalid_payload`；
- backend create/start 一般失败；
- 用户命令返回非零 exit code；
- stream 断开或 enqueue 失败；
- context canceled/deadline exceeded；
- 已确认 route 返回任何错误；
- provisional route 已被其他并发请求确认或仍在使用；
- 所有 node 都已尝试。

特别是 transport error 不能自动重试，因为 console 无法判断 worker 是否已经创建 session或执行命令。

### 13.5 重试耗尽后的错误

如果至少一个 worker 明确返回 `session_capacity_exceeded`，且没有未尝试且当前可派发的候选，则保留该错误：

```text
error_code: session_capacity_exceeded
HTTP: 429
```

不要把它改写为：

- `no_worker`；
- `no_capacity`；
- 通用 `execution_failed`。

这样调用方能够区分 session 数量容量与 capability 命令并发容量。

## 14. Task、Command ID 与取消语义

### 14.1 Task 状态

逻辑 task 的状态流程保持：

```text
queued -> dispatched -> running -> terminal
```

容量重试发生在 `running` 内部。每次成功 enqueue 到 worker 后调用现有 `markTaskRunning(taskID, commandID)`：

- 第一次 attempt 写入 worker-A command ID；
- 第二次 attempt 更新为 worker-B command ID；
- 最终 Task API 暴露最后一次实际 attempt 的 command ID。

不新增 `retrying` task 状态，避免扩大 API 和数据库状态机。

`onDispatched` 应改为可以把 task 持久化错误返回给 dispatch loop，或保证 loop 在进入下一次 attempt 前检查由该 callback 触发的 context cancel。`markTaskRunning` 失败后不得继续容量重试，否则 worker attempt 会继续增加，而 task 已无法可靠记录当前 command ID。

### 14.2 Request ID 幂等

重试发生在一个已存在的 task 内，不创建新 task，因此 `request_id` 仍按原逻辑去重。相同 `request_id` 的调用不会因为内部 worker retry 产生第二个逻辑任务。

### 14.3 取消与超时

- Task cancel 取消共享的 task context。
- 正在等待 worker 结果的 attempt 按现有逻辑结束。
- attempt 之间发现 context 已取消时立即停止，不再选 worker。
- retry 不重置 `timeout`。
- 如果容量重试消耗了剩余 deadline，最终状态仍为 `timeout`，而不是无限延长请求。

### 14.4 SubmitTask 预检查

当前 `checkCapabilityAvailability` 在创建 task 前只检查 online capability 和 `max_inflight`。本方案第一阶段不把 heartbeat session capacity 加入该预检查，原因是：

- active 快照不是最终事实；
- 在 worker 侧返回容量错误时，现有 API 会创建一个可查询的 failed task；
- 在 task 创建前直接返回错误会改变 Task API 生命周期和幂等记录行为。

Session capacity 分组和重试统一放在 task 已创建后的 dispatch 阶段。

## 15. API 与观测

### 15.1 错误 API

现有映射继续使用：

- REST terminal：`session_capacity_exceeded -> 429`；
- Task：`status=failed`，保留 error code/message；
- MCP：tool error 中保留 `session_capacity_exceeded`。

不需要新增外部错误码。

### 15.2 Worker inflight API

建议扩展 `/api/v1/workers/inflight`，在保留现有 `active_session_count` 的同时增加：

```json
{
  "node_id": "worker-a",
  "active_session_count": 3,
  "terminal_session_capacity": {
    "known": true,
    "max_active_sessions": 4
  },
  "capabilities": []
}
```

语义：

- `known=false`：legacy/unknown，不显示为 unlimited；
- `known=true,max=0`：明确 unlimited；
- `known=true,max>0`：可显示 `active/max`。

核心调度实现不依赖 Web UI，但 dashboard 后续可把 Active Sessions 从单值改为：

```text
3 / 4
3 / unlimited
3 / unknown
```

Web 变更应作为独立提交，避免阻塞协议与 console 调度上线。

### 15.3 结构化日志

console 至少记录以下事件，不记录 command、代码、路径或原始 payload：

- `terminal capacity candidate skipped`
  - node ID
  - active
  - max
  - task ID
- `terminal capacity retry`
  - task ID
  - previous node ID
  - next attempt index
- `terminal capacity retry exhausted`
  - task ID
  - attempted node count
- `worker terminal capacity invariant violated`
  - node ID
  - active
  - max

项目当前没有统一 Prometheus 指标框架，本方案不为此单独引入 metrics 依赖。可先依赖结构化日志和 inflight API。

## 16. 竞态与故障分析

### 16.1 Heartbeat 显示可用，但 worker 已满

处理：worker 返回容量错误，console 回滚 provisional route 并尝试其他 node。

### 16.2 Heartbeat 显示满载，但 worker 刚释放 slot

处理：reported-full 只排在最后，不永久排除。没有其他候选时仍允许探测，worker 可以成功接单。

### 16.3 Hello 与第一条 heartbeat 之间

处理：Hello 携带连接时 active 数。即使构建 Hello 后 janitor 释放了 session，最多暂时高估；reported-full last-resort 仍可探测。

### 16.4 两个请求竞争最后一个 slot

处理：两者都可能选择同一 worker，worker 原子准入只允许一个创建；另一个收到容量错误后尝试其他 node。

### 16.5 Worker 在返回容量错误后立即获得空位

处理：本次请求仍排除该 node，避免循环。其他请求可以使用新空位。

### 16.6 Worker 在 enqueue 后断流

处理：不自动重试。执行状态不明确，重试可能重复创建 session 和执行命令。沿用现有 unavailable/dispatch failure 语义。

### 16.7 Console 重启后 route 丢失

处理：用户提供的 session ID 被标记为 intent unknown，不做强容量过滤。该问题的完整解决仍需要 worker session snapshot 和 route 重建，不属于本方案。

### 16.8 新旧 worker 混部

处理：capacity message 缺失归入 unknown。已知可用 worker 优先，unknown 次之，reported-full 最后。旧 worker 返回已有的容量错误时仍可进入安全重试路径。

### 16.9 所有 worker 满载

处理：按分组顺序耗尽候选，reported-full 允许最后探测。所有实际 attempt 均返回容量错误后，task 失败为 `session_capacity_exceeded` / HTTP 429，route 不残留。

## 17. 测试计划

### 17.1 Proto 与兼容性

1. 新 message presence 能区分 absent 与 present/max=0。
2. Go 生成代码与四个 Go consumer（console、worker-docker、worker-bridge-e2b、worker-sys）编译通过。
3. BoxLite tonic/prost 构建生成新字段。
4. 新 worker Hello 可被旧字段集合的测试 server 接收。
5. 新 console 接收缺失 capacity message 的 legacy Hello。
6. Hello 中负 max/active 被拒绝，超大配置不会静默截断。
7. Heartbeat 中负 `active_session_count` 被拒绝，不进入调度快照。

### 17.2 Worker Hello

三个 sandbox worker 分别覆盖：

1. `max=0` 时仍发送存在的 capacity message。
2. 正数 max 原样上报。
3. 初次连接 active=0。
4. gRPC 重连且 manager 持有 session 时，Hello 上报非零 initial active。
5. Heartbeat 继续更新 active，不改变 max。
6. worker-sys 不发送 terminal capacity message。

### 17.3 Console 候选选择

1. A reported-full、B known-available，known-new 请求只派发到 B。
2. A known-available、B unknown，优先 A。
3. 只有 unknown worker 时保持可派发。
4. 所有 worker reported-full 时仍选择一个 last-resort probe。
5. `max=0` 被视为 known available。
6. capability `max_inflight` 满载仍被排除。
7. reported-full 但持有已确认 route 时，已有 session 请求仍派发到该 node。
8. `terminalResource` 不使用新 session 容量过滤。
9. echo/pythonExec/computerUse/readImage 选择逻辑不变。
10. 排除集合确保同 node 不被二次尝试。
11. 首选组 candidate 在 snapshot 后抢占失败时，会继续同组并降级到下一组。

### 17.4 容量重试

1. worker-A 返回容量错误，worker-B 成功，同一请求最终成功。
2. A、B 都返回容量错误，最终保留 `session_capacity_exceeded`。
3. A 返回一般 execution error，不尝试 B。
4. A transport 断开，不尝试 B。
5. A capacity error 后 context 到期，不尝试 B。
6. 每次 attempt 使用不同 command ID。
7. task ID 和 request ID 在所有 attempt 中不变。
8. Task API 最终 command ID 是最后一次 attempt。
9. 前一个 attempt 的 capability inflight 在重试前释放。
10. 总执行时间不超过原始 deadline。
11. 重试成功后 route 确认到 worker-B。
12. 所有重试失败后不残留 route。
13. `markTaskRunning` 持久化失败后不进入下一次 attempt。

### 17.5 Provisional route 并发

1. 两个请求共享 provisional route，其中一个 capacity error 时不能迁移到第二 node。
2. 一个请求成功确认 route 后，另一个较晚 rollback 不清除已确认 route。
3. 所有 provisional users 都失败时，只有最后一个成功移除 reservation 的请求可重试。
4. ABA reservation ID 保护在重试后仍成立。
5. 同一 session ID 不会同时在两个 fake worker 上收到创建命令。
6. Rollback 后第三个请求重新把 route 指向已尝试 node 时，当前请求停止 retry 且不破坏新 route。
7. Race detector 下 route use、pending command 和 capability inflight 不泄漏。

### 17.6 HTTP、Task 与 MCP

1. 容量 retry 成功时 REST/MCP 返回成功，不暴露中间错误。
2. retry 耗尽时 REST 返回 429。
3. Task 返回 failed + `session_capacity_exceeded`。
4. MCP tool error 保留容量错误码和消息。
5. async/auto/sync 三种 task mode 的最终状态一致。
6. cancel 发生在 attempt 之间时不再派发。

### 17.7 验证命令

```bash
(cd api && ./scripts/gen-go.sh)
go -C api test ./...
go -C console test ./...
go -C worker/worker-docker test -race ./...
cargo test --manifest-path worker/worker-boxlite/Cargo.toml
go -C worker/worker-bridge-e2b test -race ./...
go -C worker/worker-sys test ./...
```

需要增加一个 console 双 fake-worker 集成用例，真实走：

```text
Connect Hello capacity
-> heartbeat active
-> SubmitTask terminalExec
-> worker-A capacity result
-> worker-B success result
-> task/result/route assertion
```

三个 backend 的容量准入已经在前置方案中完成真实验证。本方案不改变 backend lifecycle，发布前仍应各执行一次 terminal create/reuse/cleanup smoke，确认 Hello 改造没有影响连接流程。

## 18. 实施文件范围

### 18.1 API

- `api/proto/registry/v1/registry.proto`
- `api/gen/go/registry/v1/registry.pb.go`，由脚本生成
- `api/README/proto.md`

### 18.2 Console

预计修改：

- `console/internal/grpcserver/service.go`
- `console/internal/grpcserver/session_runtime.go`
- `console/internal/grpcserver/service_connect.go`
- `console/internal/grpcserver/service_dispatch.go`
- `console/internal/grpcserver/terminal_session_routes.go`
- `console/internal/grpcserver/task_owner_scope.go`
- `console/internal/grpcserver/task.go`
- `console/internal/grpcserver/connect_service_test.go`
- `console/internal/grpcserver/task_owner_scope_test.go`
- `console/internal/grpcserver/task_persistence_test.go`
- `console/internal/httpapi/worker_stats.go`
- 对应 HTTP/MCP/stats 测试

不需要数据库 migration。

### 18.3 Workers

`worker-docker`：

- `internal/runner/runner.go`
- `internal/runner/hello_builder.go`
- `internal/runner/session_client.go`
- 对应测试

`worker-boxlite`：

- `src/runner/mod.rs`
- `src/runner/hello_builder.rs`
- `src/runner/session_client.rs`
- `build.rs` 通常无需修改，编译时会重新生成 Rust proto
- 对应测试

`worker-bridge-e2b`：

- `internal/runner/runner.go`
- `internal/runner/hello_builder.go`
- `internal/runner/session_client.go`
- 对应测试

Worker terminal manager 只作为 active count 数据源，不改容量生命周期逻辑。`worker-sys` 无需发送该字段，但共享 proto 更新后必须执行完整编译与测试，并覆盖 console 对 worker-sys Hello 清除或忽略 terminal capacity 声明的行为。

### 18.4 文档与可选 Web

- `README/API.md`
- `README/API.zh-CN.md`
- `console/README/overview.md`
- 三个 worker 的 `README/overview.md`
- 可选：`web/src/types/workers.ts`
- 可选：workers API service/store/table 与测试

## 19. 实施顺序与提交边界

按项目边界拆分：

1. `api`：新增 message 和生成 Go 代码。
2. `worker-docker`：Hello 上报 max + initial active。
3. `worker-boxlite`：Hello 上报 max + initial active。
4. `worker-bridge-e2b`：Hello 上报 max + initial active。
5. `console`：解析 capacity、容量候选分组、有限重试、测试。
6. `console` API：扩展 inflight 观测字段。
7. `web`：可选展示 active/max。
8. 根 API 与各项目 README 同步。

协议提交必须先落地，随后每个工程都应在自己的提交中独立构建和测试。

## 20. 发布与回滚

### 20.1 推荐发布顺序

1. 先发布带新 Hello 字段的三个 sandbox worker。
2. 旧 console 会忽略未知 protobuf 字段，调度行为不变。
3. 确认新 worker 连接和 heartbeat 正常。
4. 再发布容量感知 console。
5. 观察容量 skip、retry 和 retry exhausted 日志至少一个最大 terminal lease 周期。
6. 最后发布可选 dashboard 展示。

### 20.2 混合版本行为

| Console | Worker | 行为 |
| --- | --- | --- |
| 旧 | 新 | 忽略 capacity message，保持旧调度 |
| 新 | 旧 | capacity unknown，仍可派发并使用错误后重试 |
| 新 | 新 | 完整容量感知调度 |
| 旧 | 旧 | 当前行为 |

### 20.3 回滚

- 回滚 console：新 worker 的额外 Hello 字段被旧 console 忽略。
- 回滚某个 worker：新 console 将其视为 capacity unknown。
- 将 worker `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS=0`：message 仍存在，console 明确识别为 unlimited。
- 不需要数据库回滚。

## 21. 风险与控制

| 风险 | 控制 |
| --- | --- |
| Heartbeat 滞后导致误判 | worker 最终准入 + reported-full last-resort + 容量错误重试 |
| 并发请求同时选择最后一个 slot | worker 原子 reservation，失败请求改派 |
| 重试重复执行命令 | 只重试 worker 明确的 pre-execution capacity error |
| 同 session 跨 worker 重复创建 | 仅在 provisional reservation 被独占移除后重试 |
| transport failure 执行状态不明 | 不重试 transport/timeout/cancel |
| 旧 worker 的 `0` 被误判 unlimited | 使用 message presence 区分 unknown 与 explicit zero |
| 重连后第一条 heartbeat 前低估 active | Hello 携带 initial active |
| task command ID 多次变化 | 每次 attempt 更新，API 定义为当前/最后 attempt |
| console 本地容量计数漂移 | 不维护长期本地 reservation，以 heartbeat 为观测源 |
| reported-full worker 刚释放容量却被跳过 | 无其他候选时允许最后探测 |
| 新策略改变非 terminal 能力分布 | 将 capacity policy 限定为 known-new terminalExec |

## 22. 验收标准

- [x] Proto presence 能明确区分 legacy unknown 与 configured unlimited。
- [x] 三个 sandbox worker 都在进入重连循环前拒绝超过协议范围的 max 配置，并在 Hello 中上报 max 和 initial active。
- [x] Worker 重连且保留 session 时，console 在第一条 heartbeat 前获得正确初始 active 数。
- [x] known-new terminal 请求在有其他候选时跳过 reported-full worker。
- [x] 已确认 terminal session 在 worker 满载时仍固定路由并可执行。
- [x] capability `max_inflight` 与 active session 容量保持独立错误语义。
- [x] 容量竞争时，同一请求会尝试其他未尝试 worker。
- [x] 每个 node 在一次逻辑请求中最多尝试一次，包括其他请求在 retry 间隙重建 route 的情况。
- [x] 重试共享原始 deadline，取消后不继续派发。
- [x] transport、timeout、session_busy 和一般执行错误不会自动重试。
- [x] 并发 provisional route 不会导致同 session 在多个 worker 重复创建。
- [x] 重试成功后 route 指向最终成功 worker。
- [x] 重试耗尽后 route 不残留，错误仍为 `session_capacity_exceeded` / HTTP 429。
- [x] Task API 的 task ID/request ID 保持不变，command ID 指向最后一次实际 attempt。
- [x] 新旧 console/worker 四种组合均能按兼容矩阵工作。
- [x] `worker-sys` 不声明 terminal capacity，且共享 proto 更新后构建与测试通过。
- [x] Console、三个 sandbox worker 和 BoxLite Rust 测试全部通过。
- [x] 双 worker 集成测试覆盖派发前 skip 和 worker 容量错误后的成功改派。

### 22.1 验证记录

实现完成后已执行：

```bash
(cd api && ./scripts/gen-go.sh)
go -C api test ./...
go -C console test ./...
go -C console test -race ./internal/grpcserver
go -C console test -race -timeout 20m ./internal/httpapi
go -C worker/worker-docker test -race ./...
cargo test --manifest-path worker/worker-boxlite/Cargo.toml
cargo clippy --manifest-path worker/worker-boxlite/Cargo.toml --all-targets -- -D warnings
cargo test --manifest-path worker/worker-boxlite/Cargo.toml --features integration --test integration -- --test-threads=1
go -C worker/worker-bridge-e2b test -race ./...
go -C worker/worker-sys test ./...
```

运行时验证还覆盖：

- 两个真实 gRPC worker stream 的派发前 skip 与容量错误改派集成测试。
- Docker worker 使用 `max_active_sessions=1` 连接本地 console，inflight API 返回 `known=true,max=1`。
- Docker terminal 首次创建成功、第二个新 session 返回 `429 session_capacity_exceeded`、满载时原 session 复用成功。
- Worker SIGTERM 后其管理的 terminal container 已删除，临时 worker 与 token 记录已清理。
- BoxLite 三个真实 integration 用例按单线程执行，覆盖 Python、terminal create/reuse 与 VM cleanup。

## 23. 后续方向

本方案稳定后可以单独评估：

1. 按 `active/max` 比例或资源权重进行长期 session 均衡。
2. Route 级 singleflight，让并发创建同一 session 的调用共享一次 worker attempt 和重试结果。
3. Worker session snapshot/event，用于 console 重启后的 route 重建。
4. 账号级 active session quota 和公平调度。
5. Worker backend reconciliation，发现 Docker、BoxLite 或 E2B 残留 sandbox。
6. Provider/API Key 级全局容量视图。

这些能力需要更完整的 session 生命周期协议，不应与本次容量感知调度一起实现。
