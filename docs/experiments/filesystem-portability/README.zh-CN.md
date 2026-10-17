# Docker / BoxLite 文件系统持久化与迁移实验

## 结论

同一份 OCI 镜像可以作为 Docker 和 BoxLite 的共同起点，但两者的原生可写层不是共同的数据格式。**四条链路都能恢复本次测试的文件内容与增删改语义；跨后端需要转换，当前版本不能直接承诺所有文件元信息无损。**

| 链路 | 本机实际恢复路径 | 已验证结果 | 已观察到的限制 |
| --- | --- | --- | --- |
| Docker → Docker | `commit` → 新容器 | 内容、删除、目录替换、硬链接、软链接目标、FIFO、UID/GID、权限、mtime 一致 | 测试的 ACL、user xattr 丢失；稀疏文件占用增加 |
| BoxLite → Docker | 根文件系统归档 → OCI 增量层 → 原镜像 + 增量层 → 新容器 | 上述数据及测试的 ACL、user xattr 一致 | 稀疏文件占用增加；不包含 BoxLite 的 tmpfs 数据 |
| BoxLite → BoxLite | 停止 → 原生 `export` → 另一 runtime home 中 `import_box` → 启动 | 已测根文件系统内容和元信息全部一致，稀疏占用也一致 | `/tmp` 清空；`RootfsPath` 创建的 Box 仍依赖原镜像目录 |
| Docker → BoxLite | `commit` → `save` → OCI layout → `BoxOptions(rootfs_path=...)` | 内容、删除、目录替换、链接关系、FIFO、UID/GID、mtime 一致 | ACL/xattr 在 Docker commit 阶段已丢失；BoxLite 导入额外丢失测试文件的 setgid 位，软链接 mode 改变；`/tmp` 清空 |

这里的“一致”仅指 [probe.py](probe.py) 覆盖的样本。结果不能外推到任意文件系统特性、CPU 架构、Docker 存储驱动或 BoxLite 版本。

E2B Bridge 的平台能力、当前实现边界及待验证链路见 [E2B 补充调研](E2B.zh-CN.md)。该部分未进行真实云端实验。

持久化唤醒的调度验收边界，以及 BoxLite 0.10.6 的复测结果，见 [持久化就绪评估](PERSISTENCE_READINESS.zh-CN.md)。

## 环境与共同镜像

- 日期：2026-10-16。
- 宿主机：macOS / Darwin 27.0.0，Apple Silicon / arm64。
- Docker：OrbStack 中的 Linux/arm64 Engine 29.4.0，containerd 2.2.2，`overlayfs` snapshotter，containerd image store。
- BoxLite：Python SDK 0.10.0，通过其原生 runtime 执行；项目 Rust Worker 同样依赖 BoxLite 0.10.0。
- Python：3.12.15。
- 父镜像：`python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258`。
- 在父镜像上添加测试程序与基础文件，构建一次。Docker 直接运行该镜像；BoxLite 使用该镜像 `docker save` 后的相同 linux/arm64 OCI manifest 和 layers。
- 本次共同平台 manifest：`sha256:d1b24d3bfe667c2d2e0226be52a51919990335781d72dc0acb8edecec0639c2b`。重新构建时构建元数据可能改变 digest，但每次实验的两个源 runtime 仍共享同一份镜像。

实验直接调用 Docker CLI 与 BoxLite SDK，不启动 Console 或 Worker，不涉及会话租约、调度、重连或清理策略。

## 样本与判定

基础镜像预置文件后，在两个源 runtime 执行相同变更：

- 新增二进制内容、覆盖原文件、删除原文件、重命名。
- 删除并重建目录，检查旧内容不会复活；文件与目录互换。
- 创建硬链接、软链接、FIFO、8 MiB + 3 字节的稀疏文件。
- 设置 UID 1234、GID 2345、mode `06751`、固定 mtime。
- 设置 `user.onlyboxes` xattr 和含命名用户的 POSIX ACL。
- 同时写入 `/persist`、`/etc`、`/root`、`/usr/local/share`，检查持久化不限于工作目录。
- 在 `/tmp` 写入额外标记，单独检查临时挂载的行为。

校验比较 SHA-256、大小、类型、UID/GID、mode、纳秒 mtime、xattr 原始字节、软链接目标、硬链接 inode 分组、删除结果和稀疏文件 `st_blocks`。两个源 runtime 的样本校验结果完全一致。

[results.json](results.json) 保存源 manifest、环境、每条链路的差异和失败消息。`equal: false` 表示严格比较存在差异，不能把仅剩 `/tmp` 或磁盘块数差异的结果误读为文件内容恢复失败。

## 原生数据与通用格式

Docker 本机使用 OverlayFS snapshotter；BoxLite 的容器根文件系统是 ext4，其可写状态存储在 QCOW2 COW 磁盘中。本机 BoxLite 的 `/proc/mounts` 显示 `/dev/vda / ext4`。因此不能把 Docker 的 upperdir、BoxLite 的 QCOW2 或它们的导出归档视作同一种“可写层”。

检查 BoxLite 原生磁盘头发现，运行目录中的 `disk.qcow2` 带有指向缓存基础 ext4 镜像的 backing file。原生 `export` 生成的归档包含 `manifest.json` 和 `disk.qcow2`，导出磁盘的 backing file 为空，说明已扁平化。但归档配置仍保存 `RootfsPath`；隐藏原 OCI layout 后，`import_box` 可以创建 Box，随后 `start` 报 `Rootfs path does not exist`。**磁盘独立并不代表整个启动链路独立。**使用注册表镜像的配置则应按源码中的重启路径评估镜像解析/拉取依赖。

两条直接互导均已实际验证失败：

- `docker load` 读取 BoxLite 原生归档：`manifest.json` 对象不符合 Docker 镜像归档格式。
- `Boxlite.import_box` 读取 Docker save 归档：拒绝 OCI 目录成员，不是 BoxLite 原生归档。

跨后端可共同消费的是 OCI 文件系统 changeset：新增/修改用 tar entry，删除用 `.wh.<name>`；真正的 opaque directory 用 `.wh..wh..opq`。[OCI layer 规范](https://github.com/opencontainers/image-spec/blob/v1.1.1/layer.md) 定义了这些语义。本次增量转换使用逐路径 whiteout，目录替换测试不等于对所有 opaque marker 实现的穷尽验证。

脚本比较 BoxLite 同镜像的基线根文件系统和变更后的根文件系统，生成增量 tar，追加到原镜像的 layers 和 config `rootfs.diff_ids`，再生成新的 OCI manifest。它没有把全量 rootfs 归档冒充原生增量层。另行验证的 `tar → docker import` 是扁平化路径，会失去基础镜像层复用及原镜像配置，应单独看待。

## 元信息与挂载边界

### Docker commit

本机 Docker commit 后生成的变更层中，测试文件没有 ACL/xattr 的 PAX 数据，恢复后的两项属性均为空；因此 Docker → BoxLite 的这部分丢失发生在 BoxLite 读取之前。此结论只针对本次 Engine、snapshotter 和样本。

稀疏文件 SHA-256 和大小未变，但 Docker 恢复后的 `st_blocks` 从 8 增加到 16392，约从 4 KiB 变为 8 MiB。OCI 规范也指出稀疏 tar 的实现支持并不一致。不能将逻辑内容保留等同于物理分配布局保留。

### BoxLite OCI 导入

即使增量层带有 xattr，BoxLite 0.10.0 在本机恢复后的测试文件仍丢失 ACL/user xattr；mode 从 `06751` 变为 `04751`。软链接目标保留，但 mode 从 `0777` 变为 `0755`。这些是实际差异，尚未将其根因归结为某一个源码函数；Linux 上的软链接权限位本身不控制目标文件访问。

另一个可复现问题是：保留 GNU tar 的多行 `SCHILY.acl.access` 文本时，BoxLite 导入失败，报 `PAX entry error: malformed pax extension`。移除这个文本 PAX key，保留二进制 `SCHILY.xattr.system.posix_acl_access` 和 user xattr 后，同一层可以被 Docker 和 BoxLite 读取。Docker 恢复了 ACL/xattr，BoxLite 仍有上述元信息差异。该对照定位了本样本的触发字段，不代表已覆盖所有 PAX 编码。

### 元信息重放

对 Docker commit 和 BoxLite OCI 导入分别追加显式的元信息恢复，已验证所有样本的根文件系统内容及元信息一致。步骤包括 chown/chmod、恢复 ACL/xattr、必要时重建软链接、恢复硬链接组和 mtime。

硬链接组必须在文件 copy-up 后重新核对：对 OverlayFS 中的文件逐项 chown，可能使原本共享 inode 的文件分离。脚本在元信息写入后重建硬链接组，避免修复属性时破坏链接关系。重放没有修复 Docker 的稀疏物理分配，也没有恢复 BoxLite tmpfs。

### 挂载与运行状态

BoxLite 的 `/tmp`、`/dev/shm` 和部分 `/etc` 文件是独立挂载。原生导入后 `/tmp` 标记消失，额外的 stop/start 实验也观察到同样行为。全根文件系统导出应按 mount 边界处理临时挂载、bind mount 和 volume；本次 tar 排除了 `/proc`、`/sys`、`/dev`、`/tmp` 等运行目录和运行时生成的主机/DNS文件。[Docker commit](https://docs.docker.com/reference/cli/docker/container/commit/) 与 [Docker export](https://docs.docker.com/reference/cli/docker/container/export/) 同样不包含挂载 volume 的数据。

本次未测试 volume 迁移、内存、进程恢复、打开但已删除的文件、文件锁、socket、设备节点、file capability、SELinux 标签、纳秒小数时间戳、运行中数据库的一致性、跨架构迁移或性能上限。归档采集期间没有应用并发写入；生产实现仍需定义暂停/停机或应用 flush 协议。

## 对 Onlyboxes 的建议

采用两类持久化产物：

1. **同后端恢复**：使用后端原生快照/导出，保存并验证基础磁盘、镜像配置和版本依赖。BoxLite 的本地 `RootfsPath` 不能原样视为跨主机可移植配置。
2. **跨后端恢复**：保存固定平台的基础 OCI manifest digest、OCI 增量 changeset、版本化元信息 manifest 和执行配置；目标 runtime 导入后校验，再按能力重放缺失的元信息。

OCI 层可作为共同交换格式，但当前 macOS BoxLite 导入链路需要补偿 ACL/xattr/setgid 等差异。需要完整文件系统语义时，恢复成功应以校验结果为准；仅支持普通工作目录内容时，可以另设范围更小的文件归档协议。

Volume 应作为独立持久化对象；tmpfs 的保留策略需要明确声明。镜像中的 `Env`、`User`、`WorkingDir`、`Entrypoint`、`Cmd` 也属于恢复配置，不能仅靠文件 tar 推导。

## 复现

本脚本针对本次 macOS/arm64、Docker containerd image store 和 BoxLite 0.10.0 环境。Python 要求 3.12+，用于 tar extraction filter。其他宿主机可复用样本与流程，但需要重新评估预期失败和元信息差异。传统 Docker image store 的 save 格式转换不在脚本范围内。

```bash
uv venv --python 3.12 /tmp/onlyboxes-fs-repro-venv
uv pip install --python /tmp/onlyboxes-fs-repro-venv/bin/python 'boxlite==0.10.0'
/tmp/onlyboxes-fs-repro-venv/bin/python \
  docs/experiments/filesystem-portability/run.py \
  /tmp/onlyboxes-fs-repro
```

输出目录必须尚不存在。脚本构建带随机名称的专用测试镜像，正常退出或异常时停止测试 runtime 并删除自己创建的 Docker 容器、镜像；磁盘、归档、完整校验结果和日志留在指定目录。流程执行完成不表示所有 `equal` 都为 true，应查看 `results.json` 的具体差异。

本次已完整执行仓库中的脚本，原始产物保留在 `/tmp/onlyboxes-fs-portability-verified`，运行日志为 `/tmp/onlyboxes-fs-portability-verified.log`。仓库只保存文本脚本与精简结果。

BoxLite 源码核对使用 `v0.10.0`，commit `006b953a560a8293b7b646288d9e4fd9c1cad00a`：

- [磁盘初始化与重启时读取原镜像配置](https://github.com/boxlite-ai/boxlite/blob/006b953a560a8293b7b646288d9e4fd9c1cad00a/src/boxlite/src/litebox/init/tasks/container_rootfs.rs)
- [原生导出与 clone](https://github.com/boxlite-ai/boxlite/blob/006b953a560a8293b7b646288d9e4fd9c1cad00a/src/boxlite/src/litebox/clone_export.rs)
- [原生归档格式与解析](https://github.com/boxlite-ai/boxlite/blob/006b953a560a8293b7b646288d9e4fd9c1cad00a/src/boxlite/src/litebox/archive.rs)
- [OCI tar、权限与 xattr 处理](https://github.com/boxlite-ai/boxlite/blob/006b953a560a8293b7b646288d9e4fd9c1cad00a/src/boxlite/src/images/archive/extractor.rs)
