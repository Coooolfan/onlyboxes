# 持久化后重新调度唤醒：可行性与验收边界

完整的挂起、快照提交、停止与跨 Worker 唤醒时序见[沙箱会话持久化流程](../../plan/sandbox-session-persistence-lifecycle.zh-CN.md)。

## 当前判断

当前实验支持进入持久化方案设计与原型实现，尚不足以验收“持久化后可重新调度唤醒”。实验验证了后端文件系统转换与恢复，尚未建立共享持久化存储、快照提交协议和调度恢复状态机。

重新调度不要求切换 runtime。第一阶段可将目标节点限制为相同 runtime、兼容版本、相同 OS/CPU 架构且满足资源条件的 Worker。这样既能调度到另一台机器，也可以保留各后端原生恢复路径。Docker ↔ BoxLite 迁移可作为后续能力，不能直接成为任意节点都能接管的默认承诺。

| 需求 | 现有证据 | 判定 |
| --- | --- | --- |
| 恢复代码、依赖与根文件系统数据 | 同镜像四条链路已执行；样本包含工作目录之外的路径 | 可行性已验证，覆盖范围有限 |
| 同 runtime 换节点唤醒 | BoxLite 0.10.6 使用注册表镜像，隐藏源 home，在空目标 home 导入成功 | 已模拟本地缓存隔离，未跨物理机器验证 |
| 换 runtime 唤醒 | OCI 增量路径可读取，但元信息和文件类型存在差异 | 需要转换、manifest 重放及校验；不能默认无损 |
| 原节点消失后仍能唤醒 | 只有临时归档及本机文件，没有对象存储提交/下载链路 | 未验证 |
| 维持原 session ID 并重绑定 Worker | 当前 confirmed route 固定到原 Worker | 尚未实现持久化唤醒调度 |
| 同一 session 并发唤醒只生成一个可写实例 | 现有 provisional route 面向新 session，不是快照恢复所有权协议 | 未验证 |
| 故障、重试、旧节点重新上线 | 未进行持久化/恢复事务及故障注入实验 | 未验证 |

## 当前项目实现的差距

[`service_dispatch.go`](../../../console/internal/grpcserver/service_dispatch.go) 的 `pickSessionForDispatch` 遇到已有 confirmed route 时仍选择原 `NodeID`；原 Worker 不可用会返回 `session_unavailable`。这与[现行恢复语义](../../plan/CONSOLE_SESSION_RECOVERY_PLAN.zh-CN.md)一致：重连接管原资源，不把已有会话重派给另一个 Worker。

[`TerminalSessionRoute`](../../../console/internal/registry/store_terminal_session_routes.go) 目前保存 Worker 归属和绝对 lease 等路由信息，没有持久化文件系统归档引用、快照代次或可恢复规格。lease 到期会清理路由；持久化 session 需要区分“运行实例租约到期”和“持久化数据保留期到期”。

增加持久化时，session 应成为独立于运行实例的稳定对象。挂起时可没有 Worker 绑定，但仍保留账号归属、恢复规格、最新有效快照与数据保留策略；成功唤醒后才绑定新的运行实例。

## BoxLite 0.10.6 复测

2026-10-16 从 GitHub latest release、PyPI 与 crates.io 核对，最新稳定版均为 **0.10.6**，发布时间为 2026-10-12。[官方 Release](https://github.com/boxlite-ai/boxlite/releases/tag/v0.10.6)、[PyPI](https://pypi.org/project/boxlite/0.10.6/)

使用独立 Python 环境复跑现有样本，未升级项目依赖。完整路径均已执行，结果保存于 [results-0.10.6.json](results-0.10.6.json)。

| 项目 | 0.10.0 | 0.10.6 本机实测 |
| --- | --- | --- |
| BoxLite 原生导出/导入 | 已测根文件系统内容与元信息一致 | 相同 |
| `/tmp` | 恢复后清空 | 相同 |
| OCI 导入 ACL/user xattr | 测试属性丢失 | 相同 |
| OCI 导入 setgid | `06751` → `04751` | 相同 |
| OCI 导入软链接 mode | `0777` → `0755`，目标保留 | 相同 |
| 多行 `SCHILY.acl.access` | PAX 解析失败 | 相同 |
| OCI 导入 FIFO | 样本 FIFO 保留 | **样本 FIFO 缺失**，Docker 来源和 BoxLite 增量来源均观察到 |
| 本地 `rootfs_path` 原生归档 | 启动仍要求原镜像目录存在 | 相同 |
| BoxLite → Docker OCI 增量 | 已测属性保留；稀疏文件占用增加 | 相同 |

0.10.6 的新差异使仅重放属性的脚本不足：遇到缺失 FIFO 会失败。补充从 manifest 显式创建 FIFO 后，样本根文件系统校验再次一致，仍不恢复 tmpfs。它说明恢复 manifest 需要记录文件类型，不能只记录 chmod/chown。该补偿只覆盖已测样本，尚未实现通用文件系统恢复器。

### 更接近 Worker 的冷环境恢复

此前 `rootfs_path` 实验的路径依赖不能直接当作生产 Worker 的限制，因为 [`worker-boxlite`](../../../worker/worker-boxlite/src/boxlite_runtime.rs) 使用 `RootfsSpec::Image`。

新增 [native-cold-home.py](native-cold-home.py) 按注册表镜像方式验证：

1. 从固定 digest 的 Python 镜像创建 Box，执行样本写入。
2. 停止并导出原生归档。
3. 关闭源 runtime，将其整个 home 改名，使缓存、基础磁盘和原 Box 路径不可达。
4. 在空目标 home 中导入并启动。
5. 比较全部样本内容、元信息、硬链接组与稀疏占用。

**结果仅有 `/tmp` 标记缺失，已测根文件系统数据全部一致。**它证明此场景不依赖源节点本地缓存路径；原镜像仍通过注册表解析，不能据此承诺离线恢复。同宿主机上的两个 runtime home 也不等于真实跨主机、跨内核或跨架构测试。

复现命令：

```bash
uv venv --python 3.12 /tmp/onlyboxes-fs-0106-venv
uv pip install --python /tmp/onlyboxes-fs-0106-venv/bin/python 'boxlite==0.10.6'
/tmp/onlyboxes-fs-0106-venv/bin/python \
  docs/experiments/filesystem-portability/run.py \
  --boxlite-version 0.10.6 /tmp/onlyboxes-fs-0106-repro
/tmp/onlyboxes-fs-0106-venv/bin/python \
  docs/experiments/filesystem-portability/native-cold-home.py \
  /tmp/onlyboxes-fs-0106-cold-repro
```

本次完整原始结果位于 `/tmp/onlyboxes-fs-portability-0106-verified` 和 `/tmp/onlyboxes-fs-native-cold-0106`。原始日志位于同名 `.log` 文件。两组实验均已停止 runtime，主实验已删除专用 Docker 容器和镜像。

## 第一阶段建议的契约

第一阶段定义为“持久化根文件系统，恢复为新启动的执行实例”。保留普通文件、删除语义、权限、UID/GID、mtime、软链接及硬链接；对 ACL/xattr、特殊文件应明确支持或显式拒绝，不能静默丢失。不承诺进程、内存、打开的文件句柄或网络连接恢复。临时挂载和 volume 应另行定义范围。

唤醒调度先限制在同 runtime 的兼容 Worker。恢复规格至少包含基础镜像的平台 manifest digest、runtime/归档格式版本、资源配置、执行用户、工作目录、环境变量、启动命令和挂载规格。实际执行实例、路由与快照数据分别管理。

原生快照不等于共享持久化。即便磁盘格式可移植，仍要把归档及所需配置提交到独立于 Worker 的存储，并校验完整性和可访问性后才允许回收本地实例。

## 上线前必须补齐的验收

1. **真实冷节点恢复**：从 Worker A 持久化，完整提交到共享存储；使 A 及其本地资源不可达，在无缓存的同平台 Worker B 唤醒，保留原 session ID，验证文件、依赖安装结果与配置。分别覆盖 Docker 和 BoxLite。
2. **快照提交失败**：排空写入、生成、上传、校验、发布引用、回收实例各步骤中断或失败时，始终保留一个有效恢复点；不能先删除唯一副本。明确正在写入的应用和数据库的一致性协议。
3. **并发唤醒与所有权**：多个请求同时唤醒、恢复超时后重试、原 Worker 重新上线时，只允许一个实例获得当前代次的写入与服务权。旧 Worker 的清理或恢复报告不能覆盖新的路由/快照代次。
4. **恢复失败与发布顺序**：容量不足、下载失败、校验失败、启动失败、Console 重启时可重试。目标恢复并通过校验后才能发布新路由；失败不消耗或破坏有效快照。
5. **范围及资源验收**：真实项目、包管理器/依赖、数据库、较大根文件系统、大量小文件、volume、镜像凭据、公开预览重新绑定；测量快照大小、挂起/唤醒时延、临时磁盘峰值及保留期清理。

完成上述同后端验收后，可以支持“唤醒可能重新调度”的第一阶段需求。若调度池还要求任意 Docker / BoxLite 互换接管，则需另加每个方向的跨主机恢复验收与明确的文件系统语义契约，当前结果不能满足该承诺。
