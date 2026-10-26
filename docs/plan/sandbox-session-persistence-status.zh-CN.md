# 沙箱持久化实施状态与接手指南

状态日期：2026-10-26。适用分支：`feat/sandbox-snapshot`。本文区分已实现能力、本机验收证据和剩余工作；设计文档中的目标流程不能替代验收结果。

## 目标与首期边界

首期支持文件系统持久化：运行中的会话挂起、提交共享快照、停止并清理源实例，再调度到兼容的同 runtime Worker 恢复。会话 ID 和账号归属保持不变，generation 单调推进，同一时刻只能有一个可写执行所有者。恢复产生新实例和新进程，不恢复进程内存、原 TCP 连接或原宿主 PID。

Docker 与 BoxLite 分别支持自己的快照链路；不包含 Docker/BoxLite 互相导入、bridge-e2b 或 worker-sys。选择 Worker 还须核对 runtime/version、guest platform、格式及固定镜像规格；无兼容节点或无容量时复用 `no_worker/no_capacity/session_capacity_exceeded`，保留有效快照，不创建空白沙箱。

公开执行入口不会自动唤醒。调用方提交 suspend 后按 `Location` 查询操作，直到 `status=succeeded`；`snapshotting/suspended/restoring` 状态的执行请求返回 `409 session_state_conflict`。挂起未结束时请求 resume 也冲突。resume 是另一次异步操作，成功后才能执行。不能以容器停止、上传完成、某个中间阶段或 HTTP 202 代替挂起成功。管理鉴权、幂等键及请求格式以 [API 参考](../API.zh-CN.md#75-挂起--唤醒-session)为准。

共享归档完整提交、远端回读校验、描述对象及数据库引用落盘后，才能清理唯一源数据。恢复目标先保持执行入口关闭，经过 Console 所有权/路由确认及显式激活才接受用户调用。恢复失败不能消费有效快照；未知结果必须保留证据并协调，不通过生成空白根补偿。

## 已实现及已验证范围

| 范围 | 当前状态与证据范围 |
| --- | --- |
| Console | 异步 suspend/resume/delete、持久操作、幂等、账号隔离、代际与路由门禁、兼容调度、S3 multipart/续签/完整回读/不可变描述、删除积压与无引用对象回收已接入。无已完成对象可列举的未登记 multipart 仍依赖 bucket 生命周期。 |
| Docker | 原生导出/导入、journal、独立租约 guard、源回滚/清理、目标激活/取消、永久删除与已完成删除历史压缩。真实 Console/MinIO 下的跨逻辑 Worker、原节点缓存、旧缓存不得覆盖新快照、离线无 Worker、Console/Worker 中断及清理有本机证据。两个 Worker 使用同一 OrbStack daemon，不能视为跨物理机器验收。 |
| BoxLite | 固定 SDK 0.10.7；生产适配器、SDK 初始化前归属清单、home/journal/store 锁与绑定、独立 lease 协议 v2、导出/回滚/导入/激活/取消/删除、传输及缓存锁、空间准入、周期缓存回收、普通初始会话到期回收。真实 Console/MinIO 下的多轮跨逻辑 Worker、返回源节点、重启及清理有本机证据。 |
| BoxLite 异常保留 | 未登记根拒绝 SDK 初始化；已确认停止的初始/托管根在满足各自条件时保留隔离，部分场景允许 Worker 注册并处理其他会话。上线不表示该根可以执行或删除；托管根仍须明确的较高代次 DELETE。 |
| BoxLite 初始创建 | SDK create 前持久化规范化选项摘要、创建者及 guard 身份；ID 不可替换。对无本地 boot、同一 guard、创建者已退出、无外部启动/租约历史且仅有 Configured/可信准备文件的创建，自动记录 abandonment、清理、保存完成回执；本地 g1 拒绝普通重建，允许授权的较高代次共享快照恢复。详见[启动说明](../../worker/worker-boxlite/docs/sandbox-snapshot-startup.md#尚未启动的创建终结)。 |

性能及数据完整性已有局部证据，不代表完整性能验收：

- BoxLite 1 GiB 跨 Worker 下载及同 Worker 缓存复制，有逐阶段耗时和采样磁盘曲线；4 × 1 GiB 的无归档缓存、实际缓存复制分别通过；5 GiB release 的完整 Console/S3 周期通过。
- 1 GiB SDK 镜像冷/热顺序各一次已验证真实拉取/复用，未控制 OS 页缓存及 registry/CDN。
- 1 GiB 主文件加 65,536 个独立随机 1 KiB 文件，逐项核对哈希、权限、UID/GID、目录集合与链接/inode 关系，完整跨 Worker 生命周期及清理通过。
- Docker 原生 1/5/10 GiB、4 × 1 GiB 有本机验收；不能将原生 fixture 的结果写成完整 Console/S3 性能矩阵。
- 样本、配额、计时和磁盘统计限制见[存储设计末尾的证据索引](sandbox-session-persistence-storage-api.zh-CN.md#首期验收清单)。样本是本机测量，不是生产 SLO。

## 最新一批变更的验收

[2026-10-26 创建终结样本](../../worker/worker-boxlite/docs/sandbox-snapshot-initial-creation-recovery-sample-2026-10-26.json)记录本批代码的验证：

- `worker/worker-boxlite`：`cargo test --locked --all-targets`。核心 library 200、Worker 138、独立 guard 进程测试 2 通过；引入 Worker 模块的 examples 还会重复执行相关测试，不能把重复测试算成新的场景。
- 原生 `boxlite_initial_creation_recovery_smoke`：7 个正向窗口、7 个拒绝窗口通过。包括 ID 未选定/已选定、shim 准备中、授权清理重放，以及磁盘/配置/启动历史/epoch/迟到未知记录的保留拒绝。
- Python payload、生命周期、阶段采样、并发和镜像探针合计 41 项测试通过。
- 真实 Console/MinIO、两台本机 macOS arm64 release Worker、Linux arm64 guest、1 MiB 数据、无归档缓存的跨 Worker 验收通过：目标前缀创建者 SIGKILL → 生产启动清理 → 较高代次恢复同一 SID → 业务校验 → DELETE → 物理清理 → 目标 Worker 再次重启，原回执保留且无重建，容量及清理积压均为零。

服务注入使用原生 SDK 前缀子进程构造现场，生产 Worker 执行恢复；它不是完整 Worker 初始创建方法任意指令处的随机崩溃验收。原生用例中的数据库先删残留为重建窗口。不能据此关闭下一节的未知启动、跨 epoch 或宿主重启待办。

## 剩余工作与完成条件

以下均未完成，接手时不要把当前分支标记为全量完成，也不要为跑通验收降低准入或证据要求。

| 优先级 | 工作 | 完成条件 |
| --- | --- | --- |
| P0 | 跨物理 Worker 部署验收 | 为同一 runtime 准备两台真实宿主、独立原生数据盘，仅共享 Console/S3；源共享提交并清理后让源节点不可达，目标恢复同 SID、新 generation、新实例和完整数据；覆盖原节点返回、无可用 Worker、失败重试、最终两端物理清理。Docker、BoxLite 分别验收，不能用同机双进程或同 daemon 代替。 |
| P0 | BoxLite Linux 宿主 VM 验收 | 在受支持 Linux 宿主运行真实 guest、独立 guard 和完整 Console/S3 链路，验证 writer cgroup、进程退出证明与生命周期恢复。现有无 VM/Linux 进程测试不等于 Linux BoxLite VM 通过。 |
| P0 | 主机重启、boot/PID namespace 与 guard epoch 变化后的协调 | 明确旧 writer 已退出、原生磁盘归属、旧授权撤销与新授权建立的协议及持久回执；分别注入 guard/Worker/宿主重启，证明不双写、不丢唯一数据、不同 epoch 不借用旧授权。当前部分场景只保留隔离，部分拒绝启动，不能手动重写配对标记。 |
| P0 | BoxLite 初始 boot 已登记后的未知结果 | 覆盖 initial_boot 落盘、guard prepare 请求在途、实际 start 返回丢失、writer 绑定及 adoption 前后的中断；只能在精确内核退出/外部授权和持久证据核对后取消或恢复。不能仅凭 guard 查询为 None、SDK stopped 或墙钟到期推断没有迟到启动。较高代次快照 target 的既有故障恢复不能代替初始创建窗口验收。 |
| P1 | 未登记原生数据及 SDK 临时残留的协调 | 对没有正向创建/清理意图的根，设计可审计的认领/保留/清理授权和重放；覆盖数据库/目录单边残留、混入未知根、名称相似及活跃 writer。默认继续保留拒绝；不能按名称、Console 清单缺席或目录年龄直接删除。 |
| P1 | BoxLite 长期 journal/lease 历史维护 | 当前 codec v2 只共享重复规格，仍保留历史，journal/展开预算 64 MiB、原生清单上限 16,384。设计回执退休/压缩及重开协议，验证迟到控制、旧 generation、删除栅栏、未知结果和回收凭据不会被丢弃。Docker 仅已完成删除可压缩，不应误写为全部历史均已维护。 |
| P1 | 完整性能和容量矩阵 | 对两种 runtime 分别测 1/5/10 GiB、4 并发、镜像与归档冷热组合及多次重复，记录逐阶段、真实磁盘峰值/采样限制、操作期限、传输与最终清理；在实际部署存储和网络上确定预算与 SLO。5 GiB BoxLite 成功不代表 10 GiB 或全矩阵完成。 |

性能预检曾因本机空间不足拒绝后续测量：当时 host/daemon 可用约 90.8/74.9 GB，部分 5 GiB/四并发预算超过 daemon 空闲，10 GiB 预算约 133.1 GB。数值是历史观测，继续前必须重测当前两个文件系统及配置；不得关闭空间检查、缩小低于实际 qcow 展开量的上限或清理用户/未知数据来绕过。需要更多空间时使用有足够容量的独立测试环境。

自动按需唤醒、异构 runtime 导入、bridge 支持和内存快照属于首期之外，不能在未确认范围时混入上述待办。

## 接手入口与建议顺序

1. 先阅读[生命周期与时序图](sandbox-session-persistence-lifecycle.zh-CN.md)、[存储/API/性能设计](sandbox-session-persistence-storage-api.zh-CN.md)、两份统一 [API](../API.md) / [中文 API](../API.zh-CN.md)，再按 runtime 阅读部署和启动说明。
2. 查看 `git status`、本分支提交及本地服务状态，区分代码、公开脱敏样本与被忽略的私有环境。不要批量启动历史 fixture。下一项有充分本地条件的实现工作是初始 boot 未知结果协调或历史维护；跨物理机器/Linux VM/SLO 工作需要实际环境，无法取得时明确记录未验证。
3. 实现故障恢复前先列出持久意图、外部授权、精确 writer 和清理回执的判定表，补对应故障窗口测试，再跑原生 SDK 和真实 Console/S3 回归。不得把未知状态归零或以新目录成功掩盖原失败。
4. 用独立状态目录/端口运行 `scripts/dev.sh`。通用步骤与 flag 见 [scripts 说明](../../scripts/README.md#本地沙箱持久化验收)，配置模板为 `scripts/snapshot.env.example`。Worker 凭据由脚本创建并复用，文件 600、目录 700；不提交或打印凭据、完整恢复规格、私有路径、签名 URL 和底层 SDK 原始错误。
5. 新验收须保存不可覆盖的私有结果和公开脱敏样本，更新本文相应状态，并把“实现完成”和“环境验收通过”分别记录。新增提交按明确路径暂存，保留本机覆盖文件。

关键实现入口：

| 工程 | 文件/目录 |
| --- | --- |
| Console | `console/internal/sandboxsnapshot`、`console/internal/grpcserver/snapshot_*`、`console/internal/objectstore`、`console/internal/httpapi/snapshot_handler.go` |
| Docker | `worker/worker-docker/internal/runner`、[生命周期说明](../../worker/worker-docker/docs/sandbox-snapshot-controller.md) |
| BoxLite | `worker/worker-boxlite/src/persistence`，特别是 `home.rs`、`native_preflight.rs`、`native_creation_recovery.rs`、`initial_creation.rs`、`journal.rs`；`src/runner/persistence_startup.rs`、`snapshot_native_engine.rs`、`terminal_persistence.rs` |
| 验收 | `scripts/tests/snapshot_e2e.py`、`snapshot_stage_metrics.py`、`snapshot_parallel_e2e.py`，以及 `worker/worker-boxlite/examples/*smoke.rs` |

构建 BoxLite 需要固定 SDK 0.10.7 以及 PATH 中的 `protoc >= 3.12`；SDK 不读取 `PROTOC` 环境变量。原生用例先构建 `boxlite-lease-guard`。本机可使用 cargo registry 中的 vendored protoc，但其绝对路径不应成为项目运行依赖。

```bash
# 在 worker/worker-boxlite 中运行；protoc 须已在 PATH。
cargo test --locked --all-targets
cargo build --locked --bin boxlite-lease-guard
cargo run --locked --example boxlite_initial_creation_recovery_smoke

# 在仓库根目录运行。
python3 -m unittest scripts.tests.snapshot_payload_test scripts.tests.snapshot_e2e_test scripts.tests.snapshot_stage_metrics_test scripts.tests.snapshot_parallel_e2e_test scripts.tests.snapshot_image_cache_test
```

## 本机现场与私有证据

以下仅为当前开发机接手定位，路径均相对仓库根目录，不能当作可迁移的运行配置。默认开发服务及其他用户工作负载不属于验收资源。

- 最新成功组：`scripts/.dev/snapshot-boxlite-initial-creation-e2e`，session 为 `onlyboxes-snapshot-boxlite-initial-creation`，HTTP/gRPC/S3 端口为 `18102/15064/19013`，私有配置为该目录的 `dev.env`。本批验收完成后通过 `dev.sh stop sandbox-persistence` 停止；测试 guest 磁盘已清理，journal/账本/名称栅栏和结果继续保留。停止整组包含守护器退出，不应据此假定重启仍是原 epoch。
- 最新结果：该目录中 `snapshot-e2e-cross-worker-creation-recovery-result.json`、`snapshot-stage-metrics-cross-worker-creation-recovery.json` 及同名前缀私有日志；标签 `creation-recovery` 已使用，不能删除结果后重跑。原生 14 用例日志位于 `/tmp/onlyboxes-boxlite-creation-recovery-final-smoke.log`，完整 Rust 复核日志位于 `/tmp/onlyboxes-snapshot-handoff-all-tests.log`；临时日志可能被系统回收，公开样本和提交的测试才是可移植证据。
- **保留的失败创建现场**：`scripts/.dev/snapshot-boxlite-image-cache-e2e` 的旧 `initial-creation` 失败标签含未规范化选项摘要的无 boot 意图，已有真实 Configured 记录，且 guard epoch 已变化。不能改摘要、补 ID、删意图或启动后强行清理；新的严格恢复理应拒绝。该目录其他标签有成功的历史样本，不能因一项失败整体重置。
- **保留的未知/旧 epoch 根**：`scripts/.dev/snapshot-large-v2-e2e` 内有旧停止初始根；`scripts/.dev/snapshot-e2e` 内有旧 lease 协议 v1 的未知结果。不能用 v2 新守护器伪造 release、删除卷/home/journal 或重绑身份。所有其他旧 fixture 也须先核对证据及资源归属再操作，不能按目录名批量删除。
- `docker/docker-compose.override.yml` 是未跟踪的本机覆盖文件，不属于本分支持久化实现；保留，不读取其中配置、不暂存、不提交。`scripts/.dev` 与所有凭据/私有验收原始文件也不提交。

复用最新成功组时，先设置匹配的 `ONLYBOXES_DEV_SESSION`、`ONLYBOXES_DEV_STATE_DIR`、`ONLYBOXES_DEV_ENV`，再通过 `scripts/dev.sh status sandbox-persistence` 检查。已有结果、需要冷缓存、权限/身份变化或不确定旧资源状态时，优先另建隔离验收目录；创建新环境不能替代旧状态的故障恢复任务。
