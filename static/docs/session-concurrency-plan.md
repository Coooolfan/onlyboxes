# Session 并发化改造计划

> 本文档为实施规范，面向接手该改造的开发者或 agent，不依赖任何外部会话上下文。
> 分支：`feat/worker-session-concurrency`

## 0. 阅读指引

实施前建议按序阅读：

1. 本文第 1、2 节（背景与概念）
2. `AGENTS.md`（仓库约定）
3. 对应工程的 `README/overview.md`
4. `README/API.zh-CN.md` 第 6 章（命令执行 API）

文中代码位置以「文件路径 + 符号名」定位，行号仅供参考，可能随改动漂移。

## 1. 项目背景

Onlyboxes 是代码执行沙箱平台，monorepo，前后端分离，核心服务以「控制节点 — 执行节点」形式部署。

```
用户 ──HTTP/MCP──> console ──gRPC 双向流──> worker ──> 容器 / microVM / 宿主机进程
```

- **控制节点** `console/`：Go + Gin。对外提供 REST / MCP，对内通过 gRPC 双向流（`api/proto/registry/v1/registry.proto` 的 `Connect`）向 worker 下发命令。
- **执行节点** `worker/`，三种实现：
  - `worker-docker/`：Go，Docker 容器为后端
  - `worker-boxlite/`：Rust，boxlite microVM 为后端
  - `worker-sys/`：Go，宿主机进程为后端，用于直接控制真实设备
- **前端** `web/`：Vue + TypeScript，本次不涉及。

## 2. 关键概念

**capability（能力）**：worker 在 gRPC 握手（hello）时声明自己支持的能力。console 按 capability 路由命令。相关常量见 `console/internal/grpcserver/service.go` 与 `task_owner_scope.go`：

| capability | 含义 | 提供方 |
| --- | --- | --- |
| `echo` | 连通性测试 | docker / boxlite |
| `pythonExec` | 一次性 Python 执行，用完即弃 | docker / boxlite |
| `terminalExec` | **有状态**终端会话中执行 shell 命令 | docker / boxlite |
| `terminalResource` | 读取 / 导出终端会话内的文件 | docker / boxlite |
| `computerUse` | 在宿主机执行 shell 命令 | sys |
| `readImage` | 读取 / 导出宿主机文件 | sys |

**session（会话）**：`terminalExec` 引入的概念。同一个 `session_id` 复用同一个容器 / microVM，保留文件系统状态。省略 `session_id` 会新建会话；未知 `session_id` 返回 `session_not_found`，除非 `create_if_missing=true`。

**lease（租约）**：session 的空闲存活时间，由 `lease_ttl_sec` 控制，续期是单调的（更短的 TTL 不会缩短现有到期时间）。worker 内的 janitor 循环定期回收过期 session。

**max_inflight**：worker 在 hello 中为每个 capability 声明的并发上限，console 侧据此做配额控制。耗尽时 console 返回 `429 no_capacity`。

**session_busy**：worker 侧错误码，表示目标 session 正忙。console 映射为 HTTP `409`，见 `console/internal/httpapi/command_handler.go` 的 `terminalExecSessionBusyCode`。

## 3. 问题陈述

当前同一个 `session_id` 同时只能执行一条命令。并发请求（包括 `terminalExec` 与 `terminalResource` 之间）会立即返回 `session_busy` / HTTP `409`。

这意味着用户无法在同一个沙箱内并行执行任务，只能串行等待，或者被迫创建多个 session（多个容器），后者既浪费资源又丢失了共享文件系统的语义。

## 4. 目标与非目标

**目标**：将单 session 的并发上限变为可配置项。

**非目标**：改变默认行为。默认值统一为 `1`，即改造后行为与改造前完全一致，需显式配置才开启并发。

**非目标**：修改 API 契约。`session_busy` / `409` 保留，语义调整为「超出单 session 并发上限」。

## 5. 现状分析

### 5.1 串行化位置

串行化完全发生在 worker 侧。

| 工程 | 机制 | 位置 |
| --- | --- | --- |
| `worker-docker` | `terminalSession.busy bool` | `internal/runner/terminal_exec.go` |
| `worker-boxlite` | `TerminalSession.busy: bool` | `src/runner/terminal_session_manager.rs` |
| `worker-sys` | 容量为 1 的 slot channel | `internal/runner/session_client.go` 的 `commandExecSlotCapacity` |

docker 与 boxlite 的 `terminalExec` 和 `terminalResource` 共用同一个 `busy` 标志，因此读文件与执行命令之间也互斥。

`worker-sys` 的单 slot 是**整个 worker 级别**的，且被 `computerUse` 与 `readImage` 共享，粒度比另外两者更粗。

### 5.2 console 侧无需改造

`console/internal/grpcserver/service_dispatch.go` 的 `dispatchCommand` 没有任何 per-session 串行逻辑，只做两件事：

- `pickSessionForDispatch`：按 `session_id` 做 node 亲和路由，保证同一 session 的请求落到同一个 worker
- per-capability 的 `max_inflight` 配额检查

同一 session 的两个并发请求会被正常路由到同一 node 并发下发。**console 不是瓶颈，本次改造不动 console 的调度逻辑。**

### 5.3 并发在语义上是安全的

三个 worker 的每次执行都是独立进程，不共享 shell 状态（cwd / 环境变量 / shell 变量）：

- `worker-docker`：`docker exec <container> sh -lc <command>`，见 `terminalExecDockerExecArgs`
- `worker-boxlite`：`BoxCommand::new("sh").args(["-lc", command])`，见 `src/boxlite_runtime.rs` 的 `exec_terminal_shell`
- `worker-sys`：`/bin/sh -lc <command>`，见 `internal/runner/computer_use.go` 的 `buildShellCommand`

并发后共享的只有文件系统，等价于用户在同一环境中开启多个终端。**因此串行化是应用层的策略选择，不是语义要求。**

### 5.4 运行时均支持并发

- **Docker**：`docker exec` 原生支持同一容器并发多个 exec。
- **boxlite**：已核实源码（见第 12 节查证方法）。guest 侧维护 `ExecutionRegistry`（`HashMap<execution_id, ExecutionState>`，注释明确为 "all active executions"）；客户端每次 exec 获得独立 `execution_id`，`attach` / `wait` / `kill` / `send_input` 均按该 id 路由；传输层为 tonic gRPC over HTTP/2，天然多路复用；`BoxImpl::exec()` 无 per-box 排他锁。
- **worker-sys**：宿主机进程，无运行时限制。

## 6. 设计

### 6.1 四项改动

将 `busy: bool` 替换为并发计数，并补齐三处因去掉互斥而暴露的竞态。以下竞态在当前代码中被 `busy` 掩盖，放开后必现或高频出现。

**(1) 引用计数**

`busy bool` → `inflight int`，新增 per-session 上限配置。超限时返回 `session_busy`。

**(2) 就绪门（容器创建竞态）**

当前流程：请求 A 将新 session 插入 map（此时容器尚未创建）→ 释放锁 → 创建并启动容器。原先第二个请求会被 `busy` 挡住。

放开并发后，请求 B 会在容器创建完成前就从 map 中取到该 session 并尝试执行，导致对不存在的容器执行命令。

解决：session 增加就绪状态（如 `ready chan struct{}` + `initErr`），非创建者阻塞等待创建完成；创建失败时所有等待者收到同一错误。

> **boxlite 上此项为强制项**：boxlite 先以 `box_id: String::new()`（空字符串）插入 session，再调用 `create_session_box()`，最后 `set_box_id()`。而 `claim_existing_session` 直接返回 `box_id`。放开并发后第二个请求会拿到**空 box_id** 去执行，是必现故障而非偶发。
> docker 版是先生成好容器名再插入 map，第二个请求拿到的名字至少是正确的，只是容器可能尚未启动。

**(3) 延迟销毁（生命周期竞态）**

当前流程：命令超时 / 取消时，调用 `destroySession` 强制删除**整个容器**。

放开并发后，一条命令超时会连带杀死同 session 中其他正在执行的命令。

解决：改为标记 session 为待销毁并停止接受新请求，待 `inflight` 归零后再真正删除容器 / box。

> boxlite 的 `execution.kill()` 只终止自身的 `execution_id`，是安全的；真正的破坏源是随后的 `remove_box()`。

**(4) 回收判断**

janitor 的空闲回收条件由 `!busy` 改为 `inflight == 0`。

### 6.2 资源配额

单 session 的并发命令共享同一份资源配额，上限提高会增加 OOM 与进程数耗尽的风险：

| 工程 | 隔离边界 | 默认配额 | 失控影响范围 |
| --- | --- | --- | --- |
| `worker-docker` | 容器 cgroup | `256 MiB` / `1.0 CPU` / `pids-limit 128` | 容器内 |
| `worker-boxlite` | microVM | `256 MiB` / `1 vCPU` | guest 内核 |
| `worker-sys` | **无** | 无 | **整台宿主机** |

`worker-sys` 直接在宿主机执行且无任何资源限制（`configureProcessIsolation` 仅做 `setpgid` 以便向进程组传播信号，不是 cgroup），并发失控会影响整台机器，包括 worker 进程自身。

这是三者默认值统一为 `1` 的主要理由。

### 6.3 错误码语义

| 错误 | HTTP | 含义 |
| --- | --- | --- |
| `session_busy` | `409` | 超出**单 session** 并发上限 |
| `no_capacity` | `429` | 超出 **worker 级** capability 配额 |

两者是不同层级的配额，均需保留。

## 7. worker-docker 改造

改动集中在 `internal/runner/terminal_exec.go` 与 `internal/runner/terminal_resource.go`。

| 改动点 | 定位 |
| --- | --- |
| `busy bool` → `inflight int` + 就绪状态 | `terminalSession` 结构体 |
| 并发上限判断（替换原 `busy` 拒绝分支） | `terminalSessionManager.Execute` |
| 容器创建加就绪门 | `Execute` 中 `createAndStartContainer` 调用处 |
| 超时 / 取消路径改延迟销毁 | `Execute` 中 `destroySession` 调用处 |
| `markSessionIdle` 改为递减计数 | `markSessionIdle` |
| janitor 回收条件 | `cleanupExpiredSessions` |
| `terminalResource` 同步改造 | `terminalSessionManager.ResolveResource` |

新增配置项见第 9 节，解析逻辑位于 `internal/config/config.go`。

**测试影响**：`terminal_exec_test.go` 与 `terminal_resource_test.go` 中有多处直接操作 `manager.mu` 与 `busy` 字段，需同步重写。

## 8. worker-boxlite 改造

### 8.1 依赖升级

`worker/worker-boxlite/Cargo.toml`：`boxlite` 由 `0.7.5` 升至 `0.9.7`（crates.io 最新 release）。

已完成 API 兼容性核对（`v0.7.5` vs `v0.9.7`），worker-boxlite 使用的全部 API：

| API | 结论 |
| --- | --- |
| `BoxliteRuntime::{new, create, get, exists, remove, shutdown}` | 签名相同 |
| `LiteBox::{id, start, exec, copy_out}` | 签名相同，新增 `attach(execution_id)` |
| `BoxCommand` builder 方法 | 相同 |
| `CopyOptions`、`RootfsSpec` | 相同 |
| `BoxOptions` | 新增 `secrets` 字段；构造处已用 `..Default::default()`，不受影响 |
| `ResourceLimits.max_processes` | 相同 |
| `gvproxy` feature | 保留 |

两处实际变更：

1. `BoxliteOptions.image_registries`：`Vec<String>` → `Vec<ImageRegistry>`。唯一的破坏性变更，但 `init_runtime`（`src/boxlite_runtime.rs`）只设置 `home_dir`，不受影响。
2. `Execution::wait()` / `kill()`：`&mut self` → `&self`。属于放宽约束，现有代码仍可编译，预期仅需处理 `unused_mut` 警告。

第 2 项对并发化有利：`&self` 使 `Execution` 可放入 `Arc` 跨 task 共享。

**预期代码改动量：0 行，或 1 行去掉 `mut`。**

**运行时风险（必须实机验证）**：

- boxlite 本地数据库新增 `v7_to_v8` 迁移，升级后首次启动需观察日志。
- `embedded-runtime` + `krunfw` 为默认 feature，guest 内核与 shim 二进制随 crate 版本分发，首次构建会重新下载 libkrunfw，构建耗时较长。
- 版本跨度为 4 个 minor（0.7.5 → 0.8.2 → 0.9.x），upstream 无规范的 BREAKING CHANGE 标记，需依赖上述符号级核对与集成测试兜底。

### 8.2 并发化

与 worker-docker 同构，改动集中在 `src/runner/terminal_session_manager.rs`。

| 改动点 | 定位 |
| --- | --- |
| `busy: bool` → 计数 + 就绪状态 | `TerminalSession` 结构体 |
| 并发上限判断 | `claim_session_for_exec`、`claim_existing_session` |
| 计数递减 | `mark_session_idle` |
| janitor 回收条件 | `cleanup_expired_sessions` |
| 延迟销毁 | `destroy_session` 及其调用点 |
| 就绪门（**强制项**，见 6.1(2)） | `claim_session_for_exec` + `set_box_id` |

`src/boxlite_runtime.rs` 的 `run_command` 本身是 per-execution 干净的，无需改造。

**测试便利**：`TerminalBackend` trait 已有 mock 实现（`StatefulShellBackend`、`BlockingShellBackend`、`BlockingMixedBackend`），可在不启动真实 microVM 的前提下覆盖并发场景。

## 9. worker-sys 改造

**结构与另外两者完全不同**：`computerUse` 与 `readImage` 均为无状态能力，**不存在 session 状态机**，因此不涉及引用计数、就绪门、延迟销毁。`readImage` 只接受固定的 `session_id == "computerUse"`。

改动仅为将硬编码常量改为配置项：

| 常量 | 位置 | 现值 |
| --- | --- | --- |
| `commandExecSlotCapacity` | `internal/runner/session_client.go` | `1` |
| `computerUseCapabilityMaxInflight` | `internal/runner/runner.go` | `1` |
| `readImageCapabilityMaxInflight` | `internal/runner/runner.go` | `1` |

两项额外考虑：

1. **单 slot 被两个 capability 共享**（见 `handleCommandDispatch`），一次 `readImage` 会阻塞 `computerUse`，严格程度超出实际需要。建议拆分为 per-capability 独立 slot。
2. `worker-sys` 无容器沙箱（见其 `README/overview.md` 顶部的 security warning，该工程标注为 `POC Only`），放开并发的前提是宿主机自身具备 cgroup / ulimit 约束。默认保持 `1`，并在文档中说明该前提。

实施时需确认 console 侧是否存在对 `worker-sys` capability `max_inflight` 的硬编码校验（参考 `console/internal/grpcserver/service_connect.go` 的能力校验逻辑），如有则一并放开。

## 10. 配置项汇总

| 环境变量 | 工程 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` | docker / boxlite | `1` | 新增，单 session 并发上限 |
| `WORKER_TERMINAL_EXEC_MAX_INFLIGHT` | docker / boxlite | `4` | 已有 |
| `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT` | docker / boxlite | `4` | 已有 |
| `WORKER_COMPUTER_USE_MAX_INFLIGHT` | sys | `1` | 新增 |
| `WORKER_READ_IMAGE_MAX_INFLIGHT` | sys | `1` | 新增 |

worker 级 `max_inflight` 是 per-session 上限的上界。开启单 session 并发时必须同步调大，否则单个 session 即可占满整个 worker 的配额。此约束需写入文档。

## 11. 实施顺序

仓库约定为「单次改动只能在单一项目中进行」（见 `AGENTS.md`），本次已获准在同一分支内跨三个 worker，但**仍应按工程拆分为独立提交**。

1. **boxlite 升级**（`worker-boxlite`）— 独立提交。先行是因为 `Execution` 的 `&self` 变更会影响后续并发化的写法。需完成 `cargo build`、`cargo test`、实机全链路验证。
2. **worker-docker 并发化** — 独立提交。作为参考实现，Go 构建快、四项改动的模式最清晰。
3. **worker-boxlite 并发化** — 独立提交。平移步骤 2 的模式，注意就绪门为强制项。
4. **worker-sys 配置化** — 独立提交。最简单，无状态机。
5. **console 校验放开**（如确有硬编码）— 独立提交。
6. **文档同步** — 见第 13 节。

## 12. boxlite 结论的查证方法

第 5.4 节关于 boxlite 支持并发 exec 的结论，可按以下方式复核。boxlite 源码位于 `../boxlite`（与 onlyboxes 同级）。

注意 0.7.5 与 0.9.x 的目录结构不同：`boxlite/src/` → `src/boxlite/src/`。

```bash
cd ../boxlite

# guest 侧多执行注册表
git show v0.7.5:guest/src/service/exec/registry.rs

# 客户端按 execution_id 路由
git show v0.7.5:boxlite/src/portal/interfaces/exec.rs

# 传输层（tonic Channel / HTTP2）
git show v0.7.5:boxlite/src/portal/connection.rs

# exec 无 per-box 排他锁
git show v0.7.5:boxlite/src/litebox/box_impl.rs

# 版本间 API 对比示例
git show v0.7.5:boxlite/src/runtime/core.rs   | grep -nE "pub (async )?fn "
git show v0.9.7:src/boxlite/src/runtime/core.rs | grep -nE "pub (async )?fn "
```

## 13. 验证清单

- [ ] `worker-boxlite` 升级后 `cargo build` / `cargo test` 通过
- [ ] `worker-boxlite` 实机验证 box 创建 / exec / copy_out / remove 全链路，确认 guest 资产与数据库迁移正常
- [ ] 三个 worker 在默认配置（上限 `1`）下行为与改造前一致，`session_busy` 仍按预期返回
- [ ] 上限设为 `>1` 时，同 session 并发执行互不干扰，文件系统状态共享正确
- [ ] 并发执行中单条命令超时，不影响同 session 其他执行
- [ ] 并发执行全部结束后，session 能被 janitor 正常回收
- [ ] 待销毁 session 在 `inflight` 归零后才真正删除容器 / box
- [ ] 容器 / box 创建失败时，所有等待就绪的并发请求都收到错误，且 session 被正确清理
- [ ] 超出 per-session 上限返回 `409 session_busy`，超出 worker 级配额返回 `429 no_capacity`
- [ ] `terminalExec` 与 `terminalResource` 可在同一 session 上并发

## 14. 文档同步清单

- [ ] `worker/worker-docker/README/overview.md`：session 并发规则、配置项、默认值
- [ ] `worker/worker-boxlite/README/overview.md`：同上
- [ ] `worker/worker-sys/README/overview.md`：`max_inflight` 固定值描述、security warning 补充并发前提
- [ ] `console/README/overview.md`：`session_busy` 相关描述
- [ ] `README/API.md` 与 `README/API.zh-CN.md`：`session_busy` 语义说明
- [ ] `AGENTS.md` 提到的 `README/release-defaults.md` 当前不存在；若本次涉及默认版本变更需先建立该文件
