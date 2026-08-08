# Console 重启后的终端会话恢复实施计划

## 1. 目标

Console 正常重启或进程崩溃后，仍能恢复尚未过期的 terminal session route，使 Worker 重连时继续通过现有恢复握手核对后端资源，调用方随后可以使用原有 `session_id` 和沙箱文件系统。

本阶段建立以下完整链路：

```text
terminalExec 成功
  -> Console 持久化 scoped session route 与绝对 lease
  -> Console 重启并加载有效 route
  -> route 以 unavailable 状态进入内存
  -> 原 Worker 重连并收到 recovery candidates
  -> Worker 核对后端资源并上报结果
  -> Console 确认 route 或持久化删除失效 route
  -> session 恢复可用
```

范围：

- 持久化已确认 terminal session 的 Worker 归属和绝对 lease。
- 支持 Console 正常退出、进程崩溃和主机重启后的恢复。
- 复用现有 Worker recovery candidate、report 和 ack 协议。
- 同等支持 `worker-docker`、`worker-boxlite` 和 `worker-bridge-e2b`。
- 保持 owner-scoped session ID 的隔离语义。
- 恢复后继续支持 terminalExec、terminalResource 和新建 public preview route。

不包含：

- 不恢复 Console 重启时正在执行的命令、输出流或非终态 task；这些 task 继续以 `console_restarted` 失败。
- 不持久化 provisional route、reservation ID 或 provisional uses。
- 不恢复已存在的 public preview URL。Preview route 仍是独立的内存状态；Console 重启后可基于恢复的 terminal session 重新创建。
- 不改变 Worker 恢复协议、后端资源标记或 Worker session manager。
- 不支持多 Console 实例共享同一 SQLite 数据库或同时调度同一 Worker。

## 2. 当前基础

当前分支已经实现 Worker 重启恢复：

- `terminalSessionRoute` 在内存中保存 `NodeID`、`LeaseExpiresUnixMs` 和恢复状态。
- Worker 断连时，已确认 route 从 `ready` 进入 `unavailable`，不会立即删除。
- Worker 重连时，Console 发送 recovery candidates；Worker 返回 `RECOVERED`、`MISSING` 或 `INVALID`。
- Worker 完成核对前不可调度；恢复失败或 lease 过期的 route 会被删除。
- terminal session 使用 owner-scoped session ID，Worker 不接触外部账号身份。

当前缺口是 `terminalSessionToNode` 和 `terminalNodeToSessionIDIndex` 仅存在于进程内存。Console 重启后，新进程无法生成任何 recovery candidate，即使 Worker 后端资源和 lease 仍然有效。

Console 已使用 SQLite、Goose 和 sqlc，并在启动时完成 migration、将非终态 task 标记为失败、清空旧 Worker connection session。Terminal route 持久化应接入同一数据库和启动顺序。

## 3. 状态归属

### 3.1 持久化稳定事实

数据库只保存跨 Console 进程仍成立的事实：

| 字段 | 含义 |
| --- | --- |
| `scoped_session_id` | owner-scoped terminal session ID，唯一主键 |
| `node_id` | session 固定归属的 Worker node ID |
| `lease_expires_unix_ms` | Worker 已确认的绝对 lease 到期时间 |
| `last_used_unix_ms` | 最近一次确认或 lease 更新的时间，用于审计和诊断 |
| `created_at_unix_ms` | route 首次持久化时间 |
| `updated_at_unix_ms` | route 最近一次持久化更新时间 |

### 3.2 仅保留在内存的瞬态状态

以下状态不得持久化：

- `RecoveryState`：`ready`、`unavailable` 和 `reconciling` 只描述当前 Console 进程与 Worker connection 的关系。每次 Console 启动时，所有加载的 route 都必须从 `unavailable` 开始。
- `ReservationID` 与 `ProvisionalUses`：它们只保护当前进程中的首次并发 dispatch。Console 重启会中断对应 task，不能把不确定的创建结果恢复为已确认 route。
- active Worker connection session ID、inflight capability 数和 recovery report 的临时幂等缓存。

### 3.3 权威边界

- SQLite 是已确认 route 和 lease 的持久化权威来源。
- `terminalSessionToNode` 是运行时索引，不得包含数据库尚未确认的 durable route。
- Worker 后端是沙箱及文件系统的权威来源。
- Worker recovery report 决定持久化 route 对应的后端资源是否仍然存在且有效。
- Console 不持久化 Docker container ID、Boxlite Box ID、E2B sandbox ID、envd token 或其他后端凭据。

## 4. 数据库设计

新增 migration：

```sql
CREATE TABLE terminal_session_routes (
    scoped_session_id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    lease_expires_unix_ms INTEGER NOT NULL CHECK (lease_expires_unix_ms > 0),
    last_used_unix_ms INTEGER NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX idx_terminal_session_routes_node
    ON terminal_session_routes(node_id);

CREATE INDEX idx_terminal_session_routes_lease
    ON terminal_session_routes(lease_expires_unix_ms);
```

`node_id` 不声明指向 `worker_nodes` 的外键，原因如下：

- runtime Worker 记录可能被 offline pruner 删除，但有效 terminal lease 必须继续等待同一 node ID 重连。
- Console 启动时会清空 `worker_nodes.session_id`，这不应影响 terminal route。
- 删除 Worker 凭据与删除 terminal route 是不同的业务动作，需要在 Worker 删除流程中显式处理，而不是依赖 registry 表的级联副作用。

新增 sqlc queries：

- `UpsertTerminalSessionRoute`
- `DeleteTerminalSessionRouteBySessionAndNode`
- `DeleteTerminalSessionRoutesByNode`
- `DeleteExpiredTerminalSessionRoutes`
- `ListActiveTerminalSessionRoutes`
- 测试和诊断需要的精确查询或计数查询

`UpsertTerminalSessionRoute` 必须满足：

- 同一 `scoped_session_id` 只能绑定一个 `node_id`。
- 已确认的同 node lease 可以更新，但不得因迟到结果缩短现有 lease。
- 不允许迟到的旧 Worker 结果覆盖已经属于其他 node 的 route。
- `created_at_unix_ms` 在更新时保持不变。

## 5. 持久化接口

在 gRPC registry 与 SQLite 之间增加窄接口，避免 route 状态机直接依赖 sqlc 类型：

```text
LoadActive(ctx, now_unix_ms) -> []PersistedTerminalSessionRoute
UpsertConfirmed(ctx, route) -> persisted | node_conflict
Delete(ctx, scoped_session_id, expected_node_id) -> deleted | not_owned
DeleteByNode(ctx, node_id)
DeleteExpired(ctx, now_unix_ms) -> deleted_count
```

实现要求：

- 生产实现使用 `persistence.DB` 和 sqlc。
- 单元测试使用内存 fake，能够注入写入、删除和加载失败。
- RegistryService 初始化时显式注入该接口，不允许生产环境静默退化为纯内存 route。
- 只有不需要重启恢复的局部测试可以使用 no-op store。

## 6. 写入一致性

### 6.1 已确认 route 的唯一提交点

合并当前分离的 route 确认与 lease 更新路径，形成单一 durable commit 操作：

```text
commitConfirmedTerminalRoute(
  scoped_session_id,
  expected_node_id,
  reservation_id,
  lease_expires_unix_ms,
  now
)
```

执行顺序：

1. 在 route 锁下验证 reservation、node 归属和 ABA 条件。
2. 要求 Worker 成功结果包含正数 `lease_expires_unix_ms`。
3. 使用独立、有限时长的数据库 context 持久化 route，不复用可能刚好到期的请求 context。
4. 数据库成功后，才把内存 route 确认为 durable、清除 reservation 并更新 lease。
5. route durable commit 成功后，才允许成功结果返回调用方并进入 task 成功状态。

数据库写入失败时：

- 不把 provisional route 暴露为已确认 route。
- 当前命令返回内部错误；Worker 已创建但未确认的资源由远端 lease 或后续明确清理回收。
- 记录不包含原始命令、文件内容或凭据的结构化错误。

### 6.2 现有 session 的 lease 延长

复用现有 session 的命令成功后，Worker 可能已经延长后端 lease。Console 必须先持久化不缩短的绝对 lease，再更新内存并返回成功。

若数据库失败：

- 不得只更新内存 lease。
- route 保持原 durable lease；请求返回内部错误。
- 后端 lease 可能更长，但不会比 Console 记录更早删除资源，最多形成由后端 timeout 最终回收的孤儿，不会错误恢复过期 session。

### 6.3 删除

以下事件必须同步删除持久化 route：

- Worker 返回 `session_not_found`。
- recovery report 返回 `MISSING` 或 `INVALID`。
- recovery 时发现 lease 已过期。
- route janitor 清理到期 route。
- 管理端明确删除对应 Worker。

删除使用 `scoped_session_id + expected_node_id` 条件，防止迟到事件删除 ABA 后的新归属。

Recovery report 的删除必须在发送 recovery ack 前完成。数据库失败时关闭本次 Worker connection，不发送 ack；Worker 重连后重新核对，不能以部分持久化状态开始接单。

### 6.4 不持久化 provisional route

首次 dispatch 前的 route reservation 保持纯内存：

- Console 重启时对应 task 已被标记为 `console_restarted`。
- 未收到成功结果前，Console 无法确认 Worker 是否创建了资源。
- 把 provisional route 写入数据库会把不确定状态错误升级为可恢复 session。

只有收到合法 terminalExec 成功结果并取得绝对 lease 后，route 才进入 `terminal_session_routes`。

## 7. Console 启动恢复

启动顺序调整为：

1. 打开 SQLite 并执行 Goose migrations。
2. 在启动事务中保持现有 task 和 Worker connection 清理：
   - 非终态 task 标记为 `console_restarted`。
   - `worker_nodes.session_id` 清空。
3. 删除 `lease_expires_unix_ms <= now` 的持久化 terminal route。
4. 读取剩余 route，并校验 session ID、node ID 和绝对 lease。
5. 创建 RegistryService，把持久化 route 装载到两个内存索引：
   - `terminalSessionToNode`
   - `terminalNodeToSessionIDIndex`
6. 所有装载 route 的内存状态固定为：
   - `RecoveryState=unavailable`
   - `ReservationID=0`
   - `ProvisionalUses=0`
7. 完成装载后才启动 gRPC 和 HTTP listener。
8. Worker 重连后，复用现有 `beginTerminalSessionRecovery` 和 `applyTerminalSessionRecoveryReport` 完成资源核对。

启动采用 fail-closed：

- migration、过期清理或 route 加载失败时，Console 启动失败。
- 不允许记录警告后以空 route 集合启动，否则会静默丢失恢复能力并允许同名 session 被重新分配。
- 单条不合法记录视为数据库完整性错误，不跳过后继续启动。

## 8. 并发与崩溃语义

### 8.1 锁和数据库顺序

Route mutation 必须遵循统一顺序：

```text
terminalRoutesMu -> SQLite write -> in-memory mutation -> unlock
```

Console 当前是单实例、SQLite 单 writer 模型。持锁执行短 SQL 写入可保持现有 ABA 与 reservation 判断在数据库提交期间不失效，也避免引入第二套 revision 状态机。

约束：

- route SQL 不得调用网络服务或 Worker。
- 数据库操作必须有固定超时。
- 不得在持有 route 锁时等待 recovery report、command result 或 proxy 请求。
- 批量 recovery report 使用单个数据库事务。

### 8.2 崩溃点

| 崩溃点 | 重启后的结果 |
| --- | --- |
| Worker 成功前 | provisional route 未持久化，不恢复 |
| Worker 成功后、数据库提交前 | route 不恢复；后端资源按 lease 最终回收 |
| 数据库提交后、返回调用方前 | route 会恢复；调用方可用原 request/task 查询结果或重试原 session |
| route 删除提交前 | 重启后再次向 Worker核对，结果最终收敛 |
| route 删除提交后、内存删除前 | 重启后 route 不再出现 |
| recovery report 事务中途 | 事务回滚且不发送 ack，Worker 重连重试 |

系统目标是 session route 的 at-least-once reconciliation，不提供中断命令的 exactly-once 执行。

## 9. Worker 生命周期与清理

### 9.1 Worker 重连

Console 重启后，持久化 route 不依赖旧 Worker connection session。相同 `node_id` 的 Worker 建立新连接时会收到所有未过期 candidate，恢复协议不需要新增字段。

### 9.2 Worker 删除

管理端删除 Worker 时：

1. 关闭当前 connection。
2. 删除该 node 的持久化 terminal route。
3. 删除内存 route 和 node 索引。
4. 删除 Worker registry、credential 和 owner claim。

删除 Worker 后不再允许其使用旧凭据重连。后端遗留资源由 Docker/Boxlite 的受限 orphan 清理或 E2B timeout 回收。

Offline pruner 删除非 provisioned Worker registry 行时，不删除 terminal route。有效 route 保持到 lease 到期，以允许同一 node ID 在凭据仍有效时重连。

## 10. Owner 隔离与安全

- 数据库主键保存完整 owner-scoped session ID，不另存一份外部 session ID。
- 所有 API 输入仍先通过 `scopeTerminalSessionID`，输出仍通过 `unscopeTerminalSessionID`。
- route 查询、更新和删除不得接受未 scoped 的外部 session ID。
- 日志不记录 scoped session ID，因为其中包含 owner 标识；使用 session hash、node ID、数量和状态。
- 不新增后端凭据、Worker secret 或 proxy traffic token 的持久化。
- 数据库文件沿用 Console 现有访问控制和备份策略。

## 11. Public preview 边界

Terminal route 恢复后：

- 用户可以对原 session 创建新的 public preview route。
- 已有 preview route key 仍不会跨 Console 重启恢复。
- Preview 解析必须继续要求 terminal route 已完成 Worker reconciliation，不能仅因数据库存在 route 就转发流量。

持久化 preview URL 涉及匿名访问凭据生命周期、撤销、账号删除和独立过期索引，应在单独 PR 中设计，不能与 terminal route 恢复隐式绑定。

## 12. 实施阶段

### 阶段 A：Schema 与持久化层

- 新增 `terminal_session_routes` migration。
- 新增 sqlc queries 并重新生成代码。
- 实现 persistence adapter 和 fake。
- 覆盖 upsert 不缩短 lease、node 冲突、条件删除、批量过期删除和事务回滚。

### 阶段 B：RegistryService durable mutation

- 注入 terminal route store。
- 合并 confirm 与 lease update 为 durable commit。
- 把 session-not-found、recovery failure、janitor 和 Worker 删除接入持久化删除。
- 为数据库失败定义统一内部错误和结构化日志。

### 阶段 C：启动恢复

- 在 listener 启动前加载有效 route。
- 重建两个内存索引并统一标记为 unavailable。
- 保持 task startup recovery 和 Worker session 清理行为。
- 增加启动失败和非法数据测试。

### 阶段 D：重启故障矩阵

- 使用临时 SQLite 文件创建第一个 Console service，确认 route 后关闭。
- 使用同一数据库创建第二个 service，模拟 Worker 重连和 recovery report。
- Docker、Boxlite 和 E2B 分别执行真实后端重启场景。
- 覆盖数据库写失败、事务回滚和各个崩溃边界。

## 13. 测试矩阵

### 13.1 Persistence

- migration 可从当前 schema 正常升级和回滚。
- confirmed route 写入后可重新加载。
- upsert 不缩短 lease。
- 不同 node 不能覆盖已有 route。
- 条件删除不删除其他 node 的 route。
- 过期清理只删除到期 route。
- malformed route 使启动失败。

### 13.2 Registry 状态机

- provisional route 永不持久化。
- terminalExec 成功必须在 durable commit 后返回。
- 复用 session 的 lease 延长同步持久化。
- 持久化失败不会产生仅内存 confirmed route。
- `session_not_found` 同时删除数据库和内存 route。
- recovery `MISSING`、`INVALID` 和过期结果同步删除。
- recovery 批量写失败不发送 ack，且不应用部分内存结果。
- stale reservation、迟到 command result 和迟到 recovery report 不能覆盖或删除新 route。

### 13.3 Console 重启

- 正常重启后有效 route 被加载为 unavailable。
- 崩溃重启后行为与正常重启一致。
- Worker 重连收到准确、稳定排序的 candidate 和原绝对 lease。
- Worker 报告 `RECOVERED` 后原 session 可继续执行且 `created=false`。
- Console 离线期间到期的 lease 不进入 candidate。
- Worker 尚未重连时，terminalExec、terminalResource 和 preview 解析返回 unavailable，不发生重新分配。
- 重启时的 provisional route 不恢复。
- 非终态 task 仍以 `console_restarted` 失败。

### 13.4 后端验收

Docker、Boxlite 和 E2B 分别验证：

1. 创建 session 并写入文件。
2. 停止 Console，但保持 Worker 后端资源。
3. 重启 Console。
4. Worker 自动重连并完成 recovery handshake。
5. 使用原 external `session_id` 读取文件。
6. 确认 lease 未重置、session 未被重新创建、容量计数正确。

## 14. 验收标准

- Console 重启后，所有未过期 confirmed route 都能进入 Worker recovery candidate。
- Worker 恢复成功后，原 session ID、文件系统、Worker 归属和绝对 lease 保持不变。
- Console 离线期间到期的 route 不恢复，后端资源最终清理。
- Worker 未重连或正在核对时返回 `session_unavailable`，不改派到其他 Worker。
- 数据库故障不会产生仅内存成功、部分 recovery ack 或静默空状态启动。
- Provisional route 和中断命令不会被误恢复。
- Owner 隔离在持久化、恢复、命令和资源访问路径中保持不变。
- 三个 Worker 后端通过同一 Console 重启故障矩阵，无需修改现有 recovery proto。
- 现有 Worker 重启恢复、capacity routing、terminalResource 和 public preview 测试继续通过。

## 15. 发布说明

首次部署本功能时，旧 Console 进程中的 route 尚未写入新表，因此部署前已经存在的 terminal session 无法跨这次升级重启恢复。功能生效后新建或成功续租的 confirmed route 才具备 Console 重启恢复能力。

发布时应明确这一单次边界；不通过猜测 Worker 资源或账号级扫描来补建旧 route。
