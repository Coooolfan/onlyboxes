# Console 重启后的终端会话恢复

本文描述 Console 与 sandbox Worker 对 terminal session 的现行恢复语义。恢复覆盖 `worker-docker`、`worker-boxlite` 和 `worker-bridge-e2b`，并保持原 `session_id`、Worker 归属、后端文件系统和绝对 lease。

## 状态边界

Console 持久化以下稳定事实：

| 字段 | 含义 |
| --- | --- |
| `scoped_session_id` | 带账号作用域的 terminal session ID |
| `node_id` | session 固定归属的 Worker node ID |
| `lease_expires_unix_ms` | Worker 已确认的绝对 lease 到期时间 |
| `last_used_unix_ms` | 最近一次成功确认或续租时间 |
| `created_at_unix_ms` | route 首次持久化时间 |
| `updated_at_unix_ms` | route 最近一次持久化更新时间 |

以下瞬态状态只属于当前 Console 进程：

- `ready`、`unavailable`、`reconciling` 恢复状态；
- provisional route、reservation ID 和 provisional use 计数；
- Worker connection session、capability inflight 和恢复报告幂等缓存；
- terminal session 容量快照。

SQLite 是 confirmed route 与绝对 lease 的持久化来源；Worker 后端是 sandbox、文件系统和进程状态的来源。Console 不持久化容器 ID、Box ID、E2B sandbox ID、envd token 或其他后端凭据。

Public preview route 也持久化到 SQLite。Console 启动时删除已过期的 preview route 并恢复有效 route 的原 URL；在对应 terminal session 和 Worker 完成恢复前，公开入口暂时不可用。

## Confirmed route 写入

首次 `terminalExec` 使用 provisional route 防止并发请求把同一 session 分配到不同 Worker。Worker 成功返回后，Console 按以下顺序确认 route：

1. 从结果读取 `lease_expires_unix_ms`。
2. 在 SQLite 中写入 scoped session ID、Worker node ID 和绝对 lease。
3. 数据库提交成功后，把内存 route 标记为 confirmed。
4. 向调用方返回成功结果。

复用已有 session 时，Console 只接受不缩短 lease 的更新。数据库写入失败会使请求失败，不会产生仅存在于内存的 confirmed route。

Provisional route 不写入数据库。Console 重启会中断对应命令，非终态 task 由启动恢复标记为 `console_restarted`。

## Console 启动

Console 在启动监听前执行 terminal route 恢复：

1. 执行数据库 migration。
2. 删除 `lease_expires_unix_ms <= now` 的记录。
3. 加载所有未过期 confirmed route。
4. 重建 session-to-node 与 node-to-session 内存索引。
5. 将所有加载 route 标记为 `unavailable`。

加载或清理失败会终止 Console 启动，避免以不完整路由状态接收请求。恢复出的 route 在 Worker 完成资源核对前不可调度，也不会被分配给其他 Worker。

## Worker 重连握手

拥有 `terminalExec` capability 的 Worker 每次连接都必须完成恢复握手：

1. Console 根据 `node_id` 发送该 Worker 的全部未过期 recovery candidates。
2. Candidate 携带 scoped session ID 与原绝对 lease。
3. Worker 核对其后端资源，逐项返回 `RECOVERED`、`MISSING` 或 `INVALID`。
4. Console 要求报告完整覆盖本次 candidates，拒绝未知、重复或缺失结果。
5. `RECOVERED` route 变为 `ready`；其他结果及已过期 route 从 SQLite 和内存中删除。
6. Console 完成持久化变更后发送 recovery ack，Worker 收到 ack 后才可接收命令。

同一连接重复提交完全一致的报告时，Console 可重复返回 ack；报告内容变化会被拒绝。

## 请求语义

- Worker 离线或 route 正在核对时，已有 session 返回可重试的 `session_unavailable`。
- 已有 confirmed route 始终固定到原 Worker，不因容量或离线状态改派。
- `MISSING`、`INVALID` 或 lease 到期会删除 route；后续请求遵循正常的 `session_not_found` 或显式创建语义。
- Console 重启时正在执行的命令、输出流和非终态 task 不恢复。
- Public preview route 在启动时独立恢复；对应 terminal route 与 Worker 恢复完成后，原公开 URL 重新可用。

## 删除与清理

以下事件同时删除 SQLite 与内存 route：

- Worker 明确返回 `session_not_found`；
- recovery 返回 `MISSING` 或 `INVALID`；
- lease 到期或定期清理发现 route 过期；
- 管理员删除已 provision 的 Worker。

持久化删除失败时，Console 不确认对应状态变更。Worker 侧无法被重新接管的资源由各后端的受限 orphan cleanup 或 provider timeout 回收。

## 实现入口

- [Console route 状态机](../../console/internal/grpcserver/terminal_session_routes.go)
- [Console Worker 连接握手](../../console/internal/grpcserver/service_connect.go)
- [SQLite route store](../../console/internal/registry/store_terminal_session_routes.go)
- [数据库 schema](../../console/db/migrations/00006_terminal_session_routes.sql)
- [gRPC 恢复协议](../../api/proto/registry/v1/registry.proto)
- [Console 重启恢复测试](../../console/internal/grpcserver/terminal_session_routes_test.go)
