# BoxLite SDK 初始化前的归属校验

BoxLite `0.10.7` 初始化时会更新恢复状态、回收没有数据库记录的 Box 目录，并处理原生本地 snapshot 的 pending 标记。持久化 Worker 必须先核对 home/journal 归属和原生资源清单，再允许 SDK 打开 home。会话恢复握手和执行门禁不能代替这个顺序。

当前提供 `RuntimeHomeGuard` 和 Worker 内部的 `boxlite_runtime::initialize_guarded`，原生生命周期验收已使用该入口。生产启动配置、生命周期引擎注册和独立 execution lease 尚未完成，Worker 仍不声明 `sandbox_persistence`。这份说明不代表跨 Worker 或真实 Console/S3 验收已经完成。

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

当前对 boot ID 和 PID namespace 变化采取明确拒绝策略，不自动重新绑定已有 home。重启后安全重绑定、已知清理残留的初始化前核对和修复仍需完成；不得手动改写标记以跳过旧 writer 的退出证明。

## SDK 初始化前的资源清单

重开已有配对时先以只读 SQL 核对固定 schema `10`，不调用 SDK、不运行迁移、不创建空数据库代替丢失数据。初始化入口在注册 Guard 后才创建 SDK runtime；同一进程已经初始化过 SDK 时不能事后补装 Guard。后续 SDK 访问继续核对目录及配对标记，journal 关闭后也不能继续使用该入口。

清单核对包括：

- `boxes`、`db`、数据库及 WAL/SHM 的文件类型、归属与写入权限；拒绝重定向路径和非法硬链接。
- 配置与状态记录数量、原生 ID、唯一名称、配置中的 Box home、状态列与状态 JSON 的一致性。最多接受 16,384 条原生记录，配置/状态 JSON 各不超过 64 KiB。
- 物理 Box 目录必须具有对应数据库配置。发现 orphan 时保留目录并拒绝启动，让 SDK 的自动清理保持不可达。即使是已知删除阶段的数据库先删、物理目录后删残留，也不会未经核对直接交给 SDK 清理。
- journal/probe 固定的 ID 必须匹配原生名称。恢复实例使用对应操作和 generation 派生的名称；未确认 ID 的导入只允许匹配已记录操作的停止实例，后续仍由原生导入适配器校验独立磁盘及完整规格。
- 已确认名称占位必须保留原 ID、对应的精确配置、无 PID 且无 guest 磁盘。丢失、替换或异常启动的占位不能被当作可用资源。
- 真实终端/恢复实例必须保留 `auto_delete=0` 和 `detach=true`；名称占位按精确配置使用 `detach=false`，仅允许无 PID、无 guest 磁盘的 configured/stopped 状态。已登记 home 内尚未进入持久化操作的普通终端、以及 unnamed 单次 Python Box 仍按各自原有生命周期处理。
- `.snapshot_pending` 不属于平台文件系统快照协议。发现后保留标记并拒绝启动，不让 SDK 自动重写其磁盘或 backing 路径。

清单证明资源属于可核对的本地状态，不授予执行权。SDK 打开后，恢复动作仍要检查完整规格、实际进程身份、generation、有效 lease、连接恢复 ACK 和显式激活；不会因为清单通过而启动停止实例或创建替代根。

普通 SDK 初始化入口若发现受保护 home 的归属标记或锁文件，会明确拒绝，不能通过漏装 Guard 绕过校验。生产持久化配置接入仍须强制使用受保护入口；不能把删除全部归属信息后改用普通启动当作恢复方案。

## 验证

```bash
cargo test --locked
cargo run --locked --example boxlite_home_guard_smoke
cargo run --locked --example boxlite_lifecycle_controls_smoke
```

无 VM 测试覆盖双向配对、独占锁及继承描述符、目录替换、复制标记、不同 Worker/host/boot/namespace、丢失历史、未登记数据、链接/权限拒绝，以及 orphan、原生 pending snapshot、规格名称冲突和不支持 schema 的原样保留。

`boxlite_home_guard_smoke` 创建专用临时 home 和测试子进程，由父进程强制结束该 Worker 子进程。新进程先取得 Guard 再打开 SDK，核对原 VM 的 CID、PID、进程起始时间和业务文件，整个重开路径不调用 start。它还验证普通入口拒绝受保护 home、拒绝事后重新注册，以及 SDK 启动前拒绝 orphan 并保持其字节不变。测试最后用精确记录的实例完成 SDK 停止与清理。

完整生命周期验收在 Guard 下执行快照、回滚、共享对象传输、缓存恢复、显式激活和永久删除；SDK 所有任务结束后，再运行只读清单核对保留的名称占位。

这些实验只证明本机测试进程重开和原生归属边界。VM 在 Worker 被强制结束后继续存在，说明独立 lease 约束仍然必要；本测试不证明过期 VM 会自行停止，也不证明宿主机重启、Linux detached PID namespace 的所有 writer 退出或跨机器调度已完成验收。
