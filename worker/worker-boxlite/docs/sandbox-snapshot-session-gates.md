# BoxLite 会话持久化门禁

会话管理器在同一个锁内协调命令认领、generation、lease、预览连接、容量和普通回收。它为挂起与恢复控制器提供操作入口；当前 Worker 仍不声明 `sandbox_persistence` 能力，收到持久化管理帧会返回 `persistence_unavailable`。这些门禁尚不构成 Console 挂起/唤醒协议的完整接入。

## 请求与绑定

Console 到 Worker 的 `terminalExec`、`terminalLeaseRenew` 和 `terminalResource` 私有 payload 携带 `generation`，缺省为普通会话的 `0`。连接接收器另外注入本次 `ConnectAck.session_id`，调用方不能通过 JSON 覆盖它。已管理的会话必须同时匹配 generation 和当前控制连接，旧连接排队中的请求不能在新连接恢复后重新执行。预览 token 的本端解析也接收私有 `generation` 字段；Console 签发与其他后端的同步接入仍需完成，不能据此认定持久化预览链路已通过系统验收。

| 本地状态 | 执行、资源、续期与新预览 |
| --- | --- |
| 普通会话，无 journal 归属 | 使用 generation `0` 的现有路径 |
| 初始 probe | 仅允许已绑定到 probe 中同一 Box ID 的 generation `1` 请求 |
| source 已保留，或 target 尚未激活 | 全部关闭；不得因 `create_if_missing` 创建替代实例 |
| active / 已确认回滚 | 使用固定 Box ID、当前 generation、控制连接与有效 lease |
| finalized / canceled / deleted | 保持关闭；同一稳定 session ID 不退回普通创建路径 |
| journal 不可用或绑定冲突 | 关闭管理入口并保留实例，等待核对 |

恢复目标先在 journal 中固定原生 ID 和绝对 lease，再绑定到管理器。绑定本身不启动 VM、不执行用户命令，也不开放预览。即使 journal 已写入 `active`，运行中的控制器仍需通过本次连接的显式激活入口开放门禁。重启恢复中已有的 active 记录只能在 Console 恢复握手结束后开放。

管理器依赖控制器先完成原生身份、完整恢复规格与环境就绪核对，不能用管理器绑定代替[原生归档验收](sandbox-snapshot-runtime.md)中的校验。

## source 保留与排空

控制器先调用 `hold_snapshot_source`，原子关闭入口并取得当前 Box ID，再持久化 source claim。写入失败也不能重新开放门禁或让普通回收删除源数据。随后使用 `drain_snapshot_source` 等待已经接受的命令、资源访问与预览连接结束，再调用原生停止和导出。

保留中的实例不因普通 lease janitor 或已接受命令的失败而删除。排空期间发生执行故障会阻止继续挂起，原实例保留用于核对或回滚。外层超时结束等待不会解除保留状态。

每个预览连接持有固定 Box ID、generation 对应的取消 token 与独立计数。保留 source 时立即取消已有连接；即使很快回滚并重新激活，旧 token 也不会恢复。SDK tunnel 打开在独立任务中运行：客户端离开时只结束客户端连接，已开始的 SDK 调用继续持有计数，直到打开尝试结束、连接关闭。排空不能把已丢弃的 JoinHandle 当作 SDK I/O 已结束。

预览连接关闭不等于 guest HTTP 服务或后台进程停止。归档前仍须完成原生 VM 停止和宿主进程退出核对。

## lease、容量与清理

配置独立守护器后，受管理会话的执行、显式续期和恢复先记录同一操作、generation、Box ID 的绝对到期意图，再通过固定服务身份核对实际写入者；需要延长时执行 CAS 续租。守护器确认成功后才提高 Worker 内存中的期限。毫秒期限向上取整以覆盖内存中的完整时间精度；确认返回时还核对本地目标没有过期。已有较长期限时不生成新的 revision，但仍查询守护器，较短的续期请求不能绕过核验。

资源访问、新预览和预览保活使用只读 `check_running`，同时检查有效期限与精确内核进程身份，不刷新 lease。守护器不可用、身份不匹配、实际写入者退出或租约撤销时关闭门禁并取消已有预览，保留原实例；不创建替代根。RPC 与内存期限发布在会话锁下串行，独立守护进程的到期执行不依赖这个锁。

`NativeControlEngine` 从原生适配器安装同一守护器客户端，再安装 journal。重开带有受保护 active/ready 操作的 journal、绑定实例和显式激活，都要求相同服务身份及仍有效的写入者；缺失或替换客户端不能退回无守护器路径。journal 中尚未得到守护器确认的较远期限只表示意图，不能据此恢复执行权。

配置守护器后，首次 generation `1` Inspect 将当前会话已有的运行实例纳入持久化管理。journal 先固定原 Box ID、精确写入者、守护器身份和初始绝对期限，再调用守护器 adopt；确认前关闭 generation `0` / `1` 的普通入口。采纳未确认时，管理入口只允许 Inspect 重试及更高 generation 的永久删除，不能提前创建快照或回滚意图。重试使用首次记录的期限，不能通过重试延长授权、更换进程或重新启动实例。

管理器重建后，Inspect 只附着已记录且仍运行的原 CID。确认过的记录使用守护器当前有效期限恢复本地状态，未确认的记录保留首次期限等待核对；缺失实例不会创建空白替代根。采纳中的根及已受守护器保护的根保留磁盘和容量：普通 janitor 或命令失败不能删除它们，停止写入与释放 owner 分别由独立租约和显式清理协议负责。

配置守护器后，首次创建在 SDK start 前固定 `InitialBoot` 并取得独立启动授权，SDK start 完成且 `bind_boot` 确认精确写入者后才发布会话。Console 尚未接管的 generation `0` 会话使用该 owner 的 generation `1` 内部租约完成执行、续租和重连，首次 Inspect 再持久化切换公开代际门禁。管理器重建只附着记录中的运行 CID，并沿用原授权；未确认启动保留容量，不回退到普通创建。未配置守护器时保留原有路径。生产启动通过显式配置固定全局 store/home 配对并注册能力；回收凭据维护及未知结果协调仍需完成，详见[部署与验收边界](sandbox-snapshot-deployment.md)。

未绑定本地 session 的 source/target 操作也占用容量。target 从虚拟预留转为 ready 绑定时只计一次，普通清理进行中的容量也不会与同一操作重复相加。只有 journal 中已完成清理的终态才能通过 `detach_snapshot_instance` 释放保留的容量；该方法本身不删除原生实例。控制器必须先完成物理清理及其核对，再写入终态。

ready 或回滚后的绑定会重新查询固定 Box ID，要求后端已运行，并刷新 Worker 缓存中的 SDK 句柄。BoxLite `stop()` 会使旧句柄失效，即使随后恢复同一 ID，也必须刷新才能继续执行。绑定拒绝缺失实例或返回其他 ID 的后端，不按名称恢复，不启动或新建实例；绑定成功仍不开放门禁。

永久删除记录不能被更高 generation 的恢复预留替换。Console 重连时重复的终态恢复提示也不能重新占用容量。原生适配器已提供 source 最终回收、未激活 target 取消，以及永久删除对已登记历史名称、实例与本地归档的清扫，并保留名称以拒绝迟到创建/导入。控制器必须先完成实际清理核对，再释放容量或返回 Console ACK。 配置守护器时，还需先持久化并确认写入者退出凭据、完成物理清理，再确认独立 owner 释放；原生适配器已执行该顺序。

## 启动与重连

持久化 journal 必须在普通恢复之前安装，Console 的 source/target 提示也必须在普通 orphan 清理之前处理。提示与 journal 会保护普通稳定名称、恢复操作派生名称和该 scope 的导入历史。已被明确激活或终结的同一操作不会被旧提示改回未完成状态。

active 恢复仅查询 journal/probe 中固定的原生 ID，不按稳定名称寻找替代实例。SDK 实例必须已经运行；普通恢复不会启动停止中的受管理实例，也不会为缺失实例创建空白根文件系统。停止实例的再次启动需要控制器在门禁关闭时根据 lease 与 Console 所有权明确执行。

如果未安装 journal 却收到非零 generation 或未完成操作提示，连接会在调用 SDK 恢复与 orphan 清理之前失败。Worker 尚需在 SDK 初始化之前核对宿主身份、PID namespace、home/journal 归属及本地原生资源清单，不能仅依赖会话管理器完成这些检查。

## 验证

```bash
cargo test --locked
cargo run --locked --example boxlite_session_gates_smoke
```

macOS 管理器测试另使用真实 Unix 通道、独立到期线程和本次测试拥有的内核进程，核对普通执行/续期的确认顺序、较短续期的只读核验，以及守护器丢失时执行、资源、预览、恢复和激活均不开放、不延长内存期限或创建根。执行后端在这些测试中为可控实现；真实 VM 的恢复、续租和资源访问见[管理帧验收](sandbox-snapshot-control.md)。

管理器测试覆盖并发排空、预览打开任务的延迟完成、执行失败时保留源数据、ready/active 边界、过期连接与 generation、lease 写入失败、固定 ID 恢复、永久删除及容量转换。

原生 smoke 在本次调用拥有的私有 SDK home 中使用实际 Worker 管理器与后端：创建文件 → 保留并停止 source → 在同一 ID 回滚、保持门禁关闭再明确激活 → 再次保留并停止 source → 导出、HTTP 分片上传和完整回读 → 删除 source → 下载、导入与就绪探测 → 保持 ready 入口关闭 → 模拟本地协调方激活 → 通过原管理器读取恢复文件。它还验证受控启动的 lease、就绪探测与应答重放，并重建管理器，核对只恢复同一运行 ID，以及受管理实例停止时不会被重复启动请求或普通恢复启动。

示例使用临时 HTTP 对象服务并由测试协调方写入本地提交/激活记录。它不验证真实 S3 签名或条件写入、Console 数据库与路由事务、Worker 跨进程崩溃、跨机器调度或生产性能。完整管理帧编排、启动归属校验、独立 lease 强制执行和原生清理链路仍需完成。
