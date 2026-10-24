# BoxLite SDK 初始化前的归属校验

BoxLite `0.10.7` 初始化时会更新恢复状态、回收没有数据库记录的 Box 目录，并处理原生本地 snapshot 的 pending 标记。持久化 Worker 必须先核对 home/journal 归属和原生资源清单，再允许 SDK 打开 home。会话恢复握手和执行门禁不能代替这个顺序。

当前提供 `RuntimeHomeGuard` 和 Worker 内部的 `boxlite_runtime::initialize_guarded`，原生生命周期验收已使用该入口。生产启动通过[显式配置](sandbox-snapshot-deployment.md)绑定专用 home/journal 与独立 execution lease，再注册生命周期引擎和 `sandbox_persistence`。默认关闭。这份说明不代表跨 Worker 或真实 Console/S3 验收已经完成。

## home 与 journal 的绑定

首次使用需要两个独立、互不嵌套的绝对目录：专用 SDK home 和 journal 根目录。目录必须属于当前用户且权限为 `700`；不存在时创建，不修改已有目录的权限。初始 home 和 journal 必须为空，不能凭一个空 journal 采纳已有 SDK 数据，也不自动迁移未登记的旧 home。

| 文件 | 作用 |
| --- | --- |
| home 中的 `.onlyboxes-owner.lock` | 在 SDK 初始化之前取得、覆盖受保护 home 生命周期的独占锁 |
| home 中的 `.onlyboxes-owner.json` | 记录 Worker、宿主身份及配对信息 |
| journal 中的 `native-home.json` | 相同配对信息的反向记录，防止换用空 journal |
| journal 的 `owner.json`、`journal.json`、`journal.lock` | 原有 durable 状态、Worker 绑定及独占锁 |

两份配对记录都使用私有普通文件、单硬链接、原子替换和文件/目录 sync。它们包含精确 SDK 版本、Worker ID、宿主机 ID、boot ID、PID namespace、随机配对 ID，以及两个目录的规范路径、设备号和 inode。记录不包含 Worker secret、镜像环境变量或对象存储授权。

复制目录、改名搬迁目录、替换目录、换用 Worker ID、换用 journal、标记丢失或损坏、异常权限、符号链接和非法硬链接均不能自动采纳。首次配对中途失败或仅剩一份标记时保持不可用，不通过重写标记重置 generation。Guard 释放时先关闭 journal、使已有引用失效，再显式释放 home 锁；并发 fork 短暂继承的描述符不能延迟所有权释放或解除后续所有者的锁。

当前对 boot ID 和 PID namespace 变化采取明确拒绝策略，不自动重新绑定已有 home。重启后安全重绑定仍需完成；不得手动改写标记以跳过旧 writer 的退出证明。

## SDK 初始化前的资源清单

重开已有配对时先以只读 SQL 核对固定 schema `10`，不调用 SDK、不运行迁移、不创建空数据库代替丢失数据。初始化入口在注册 Guard 后才创建 SDK runtime；同一进程已经初始化过 SDK 时不能事后补装 Guard。后续 SDK 访问继续核对目录及配对标记，journal 关闭后也不能继续使用该入口。

清单核对包括：

- `boxes`、`db`、数据库及 WAL/SHM 的文件类型、归属与写入权限；拒绝重定向路径和非法硬链接。
- 配置与状态记录数量、原生 ID、唯一名称、配置中的 Box home、状态列与状态 JSON 的一致性。最多接受 16,384 条原生记录，配置/状态 JSON 各不超过 64 KiB。
- 物理 Box 目录必须具有对应数据库配置，或属于下述已登记清理意图允许回收的残留。未知 orphan 保留并拒绝启动，让 SDK 的自动清理保持不可达。
- journal/probe 固定的 ID 必须匹配原生名称。恢复实例使用对应操作和 generation 派生的名称；未确认 ID 的导入只允许匹配已记录操作的停止实例，后续仍由原生导入适配器校验独立磁盘及完整规格。
- 已确认名称占位必须保留原 ID、对应的精确配置、无 PID 且无 guest 磁盘。丢失、替换或异常启动的占位不能被当作可用资源。
- 真实终端/恢复实例必须保留 `auto_delete=0` 和 `detach=true`；名称占位按精确配置使用 `detach=false`，仅允许无 PID、无 guest 磁盘的 configured/stopped 状态。已登记 home 内尚未进入持久化操作的普通终端、以及 unnamed 单次 Python Box 仍按各自原有生命周期处理。
- `.snapshot_pending` 不属于平台文件系统快照协议。发现后保留标记并拒绝启动，不让 SDK 自动重写其磁盘或 backing 路径。

清单还校验操作中保存的 `BootExecutable` 属于配对 SDK home 和同一 Box ID。已完成操作可以保留绑定元数据而不保留已清理的 shim 文件。受控启动前的可执行文件准备和 PID 文件缺失时的内核识别见[执行租约说明](execution-lease.md#启动前的写入者识别)。

清单证明资源属于可核对的本地状态，不授予执行权。SDK 打开后，恢复动作仍要检查完整规格、实际进程身份、generation、有效 lease、连接恢复 ACK 和显式激活；不会因为清单通过而启动停止实例或创建替代根。

普通 SDK 初始化入口若发现受保护 home 的归属标记或锁文件，会明确拒绝，不能通过漏装 Guard 绕过校验。生产持久化配置开启后强制使用受保护入口，并在 journal 固定外部守护器 store 与账本目录身份；不能把删除全部归属信息后改用普通启动当作恢复方案。

## 数据库先删的已知清理残留

`RuntimeHomeGuard::open` 在已有配对目录中先执行初始化前的恢复，再运行严格只读清单；`census()` 本身仍不删除文件。恢复只处理同时满足这些条件的原生目录：数据库中已没有该 ID，当前操作的 durable `cleanup_box_ids` 和 `cleanup_names` 已登记同一 ID/名称，且清理授权属于该 scope 当前的操作/generation。

| 当前操作 | 可回收条件 |
| --- | --- |
| source committed/finalized | 已共享提交、完整远端回读、归档已校验，且源停止确认和原生名称匹配 |
| target canceling/canceled | 匹配操作/generation 派生名称，且已登记目标取消清理意图 |
| session deleting/deleted | 更高代次的永久删除意图，名称属于该 scope 的原终端或历史恢复操作 |

恢复聚合该原生 ID 在同一 scope 历史中的 source、started 和 cleanup 进程证据，要求每个已记录的 lifecycle 都已退出。曾尝试启动但未记录启动进程的结果不能用另一个旧进程已退出代替，必须有当前清理动作记录的进程退出证据。仅在受控协议下从未选中/启动的导入 ID 可使用未启动判定；现场必须没有未知 PID 记录。

如果残留目录还有 `shim.pid`，它必须是私有的受限普通文件，PID 和进程起始时间都要匹配已确认退出的记录。异常链接、超限或未知 fingerprint 都保持关闭。恢复不会通过 PID 文件去 signal 一个进程，也不调用 SDK 的 force remove。已确认的名称占位不能被当作普通残留删除。

规划先检查全部目录；混入未知、未提交或仍有活跃 writer 的目录时不启动任何回收。journal 的状态锁覆盖规划与物理删除，删除前再次核对退出证据，按精确 ID 删除并 sync 父目录。真实 I/O 故障可能留下部分已授权清理，下一次启动继续核对；不会据此放宽其他目录的授权。

这一步不更新阶段、不生成最终 ACK，也不清理共享对象或归档。后续控制器仍须完成原生名称占位、必要的 artifacts 清理、finalized/canceled/deleted 状态写入与 Console 确认。未提交且没有永久 DELETE 授权的源磁盘、未知目录、丢失的数据库、异常 schema、原生 pending snapshot 仍保留并拒绝启动。

## 启动失败定位

启动错误使用固定阶段名区分目录布局、租约账本、守护器发现/宿主身份、home 所有权、账本绑定、pythonExec home、SDK 初始化、原生适配器和控制引擎。错误不附带底层 SDK、IO 或传输错误原文，避免路径、配置及授权信息进入日志。已有 home 的 journal 打开和原生清单恢复失败另有 `journal_open`、`native_reconciliation` 阶段日志。

`control_engine` 阶段中，初始会话恢复进一步区分 `initial_companion_identity`（保存的守护器身份不匹配）与 `initial_writer_lease`（同一守护器未确认匹配的运行租约）。只有 Worker ID 和 store ID 相同不足以授予执行权；运行恢复要求 epoch 和进程身份同样匹配。守护服务重启后的运行恢复和清理协调尚未实现，不能重写 journal 绕过检查。下述已知停止初始根可保留隔离，让 Worker 完成注册，但不会因此恢复该会话。

本机旧测试目录在写满 guest 磁盘、普通会话逻辑删除及整组服务重启后，重现了 `control_engine` / `initial_companion_identity` 拒绝。配对目录和原生清单通过，当前守护器与旧记录的 Worker/store 相同，但 epoch/进程不同。更改新会话磁盘容量的同时也重启了守护器，因此该结果不能归因于磁盘容量配置。该目录的原生数据与账本继续保留；新目录中通过的快照周期不能替代旧状态恢复验收。

## 已知停止初始根的保留

初始会话已经确认启动，但从未进入快照生命周期时，guest 退出、租约撤销或普通会话逻辑删除可能留下受保护的本地根。启动允许将它保留为不可执行的待协调资源，前提是同时验证：

- journal 保留同一 scope/Box ID 的 probe、未标记 managed 的初始 boot 和已确认 adoption；该 scope 不存在生命周期当前操作。
- 当前独立守护器绑定的 Worker/store 与旧记录一致，SDK home 与账本目录继续通过既有绑定校验。
- 最初发起 SDK 启动的 Worker 进程已退出；守护器身份变化时，旧守护器的精确进程身份也已确认退出。
- 已记录的写入者及 Linux 上绑定的整个 writer cgroup 均已退出。无法取得完整退出证明时继续拒绝。
- 对当前存活守护器的 RPC 返回同一 Box ID、scope、generation、writer 的已撤销租约，revision 和期限不早于 journal。记录丢失、无法连接、仅经过墙钟期限或仅有 stopped 状态均不满足条件。

通过后保留原 boot/adoption、服务 epoch、磁盘和 probe，不修改 journal，不续租、不启动原实例、不创建替代实例，也不删除根。该 scope 的命令、文件、预览和恢复入口继续关闭；名称受 orphan 清理保护，并继续占用会话容量。Worker 可以注册并处理剩余容量中的其他会话。日志阶段为 `stopped_initial_held`。

未知启动结果、未确认 adoption、缺失 boot、managed 初始根，以及已有 source/target/delete 等生命周期当前操作不适用此路径。它也不会把 Console 中看不到的会话解释为删除授权；后续运行恢复、永久清理与容量释放仍需要独立协调。

本机旧目录验收先核对 journal 和 qcow2 字节指纹，再启动整组服务。此前失败的 Worker 以此路径上线，旧 Box 仍为 stopped，原 journal 与 612,499,456 字节磁盘保持一致。同一目录随后通过新的 1 MiB 会话的真实 Console/S3 跨 Worker 周期：在保留旧根的 Worker 创建新会话，挂起后停止源 Worker，在另一 Worker 恢复并校验文件，重新启动源 Worker，最后永久删除新会话并排空其清理队列。挂起约 25.25 秒、恢复约 39.32 秒，归档 40,091,612 字节。完成后旧 CID 仍 stopped、磁盘字节不变，初始 boot/adoption/probe 继续保留。新会话清理完成不表示这份旧根已被回收。

该结果证明保留隔离、Worker 可注册及其他会话的跨 Worker 生命周期可用，不证明旧会话可继续执行，也不替代宿主重启或跨物理机器验收。

## 验证

```bash
cargo test --locked
cargo build --locked --bin boxlite-lease-guard
cargo run --locked --example boxlite_home_guard_smoke
cargo run --locked --example boxlite_lifecycle_controls_smoke
```

无 VM 测试覆盖有授权残留的幂等清理、阶段/归档不变、无清理意图和未提交源拒绝、未知目录混合保留、活跃/未知 PID 拒绝、未启动导入与未知启动结果区分、永久删除及历史 lifecycle 退出核对；也覆盖双向配对、独占锁及继承描述符、目录替换、复制标记、不同 Worker/host/boot/namespace、丢失历史、未登记数据、链接/权限拒绝，以及 orphan、原生 pending snapshot、规格名称冲突和不支持 schema 的原样保留。

`boxlite_home_guard_smoke` 创建专用临时 home、SDK 宿主子进程和独立守护子进程，先确认账本采纳及续租，再强制结束 SDK 宿主，模拟 Worker 进程丢失。新进程先取得 Guard 再打开 SDK，核对原 VM 的 CID、PID、进程起始时间、业务文件及仍增长的后台写入，整个重开路径不调用 start。它还验证普通入口拒绝受保护 home、拒绝事后重新注册，以及 SDK 启动前拒绝 orphan 并保持其字节不变。独立守护子进程不初始化 SDK，在旧期限后仍保留运行、在续租期限到期时精确停止；父进程确认退出并保留 guest 磁盘后，才由 SDK 完成授权清理。它还保留真实 VM 的清理 ID 和退出证据，重建数据库先删而物理目录残留的故障窗口，由新进程在 SDK 初始化前回收，并重放永久删除确认名称占位 ID 不变。这是故障窗口重建，不是在任意 SDK 指令处中断数据库/文件系统提交的验收。

完整生命周期验收在 Guard 下执行快照、回滚、共享对象传输、缓存恢复、显式激活和永久删除；SDK 所有任务结束后，再运行只读清单核对保留的名称占位。

这些实验只证明本机测试进程重开和原生归属边界，以及[独立租约子进程](execution-lease.md)在 SDK 宿主 SIGKILL 后保留续租、到期停止和保留磁盘。VM 依靠持续运行的独立子进程执行期限，不会因本地状态文件存在就自行到期停止。它不证明完整 Worker 生产注册、守护服务监督、宿主机重启、Linux BoxLite VM 或跨机器调度已完成验收。
