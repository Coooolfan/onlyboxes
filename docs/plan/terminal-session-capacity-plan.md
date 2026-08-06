# Terminal 最大活跃 Session 数量限制改造计划

## 1. 背景

Onlyboxes 的 `terminalExec` 会在 worker 中创建或复用一个有状态 sandbox session。同一个 `session_id` 对应一个 Docker 容器、BoxLite microVM 或 E2B sandbox，并在 lease 到期后回收。

当前三个 sandbox worker 已经具备以下并发限制：

| 配置 | 作用域 |
| --- | --- |
| `WORKER_PYTHON_EXEC_MAX_INFLIGHT` | 单个 worker 上并发执行的 `pythonExec` 数量 |
| `WORKER_TERMINAL_EXEC_MAX_INFLIGHT` | 单个 worker 上并发执行的 `terminalExec` 数量 |
| `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT` | 单个 worker 上并发执行的 `terminalResource` 数量 |
| `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` | 单个 terminal session 内共享的 `terminalExec` 与 `terminalResource` 并发数量 |

这些限制只约束正在执行的命令，不约束处于 lease 内的 terminal session 总数。用户可以串行创建任意数量的 session，使 worker 长时间保留大量容器、microVM 或远程 E2B sandbox。

本计划增加独立的 terminal session 数量上限，同时保留所有现有 capability 和单 session 并发限制。

## 2. 目标

新增统一配置：

```text
WORKER_TERMINAL_MAX_ACTIVE_SESSIONS
```

对应 `config.toml` 键：

```toml
terminal_max_active_sessions = 0
```

该配置适用于：

- `worker-docker`
- `worker-boxlite`
- `worker-bridge-e2b`

目标行为：

1. 限制单个 worker 进程同时管理的 terminal session 数量。
2. 容量已满时拒绝创建新 session。
3. 容量已满时仍允许调用已有 session。
4. 创建中、可用、待销毁和底层清理中的 session 均占用容量。
5. session 创建失败、lease 回收、延迟销毁或 worker shutdown 后正确释放容量。
6. heartbeat 的 `active_session_count` 与实际容量占用保持一致。
7. 保持现有 capability `max_inflight` 和 `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` 语义不变。

## 3. 非目标

本次不处理以下内容：

- 不删除或弃用任何现有 `*_MAX_INFLIGHT` 配置。
- 不让 `pythonExec` 与 terminal session 共享容量池。
- 不修改 `worker-sys`。该实现没有 terminal sandbox session，且不具备 sandbox 级 CPU/Memory 隔离。
- 不增加 console 的 session 列表、详情或持久化查询 API。
- 不增加 session snapshot/event 协议。
- 不让 console 基于 session 剩余容量进行 worker 调度。
- 不实现跨 worker、跨进程或跨 E2B API Key 的全局 session 配额。
- 不改变 terminal lease 的最小值、最大值或续期语义。

## 4. 配置语义

### 4.1 默认值

第一阶段默认值为 `0`：

```text
0 = 不限制 terminal session 数量
N > 0 = 最多同时管理 N 个 terminal session
```

使用 `0` 可以保持未配置部署的现有行为。生产环境应根据资源预算显式配置正数。

不能使用 `WORKER_TERMINAL_EXEC_MAX_INFLIGHT` 作为默认 session 数量。前者限制并发命令，后者限制 lease 内存活的 sandbox，两者没有等价关系。

### 4.2 Active Session 定义

以下状态均计入 active session 容量：

| 状态 | 是否计入 | 原因 |
| --- | --- | --- |
| `creating` | 是 | 已预留一个即将创建的 sandbox |
| `ready + idle` | 是 | sandbox 仍存活并占用资源或配额 |
| `ready + inflight` | 是 | sandbox 正在执行命令 |
| `destroying + inflight` | 是 | sandbox 仍需等待已有命令排空 |
| backend cleanup in progress | 是 | 容器、microVM 或远程 sandbox 尚未完成清理尝试 |
| cleanup attempt completed | 否 | worker 已完成其可执行的本次清理责任 |

容量计数是 worker 管理的 reservation 数，不应简单等于 session map 中非 `destroying` 项的数量。

### 4.3 容量检查顺序

处理 `terminalExec` 时按以下顺序判断：

1. 请求指向一个存在且可用的 session：不检查 session 总容量，只检查单 session inflight。
2. 请求指向一个不存在的 session，且 `create_if_missing=false`：返回 `session_not_found`，不暴露容量状态。
3. 请求省略 `session_id`，或指定不存在的 session 且 `create_if_missing=true`：检查并预留 session 容量。
4. 容量预留成功后才调用 backend 创建 sandbox。

同一个 `session_id` 的并发创建只能产生一个 session reservation。后续调用等待第一个创建结果，不重复消耗容量。

### 4.4 错误语义

新增 worker 领域错误：

```text
code: session_capacity_exceeded
message: terminal session capacity exceeded
```

错误映射：

| 接口 | 行为 |
| --- | --- |
| Worker command result | `CommandError{code="session_capacity_exceeded"}` |
| REST terminal API | HTTP `429 Too Many Requests` |
| Task API | task `failed`，保留原始 error code/message |
| MCP | tool error，保留可识别的 error code/message |

该错误不得复用 `session_busy`：

- `session_busy` / HTTP `409` 表示目标 session 的命令 inflight 已满。
- `session_capacity_exceeded` / HTTP `429` 表示 worker 无法再创建新 terminal session。
- `no_capacity` / HTTP `429` 继续表示 console 根据 capability `max_inflight` 判断所有候选 worker 均无命令容量。

## 5. 通用实现约束

### 5.1 Reservation 必须与创建原子关联

每个 worker 的 session manager 必须在保护 session map 的同一临界区内完成：

1. 判断 session 是否已经存在。
2. 判断是否允许创建。
3. 检查最大活跃 session 数。
4. 增加 reservation。
5. 插入 creating session。

禁止先检查计数、释放锁、再插入 session；该流程会允许并发请求同时越过上限。

### 5.2 Reservation 只能释放一次

所有 session 清理路径必须汇入统一的 retire/cleanup 流程。清理流程至少携带：

- session ID
- backend sandbox/container/box ID
- 是否持有 session capacity reservation

以下路径必须覆盖：

- backend create 失败
- backend start 失败
- terminalExec 超时或取消
- terminalResource 超时或发现 sandbox 丢失
- 延迟销毁的最后一个 inflight 命令退出
- janitor 回收 lease 已过期 session
- worker 正常 shutdown
- worker 启动后 manager 初始化失败的回滚路径

### 5.3 释放时机

严格顺序：

```text
从可路由 session map 中移除
-> 执行 backend cleanup（或确认没有创建出 backend 资源）
-> 释放 session capacity reservation
```

不能在从 map 删除后立即释放 reservation，否则 backend 清理期间可能创建新 sandbox，并短暂突破实际资源上限。

backend cleanup 失败时仍应在完成本次有界清理尝试后释放 reservation，并记录结构化告警。永久保留 reservation 会使 worker 在一次清理故障后永久损失容量。

底层残留资源的发现与对账不在本次范围内，应作为后续 reconciliation 能力处理。

### 5.4 Heartbeat

三个 sandbox worker 当前均通过 heartbeat 上报 `active_session_count`。改造后该字段应返回 capacity reservation 数，包括 `destroying` 和 cleanup in progress 状态。

该字段仍是观测数据，不是 console 的最终准入依据。第一阶段不在协议中上报 `max_active_sessions`，console 不根据该值进行容量调度。

## 6. worker-docker 实施计划

### 6.1 配置

修改：

- `worker/worker-docker/internal/config/config.go`
- `worker/worker-docker/internal/config/config_test.go`
- `worker/worker-docker/internal/config/source_test.go`
- `worker/worker-docker/config.example.toml`
- `worker/worker-docker/README/overview.md`
- `worker/worker-docker/README/config-file.md`

新增：

```go
TerminalMaxActiveSessions int
```

`worker-docker` 当前的 `positiveInt` 不接受 `0`，需要增加或复用非负整数解析函数。

### 6.2 Session Manager

修改 `worker/worker-docker/internal/runner/terminal_exec.go`：

- `terminalSessionManagerConfig` 增加 `MaxActiveSessions`。
- `terminalSessionManager` 增加最大值与当前 reservation 计数。
- `claimSession` 只在确实创建新 session 时预留容量。
- `newSessionLocked` 只接受已经成功预留容量的创建路径。
- `ActiveSessionCount` 返回 reservation 计数。
- janitor、延迟销毁和 `Close` 通过统一 helper 清理容器并释放 reservation。
- 创建失败且容器未成功创建时直接释放 reservation。

`terminalResource.go` 不需要增加独立容量检查，因为它只 claim 已有 session；需要增加回归测试，确保满载时 resource 操作仍可执行。

`python_runtime.go` 不修改。

### 6.3 Docker 特有风险

- `docker rm -f` 有独立清理超时；reservation 应在该调用返回后释放。
- cleanup 失败可能留下带 `onlyboxes.managed=true` 标签的容器，但本次仍释放 reservation。
- session map 删除与容器实际删除之间不能开放容量。
- shutdown 批量清理时应逐个释放 reservation，最终计数必须为零。

## 7. worker-boxlite 实施计划

### 7.1 配置

修改：

- `worker/worker-boxlite/src/config.rs`
- `worker/worker-boxlite/src/config_source.rs`
- `worker/worker-boxlite/config.example.toml`
- `worker/worker-boxlite/README/overview.md`
- `worker/worker-boxlite/README/config-file.md`

新增：

```rust
pub terminal_max_active_sessions: u32
```

保留 `0` 表示不限制。配置测试中的 `Config` 字面量需要补齐该字段。

### 7.2 Session Manager

修改 `worker/worker-boxlite/src/runner/terminal_session_manager.rs`：

- `TerminalSessionManagerConfig` 增加 `max_active_sessions`。
- manager 增加 reservation 计数。
- session 创建准入在持有 `sessions` mutex 时完成。
- session 从 map 移除后，必须保留一个 retired session/cleanup 记录穿过 `remove_box(...).await`。
- backend remove 返回后再释放 reservation。
- `active_session_count()` 返回 reservation 数。

推荐引入内部结构：

```rust
struct RetiredSession {
    box_id: String,
    reservation_held: bool,
}
```

janitor、`release_session`、`release_and_destroy_session` 和 `close` 均返回或收集 `RetiredSession`，在 mutex 外执行异步 remove。

禁止持有 Tokio mutex guard 跨越 `.await`。

### 7.3 BoxLite 特有风险

- 当前 session 从 map 删除后通常只保留 `box_id`；若 reservation 随 session drop，会在 microVM 删除前提前释放。
- `boxlite_runtime` 还维护 `box_id -> Arc<LiteBox>` 缓存；容量释放应发生在缓存清理和 runtime remove 尝试之后。
- real runtime 验证需要支持 BoxLite 的主机环境，普通 mock 测试不能替代 microVM smoke。
- shutdown 和 janitor 的 async remove 不应阻塞 session map 的其他操作。

`boxlite_runtime.rs` 的 Python 一次性 Box 创建流程不修改。

## 8. worker-bridge-e2b 实施计划

### 8.1 配置

修改：

- `worker/worker-bridge-e2b/internal/config/config.go`
- `worker/worker-bridge-e2b/internal/config/source_test.go`
- `worker/worker-bridge-e2b/config.example.toml`
- `worker/worker-bridge-e2b/README/overview.md`
- `worker/worker-bridge-e2b/README/config-file.md`

新增：

```go
TerminalMaxActiveSessions int
```

该工程已经提供 `nonNegativeInt`，可直接解析 `0=不限制`。

### 8.2 Session Manager

修改 `worker/worker-bridge-e2b/internal/runner/terminal_exec.go`：

- `terminalSessionManagerConfig` 增加 `MaxActiveSessions`。
- manager 增加最大值与 reservation 计数。
- E2B Control API `Create` 前完成容量预留。
- E2B `Kill` 的有界清理调用返回后释放 reservation。
- 创建失败且没有获得 sandbox 时释放 reservation。
- `ActiveSessionCount` 返回 reservation 计数。

E2B 对本地已过期 session 有同步替换逻辑：当 `create_if_missing=true` 时，旧 sandbox 被清理后会用相同 session ID 创建新 sandbox。该流程应将旧 session 的 reservation 转移给新 session，不应先释放后重新竞争。这样在 worker 已满时，过期 session 仍能被正确替换，同时不会额外占用一个 slot。

### 8.3 E2B 特有风险

- `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS` 只限制单个 worker 进程管理的 session，不限制同一 E2B API Key 下所有 worker 的 sandbox 总数。
- worker 进程崩溃后，本地 session map 和 reservation 丢失，远端 sandbox 会继续存活到 E2B timeout。
- gRPC 断线重连不会丢失 manager，因为 manager 位于重连循环外；只有进程退出或崩溃会丢失本地状态。
- E2B `Kill` 失败时，在完成清理尝试后释放本地 reservation；远端 sandbox 依赖已同步的 timeout 最终回收。
- session 数量上限同时也是 E2B 成本与账号并发配额的保护项，生产值应结合 E2B 套餐和 worker 实例数量设置。

`python_runtime.go`、E2B client、envd protobuf 和生成代码均不修改。

## 9. worker-sys

`worker-sys` 不参与本次改造：

- `computerUse` 和 `readImage` 是无状态能力。
- 固定的 `session_id="computerUse"` 不是持久 sandbox session。
- worker 直接执行宿主机进程，没有可按 terminal session 计数的容器或 VM。
- 继续使用 `WORKER_COMPUTER_USE_MAX_INFLIGHT` 和 `WORKER_READ_IMAGE_MAX_INFLIGHT` 控制并发。

## 10. Console 最小配套

### 10.1 错误映射

修改 console，使 `session_capacity_exceeded`：

- REST terminal API 返回 HTTP `429`。
- Task 和 MCP 保留 worker 错误码与消息。
- 不被转换为通用 `502 execution_failed`。

涉及位置预计包括：

- `console/internal/httpapi/command_handler.go`
- `console/internal/httpapi/mcp_errors.go` 或现有 MCP task failure 映射位置
- 对应 REST、Task 和 MCP 测试

### 10.2 路由回滚

console 会在 worker 实际创建 session 前预留 `session_id -> node_id` 路由。若本次请求新建了路由，而 worker 返回 `session_capacity_exceeded`，必须清除该预留路由。

修改 `console/internal/grpcserver/service_dispatch.go` 与内存路由记录：

- 增加 `isSessionCapacityCommandError` 判断。
- 每次新建 provisional route 时分配单调递增的 reservation ID。
- worker 成功返回后确认 route，并清除其 provisional reservation ID。
- 容量错误只回滚 `session_id`、node ID 和 reservation ID 均匹配的 provisional route，防止并发成功请求或同节点 ABA 重建的 route 被较晚错误误删。
- 已存在 session 的稳定路由不因一般执行错误被清除。

第一阶段不自动重试其他 worker。下一次请求可以重新选择 worker。自动重试需要保证错误发生在 sandbox 创建和用户命令执行之前，并维护本次调度已尝试 node 集合，留待容量感知调度阶段实现。

## 11. 第一阶段调度行为

第一阶段不修改 protobuf。console 只知道 heartbeat 上报的 `active_session_count`，不知道每个 worker 配置的最大 session 数，因此仍按 capability inflight 选择 worker。

允许出现以下保守失败：

```text
worker-A terminal session 已满
worker-B 仍有 session 空位
console 按 terminalExec inflight 选择 worker-A
请求返回 session_capacity_exceeded / 429
```

该行为不影响容量安全，只影响集群利用率。

后续可增加独立阶段，完整评估与实施方案见 [terminal-session-capacity-aware-dispatch-plan.md](./terminal-session-capacity-aware-dispatch-plan.md)：

1. hello 通过带 presence 的 `terminal_session_capacity` 上报最大值与初始 active 数。
2. heartbeat 继续上报当前 reservation 数。
3. console 对新 session 优先选择有剩余容量的 worker。
4. worker 本地准入继续作为最终一致性保障。
5. 容量竞争失败时安全重试尚未尝试的 worker。

该后续阶段不属于本计划的验收范围。

## 12. 资源规划

### 12.1 Docker 与 BoxLite

可使用以下近似上界规划内存：

```text
terminal_max_active_sessions * terminal_memory_limit
+ python_exec_max_inflight * python_memory_limit
+ worker / Docker / BoxLite runtime 预留
<= 主机可用内存
```

CPU 可使用相同方式估算。多个命令在同一 terminal session 中共享该 session 的 CPU/Memory 配额，但会增加进程数、文件 IO 和调度竞争。

Docker memory 是 cgroup 上限而非预留量，实际部署可以允许受控超配；BoxLite microVM 的常驻内存特征应以实测为准。

### 12.2 E2B

E2B sandbox 资源由 template 和 E2B 平台决定。每个 worker 的配置值应满足：

```text
worker 实例数 * terminal_max_active_sessions
+ worker 实例数 * python_exec_max_inflight
<= E2B 账号允许的并发 sandbox 预算
```

该公式是静态规划值，不是系统级强一致配额。

### 12.3 多账号公平性

session 上限是 worker 全局上限，不是账号配额。共享 normal worker 上，一个账号可以通过创建长 lease session 占满容量并影响其他账号。

本次不增加 per-account session quota。生产环境可先通过较短默认 lease、合理最大 lease 和 worker 扩容降低影响。账号级 quota 应在 console 拥有可靠 session 状态后单独设计。

## 13. 测试计划

### 13.1 三个 sandbox worker 的共同用例

每个 worker 必须覆盖：

1. `max=2` 时前两个不同 session 创建成功，第三个返回 `session_capacity_exceeded`。
2. 容量错误发生在 backend create 之前。
3. 容量已满时，已有 session 的 `terminalExec` 成功。
4. 容量已满时，已有 session 的 `terminalResource` 成功。
5. 未知 session 且 `create_if_missing=false` 返回 `session_not_found`，不返回容量错误。
6. 同一个 session ID 的并发创建只占一个 reservation。
7. backend create 失败后容量恢复。
8. 创建中的 session 计入容量。
9. session lease 到期并完成清理尝试后容量恢复。
10. destroying session 在 inflight 排空前持续占容量。
11. backend cleanup 阻塞期间持续占容量。
12. terminalExec 超时触发延迟销毁，最终释放容量。
13. terminalResource 超时或 sandbox 丢失后最终释放容量。
14. shutdown 清理所有 session，reservation 最终为零。
15. `max=0` 时保持当前无限制行为。
16. heartbeat `active_session_count` 与 reservation 数一致。
17. race detector 或并发测试不出现计数负数、重复释放或超限创建。

### 13.2 E2B 额外用例

- 已过期 session 原地替换时转移 reservation。
- E2B `Kill` 阻塞期间不提前释放容量。
- E2B create 失败且 sandbox 为 nil 时释放容量。
- manager `Close` 等待正在进行的 create，并在其完成后清理和释放。

### 13.3 BoxLite 额外用例

- retired session 在 `remove_box().await` 完成前持有 reservation。
- 删除 Box 时不持有 session map mutex。
- Box runtime/cache 清理完成后才释放 reservation。
- shutdown 取消正在进行的 Box 创建；backend 不响应取消时有界返回，并清理迟到创建的 Box。

### 13.4 Console 用例

- REST terminal 将 `session_capacity_exceeded` 映射为 `429`。
- Task 查询保留 error code/message。
- MCP 返回可识别的 tool error。
- 新建 route 遇到容量错误后被清除。
- 已有 route 的一般执行错误不误清理。

## 14. 验证命令

### 14.1 console

```bash
go -C console test ./...
```

### 14.2 worker-docker

```bash
go -C worker/worker-docker test -race ./...
```

按现有 worker 文档执行 Docker backend 的集成验证。

### 14.3 worker-boxlite

```bash
cargo test --manifest-path worker/worker-boxlite/Cargo.toml
```

在支持 BoxLite 的真实主机上执行 `boxlite_smoke`，验证 Box 创建、命令执行、并发、删除和 shutdown。

### 14.4 worker-bridge-e2b

```bash
go -C worker/worker-bridge-e2b test -race ./...
```

具备 E2B 凭据时执行真实 client lifecycle smoke：

```bash
E2B_INTEGRATION=1 \
E2B_API_KEY=... \
E2B_TERMINAL_TEMPLATE=... \
go -C worker/worker-bridge-e2b test ./internal/e2b \
  -run TestIntegrationSandboxCommandAndFile -count=1 -v
```

并执行真实 manager 容量 lifecycle，验证容量错误发生在第二次 E2B `Create` 前、已有 session 在满载时可用、`Kill` 后 slot 可复用且 `Close` 归零：

```bash
E2B_INTEGRATION=1 \
E2B_API_KEY=... \
E2B_TERMINAL_TEMPLATE=... \
go -C worker/worker-bridge-e2b test ./internal/runner \
  -run TestIntegrationTerminalSessionCapacityLifecycle -count=1 -v
```

## 15. 实施顺序与提交边界

按照子工程边界拆分提交，避免在一个提交中混合多个 worker：

1. `console`：增加错误映射和新建 route 回滚。
2. `worker-docker`：增加配置、容量 reservation、测试与本工程 README。
3. `worker-boxlite`：增加配置、跨 async cleanup 的 reservation、测试与本工程 README。
4. `worker-bridge-e2b`：增加配置、远端 cleanup reservation、测试与本工程 README。
5. 根文档：同步 `README/API.md`、`README/API.zh-CN.md` 和跨工程说明。

每个 worker 提交都应独立构建、独立测试，并在默认 `0` 配置下保持现有行为。

## 16. 发布与回滚

### 16.1 发布步骤

1. 先发布支持新错误码的 console。
2. 再逐个发布三个 worker。
3. worker 初次升级保持 `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS=0`。
4. 根据资源监控为单个 worker 设置正数，并观察一轮最大 lease 周期。
5. 关注 `session_capacity_exceeded`、backend cleanup failure、active session count 和任务 `429` 比例。
6. 分批降低或提高限制，不同时大幅调整 lease 与 session 数量上限。

### 16.2 回滚

将配置设为 `0` 即可关闭 session 数量限制，无需回退二进制：

```text
WORKER_TERMINAL_MAX_ACTIVE_SESSIONS=0
```

关闭限制不会销毁现有 session，也不会改变 capability inflight。

## 17. 风险与后续事项

| 风险 | 当前处理 | 后续方向 |
| --- | --- | --- |
| cleanup 失败后底层 sandbox 残留 | 有界尝试后释放 reservation 并记录告警 | worker 启动/周期 reconciliation |
| console 选择到已满 worker | 返回 `429` 并清理新建 route | 上报 max、容量感知调度和安全重试 |
| 单账号占满共享 worker | 本次不处理 | console per-account session quota |
| console 重启后丢失 session route | 本次不处理 | worker session snapshot 与 route 重建 |
| E2B 多 worker 共用 API Key 超额 | 运维静态规划 | API Key 级全局配额或 provider reconciliation |
| worker 崩溃后 E2B sandbox 暂存 | 依赖 E2B timeout | 重启后查询并接管/清理远端 sandbox |
| heartbeat 瞬时滞后 | worker 本地准入为准 | 带版本的容量快照 |

## 18. 验收标准

- [x] 三个 sandbox worker 均支持 `WORKER_TERMINAL_MAX_ACTIVE_SESSIONS`。
- [x] 默认 `0` 时行为与改造前一致。
- [x] 正数上限无法被并发创建竞态突破。
- [x] 已有 session 在容量满时仍可执行 exec/resource。
- [x] 所有创建失败、超时、lease 回收和 shutdown 路径最终释放 reservation。
- [x] destroying 与 backend cleanup in progress 状态计入 active session 数。
- [x] `session_capacity_exceeded` 在 REST 中返回 `429`，并在 Task/MCP 中保留错误语义。
- [x] console 不会保留创建失败 session 的临时 route。
- [x] 原有 capability `max_inflight` 与单 session inflight 行为和默认值不变。
- [x] `worker-sys` 行为不变。
- [x] console、worker-docker、worker-boxlite 和 worker-bridge-e2b 的单元测试通过。
- [x] Docker、BoxLite 已完成对应 backend 的真实生命周期验证。
- [x] E2B 已完成真实 backend 生命周期验证：client create/command/file/kill、`max=1` manager capacity lifecycle、完整 gRPC capability dispatch、并发 exec/resource、timeout 延迟销毁与 janitor cleanup 均通过。
