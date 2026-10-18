# BoxLite 原生归档与恢复

Worker 固定依赖 BoxLite `0.10.7`，使用原生 `.boxlite` 归档验证同 runtime 的文件系统恢复。当前 Worker 尚未声明 `sandbox_persistence` 能力，也尚未接入 Console 的挂起、上传、恢复、激活和删除协议。

## 原生验收

```bash
cargo run --locked --example boxlite_snapshot_smoke
```

需要可运行 BoxLite 的本机环境、`protoc >= 3.12` 和可访问的镜像 registry。默认镜像使用固定 digest，包含 `/bin/sh` 与 Python。可通过 `BOXLITE_SNAPSHOT_SMOKE_IMAGE` 指定另一个带 digest 的 Debian/Python 镜像；样本会删除 guest 内的 `/etc/debian_version` 以验证基础镜像文件的删除行为。

验收只创建并删除本次调用拥有的临时目录和原生 Box：

1. 子进程在独立 source home 创建并启动 Box，写入文件与元数据样本。
2. 调用 `stop()`，核对状态为 `Stopped`，保留原生实例并导出归档。
3. 源进程退出后，父进程删除整个 source home，包括源数据库、镜像缓存、backing 文件和 Box 目录。
4. 另一子进程在全新的 target home 导入归档，核对导入本身不启动实例、目标原生 ID 与源不同。
5. 显式启动目标，验证文件系统与启动配置，再删除目标和测试目录。失败时保留本次测试目录用于排查。

验收失败会返回非零退出码。输出包含归档字节数、SHA-256、导出和导入耗时。

2026-10-18 在 macOS/arm64 上运行 BoxLite 0.10.7 的结果：

| 项目 | 结果 |
| --- | --- |
| 归档大小 | 38,838,232 bytes |
| 导出 | 2,754 ms |
| 导入 | 1,474 ms |
| 普通文件内容、权限、UID/GID | 通过 |
| 硬链接、符号链接、FIFO、user xattr | 通过 |
| 8 MiB 稀疏文件逻辑内容与空洞 | 通过；恢复后占用 8 个 512-byte blocks |
| 基础镜像文件变更与删除 | 通过 |
| 归档内环境变量 | 通过 |
| `/dev/shm` 临时数据不进入恢复 | 通过 |
| 源 home 删除后的新进程、空 home 恢复 | 通过 |

这些是小样本的本机结果，不代表跨机器验收、生产容量、S3 吞吐或 1/5/10 GiB 性能目标。另一个通用 smoke 覆盖并发执行、单执行取消、文件复制与资源删除：`cargo run --locked --example boxlite_smoke`。

原生集成回归包括 detached Box 的正常重启恢复、宿主进程强制退出恢复、guest HTTP tunnel、命令执行和资源清理：

```bash
cargo test --locked --features integration --test integration -- --test-threads=1
```

这组测试运行真实 VM，需要与上述归档验收相同的运行条件。

## 生命周期接入约束

原生 export 会在磁盘复制期间 quiesce，但复制结束会恢复运行中的源实例。平台挂起必须先关闭命令和预览入口、排空执行，再停止所有写入；不能把直接对运行中 Box 调用 `export()` 视为已挂起。当前验收使用停止后的导出，不保留进程或内存。

归档可以在空 home 导入，但目标启动仍需要访问基础镜像 registry。恢复规格必须使用固定平台 digest；不能将可变 tag 的一次成功恢复当作基础镜像一致性的证明。

SDK 的 `BoxArchive::new` 采用可信本地归档策略，会保留归档携带的启动与安全配置。平台适配器必须在导入前核对受控恢复规格、路径和挂载边界、完整 SHA-256、大小及展开预算，再决定导入策略。原生 SDK 的校验不能代替 Console 的账号归属、generation、所有权和激活栅栏。

Console 协议接入还需完成 journal 的运行时接入、连接恢复和执行门禁、直接对象传输与缓存、目标容量预留、激活确认、删除回收，以及 Worker 进程失效后的独立 lease 约束。完成前不向调度器声明此后端支持持久化恢复。


## 持久化 journal

`worker_boxlite::persistence` 提供 BoxLite Worker 的私有恢复规格、归档描述与操作 journal。它是生命周期控制器的存储原语，当前尚未在 Worker 启动、命令执行或恢复握手中启用；journal 测试通过不代表已支持持久化管理 API。

journal 目录使用独立绝对路径，按 Worker identity 绑定。目录及 artifacts 子目录必须属于当前用户且仅当前用户可访问，文件必须为私有普通文件、单硬链接。进程独占锁覆盖 journal 句柄的整个有效期；相同目录不能同时被另一个进程或句柄打开。符号链接、FIFO、异常权限、损坏 JSON、未知字段和超限文件都会拒绝加载。

每次变更先复制并验证完整状态，再写入权限为 `600` 的临时文件，执行文件 sync、原子 rename 和父目录 sync，之后才返回成功。写入出错会禁用该句柄的读取与后续操作；即使 rename 实际已经完成、仅应答丢失，也必须重新打开 journal 核对状态，不能使用旧内存状态继续执行。独立 owner 标记用于识别 journal 丢失，禁止将已有目录自动重置为 generation 1。若整个状态目录被删除，后续启动集成仍需核对原生资源，不能仅凭新目录为空判断不存在旧实例。

操作记录保留账号作用域 session、generation、操作种类和阶段、固定镜像与资源规格、原生 Box ID、归档身份与校验确认。环境变量和启动命令属于私有规格，不实现 `Debug` 输出。传输授权和签名 URL 没有持久化字段，不能作为额外 JSON 字段写入记录。

主要约束：

- 同一操作的重试读取原阶段，不能重新选取归档、规格或原生实例。
- 初始探测固定已有 Box ID；同名替代实例不能被当作原实例采纳。
- source 提交需要归档已校验且远端完整回读已确认；恢复点提交后才能进入源清理完成阶段。
- target 导入需要本地归档已校验，ready/active 必须具有原生 Box ID 与绝对 lease；lease 只能延长。
- 同一 session 的并发 target 认领只能成功一个；取消、终态和 generation 检查限制旧请求的重放。
- 删除记录永久阻止同一稳定 session 的恢复与重建，更高 generation 的未知取消也不能取代删除栅栏。
- 归档文件名由 session、操作与 generation 的摘要生成，不将这些值直接拼接为本地路径。

`cargo test --locked --lib` 覆盖状态机、不可变身份、损坏与私有文件检查、并发认领、真实跨进程锁、子进程被强制结束后的记录恢复，以及写入失败和丢失提交应答的处理。该测试不创建 VM，也不验证对象存储或 Console 协议。
