# E2B Bridge 文件系统持久化补充调研

## 验证状态

2026-10-16 检查了 `worker-bridge-e2b` 源码和 E2B 官方文档。当前进程环境、Worker 本地配置与开发配置中未找到 E2B API Key 和终端模板配置，因此**没有执行真实 E2B 创建、快照或跨后端迁移**。下面区分平台文档能力、当前 Bridge 实现和待验证的迁移方案；Docker / BoxLite 的已有实测结果不适用于 E2B。

## 当前 Bridge 实现

- [`e2bBackend`](../../../worker/worker-bridge-e2b/internal/runner/e2b_backend.go) 支持创建、TTL 更新、销毁、命令执行和文件读取；恢复接口提供 List / Connect，没有 Pause 或 Snapshot 方法。
- [`CreateWithMetadata`](../../../worker/worker-bridge-e2b/internal/e2b/client.go) 明确发送 `autoPause=false`。会话租约到期后的清理由 Kill 完成。
- [`Recover`](../../../worker/worker-bridge-e2b/internal/runner/session_recovery.go) 按 metadata 查找远端沙箱并 Connect。它恢复的是仍可连接的远端对象，不是从持久化归档重建已销毁的沙箱。
- [`terminalResource export`](../../../worker/worker-bridge-e2b/internal/runner/terminal_resource.go) 上传指定文件的字节到预签名 URL。可以用于传输事先生成的归档，但它本身不生成文件系统快照、删除清单或元信息 manifest。

因此，当前 Bridge 的文件系统保留边界是远端沙箱的生命周期，不能把 Worker 重连恢复等同于销毁后恢复。正常退出保留未过期沙箱的行为也不能替代长期持久化。

## E2B 平台能力

E2B 官方提供 Pause / Connect 恢复同一个沙箱，以及 Snapshot / Create 从检查点创建新沙箱。默认快照包括磁盘与内存；文件系统快照使用 `mode="filesystem"`，恢复时从磁盘重新启动，不恢复进程。相关 SDK 的 `mode` 参数从 2.55.0 提供，文件系统快照还可能受项目开关限制；未开启时返回 `snapshot_filesystem_only_disabled`。这些是平台文档能力，当前 Bridge 尚未接入。[文件系统快照](https://docs.e2b.dev/sandbox/filesystem-only-snapshots)、[快照](https://docs.e2b.dev/sandbox/snapshots)、[持久化](https://docs.e2b.dev/sandbox/persistence)

查阅公开文档的快照和模板 API，未发现把沙箱原生快照下载为 OCI layer / Docker image 或直接导入 BoxLite QCOW2 的通用接口。因此不能把 E2B snapshot ID 当作可跨 runtime 交换的文件系统归档；自托管环境可能获得底层存储访问，但不属于当前 Bridge 的控制 API 能力范围。

## 迁移路径判断

| 链路 | 可评估的路径 | 当前状态 |
| --- | --- | --- |
| E2B → E2B，同一沙箱 | Pause → Connect；只需文件时选择 filesystem 模式 | 官方支持，Bridge 未接入暂停；未实测 |
| E2B → E2B，新沙箱 | Snapshot → 从 snapshot ID 创建新沙箱 | 官方支持，Bridge 未接入快照；未实测 |
| E2B → Docker / BoxLite | 沙箱内归档 → 下载 → 与基线比较，转换为 OCI changeset → 目标导入与元信息校验 | 方案可研究，未实测，不能承诺无损 |
| Docker / BoxLite → E2B | 基础/派生 OCI 镜像构建 E2B 模板，或在匹配模板中显式应用文件变更与删除清单 | 模板基础镜像入口有官方文档；迁移语义未实测 |

E2B 的 `Template.from_image(...)` 可从注册表镜像构建模板，但这不是直接运行本机 Docker image ID 或导入本机 `docker save` 文件。镜像还需满足 E2B 的发行版识别及包要求。[基础镜像](https://docs.e2b.dev/template/base-image)

跨后端导出应优先取沙箱内可见的文件系统视图，再构造通用 changeset；不要将 E2B 原生磁盘/内存快照格式与 Docker layer 或 BoxLite QCOW2 混用。反向迁移必须显式处理删除，单独解压新增文件无法恢复删除语义。

## 同 image 实验需要补充的约束

1. 记录实际 `uname -m` 与固定平台 OCI manifest digest，不能只比较多架构 tag 或顶层 index digest。本次已有 Docker / BoxLite 实验是 arm64，不能直接认定与 E2B 模板的平台相同。
2. 在同一份基础镜像上创建 E2B 模板，并记录模板 build ID、envd 版本与实际根文件系统基线。模板系统的运行环境准备不属于用户变更，计算 delta 时应使用 E2B 模板启动后的自身基线。
3. 核实执行用户、root/sudo 权限和归档工具。E2B 模板默认用户与工作目录为 `user`、`/home/user`，不同于 Docker 的默认行为。完整 UID/GID、ACL、xattr 恢复需要相应权限。[用户与工作目录](https://docs.e2b.dev/template/user-and-workdir)
4. 记录真实挂载表，排除临时挂载、运行时生成文件和 E2B 管理文件。向运行中的 E2B 根文件系统恢复时，应保护 envd 与启动配置，避免覆盖后切断管理链路。
5. 复用 [probe.py](probe.py) 的内容、删除、类型互换、链接、权限、ACL/xattr 和稀疏文件样本。新增 E2B ↔ E2B、E2B ↔ Docker、E2B ↔ BoxLite 六个方向的独立结果，不能用 SDK 调用成功代替文件系统校验。
6. 暂停/快照期间可能中断命令流，采集前停止应用写入。生产方案还需定义数据库 flush、重新连接、恢复后的服务启动，以及独立快照对象的保留/删除策略。

下一步真实实验需要可用的 E2B 凭据配置路径与目标模板；若要验证严格的同 image 场景，还需能构建该基础镜像对应的 E2B 模板。提供配置前，当前结论仅为代码与公开能力调研。
