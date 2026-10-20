# 生命周期管理帧调度

BoxLite Worker 在普通命令与心跳所在的 gRPC `Connect` 流中接收 `SandboxPersistenceControl`，通过 `ControlDispatcher` 为生命周期动作保留独立任务。它不使用普通 `terminalExec` 的结果缓存或并发槽。

当前阶段已接入控制帧分派、连接恢复屏障、后台任务所有权与报告发送。原生动作引擎尚未在生产启动路径中注册，Worker 不声明 `sandbox_persistence`；未注册时所有管理帧继续返回 `persistence_unavailable`。gRPC 传输测试使用可控引擎；下述原生验收使用真实 BoxLite VM 和临时 HTTP 对象服务，但不代表真实 Console/S3 已经贯通。

## 准入与私有数据

每次交付需要合法的 request ID、operation ID、账号作用域 session、正 generation、已知动作和未过期 deadline。私有 JSON 不超过 64 KiB，拒绝未知字段、重复字段、尾随 JSON 和错误字段类型。动作引擎仍需根据 action、journal、规格与归档核对各字段的业务约束。

同一 scope 同时只运行一个动作，并受 Worker 的动作总并发上限约束。重复交付、正在恢复连接或容量已满时，返回可重试的 `busy` 状态，不启动另一份原生工作。匹配操作/代次的 cancel 或更新代次的永久删除请求可以取消正在执行的动作，但在途任务结束前仍返回 busy。

签名 URL、请求头和恢复规格可能包含私有数据。控制载荷及执行上下文不实现 Debug，不写入 durable state，不在错误消息中回显。报告包含交付身份、操作身份、generation、action 和完成结果；错误使用固定类别及通用消息。原生引擎只把允许持久化的操作身份、规格、归档与原生资源证据写入 journal。

## 已接受任务的所有权

提交动作只向接收方暴露报告 receiver，不暴露可取消后台任务的 JoinHandle。接收方离开或流断开，不会释放 scope 预留或中止已进入 SDK 的操作。deadline 到期会取消上下文；引擎应在每个新副作用、网络请求或入口开放之前检查授权，并继续等待已经接受的 SDK 操作完成。

接收流使用独立报告发送任务，因此长动作不阻塞心跳或后续控制帧。每条连接最多保留 64 个报告发送任务，在接受动作前先预留报告容量；超限时中断连接并进入核对，不无限积压任务。出站通道发生背压时，心跳发送仍响应关闭和会话错误，确保能够取消连接上下文。动作完成前，其 scope 和并发槽保持占用；SDK 操作的未知结果不能因为外层等待结束而变成“未启动”。引擎发生 panic 时，调度器进入不可恢复的失败状态，并要求引擎关闭本地执行入口、保留资源，拒绝在同一进程中重新开放管理动作。

调度任务结束不代替原生/传输层对内部阻塞任务、文件 pin 和数据完整性的核对。原生引擎必须持续持有这些所有权，删除前等待实际 I/O 完成并释放读句柄。

## 连接与退出

引擎和调度器应在发起 Connect 之前注册。连接 ACK 确定新连接身份后，调度器先取消旧上下文并等待旧任务实际结束，再执行会话恢复。旧任务未结束时，连接恢复保持关闭；连接等待超时也不丢弃旧任务。

Console 的恢复 ACK 到达、会话恢复完成后，才开放新连接的动作准入。旧连接的控制帧被拒绝，旧连接随后触发的 disconnect 不会关闭新连接。每个 receiver 使用握手时固定的调度器引用；中途注册或更换对象不能绕过恢复屏障。

退出先取消并等待生命周期任务，再关闭会话管理器和 SDK。这个进程内屏障不能替代 Worker 被 SIGKILL 后的独立 lease 强制执行；独立约束、SDK 初始化前的 home/journal 归属核对、生产配置接入仍需完成。

## 验证

`cargo test --locked` 覆盖报告接收方丢弃、期限取消、匹配/不匹配取消、更高代次删除、并发槽保留、重连等待、旧连接拒绝、关闭等待、异常隔离和严格私有 JSON 解码。实际 tonic 流测试同时发送管理帧和心跳，验证长动作期间心跳持续、报告身份完整，以及未注册引擎的明确拒绝。测试不启动 BoxLite VM。

## 源实例动作引擎

`NativeControlEngine` 实现 source 的 inspect、prepare_snapshot、upload_snapshot、verify_snapshot、finalize_suspend 和 cancel/rollback。它在控制连接、scope、操作和 generation 核对之后，调用同一 SDK runtime 上的原生适配器，保持所选 Box ID 不变。

准备阶段先计算两个归档文件、展开预算与额外余量，串行核对全局暂存额度、未完成操作数、已保留缓存，以及 SDK home 和 artifacts 的可用空间，再关闭源入口。关闭后先写入 durable source 意图，再排空已接受的执行与预览。排空超时或出现执行故障时保留源与门禁，不继续导出。SDK 原生操作一旦开始就等待实际完成，外层取消不丢弃结果或开启新实例。

分片上传使用完整校验后固定的归档句柄，可在同一操作的多个授权批次之间复用。只有完整 GET 回读通过才记录 remote_verified；本地归档和上传 ETag 都不代替该确认。finalize_suspend 必须同时取得 Console 的共享提交授权与本地回读确认，才进入 committed、释放传输 pin、等待在途文件 I/O、清理原实例并保留原生名称占位。清理结束后才释放会话容量；已提交归档留作缓存，不作为跳过共享提交的依据。

回滚由 Console 明确授权。已有操作必须尚未共享提交；原生回滚将 lease 写入 journal 后才启动原实例，并保持用户入口关闭，直至同一控制连接明确完成绑定和激活。已接受执行故障会阻止快照导出；这些请求的计数与预览归零、原 VM 就绪后，可以恢复同一 root 的访问。缺少 source claim 的回滚只能使用已记录 probe 或旧 active 操作固定的 CID 和匹配规格重建 source 意图，不写入一个会阻断后续采纳的通用 cancel tombstone。管理器重建后没有本地请求计数时，仍根据已持久化的源身份完成核对，不能创建空白替代根。

暂存额度和空闲空间查询属于准入估算，不是操作系统配额。SDK 临时文件、未登记孤儿和长期缓存的回收、硬磁盘配额与生产容量验收仍需完成。同节点恢复目前仍从共享对象服务获取目标归档；已提交源归档跨操作复用的本地缓存优化尚未接入。

```bash
cargo run --locked --example boxlite_lifecycle_controls_smoke
```

该验收在临时私有 home 中运行真实会话管理器、原生动作引擎与 BoxLite VM，直接交付 protobuf 管理帧。覆盖预算拒绝后源仍可执行、缺少 claim 的源回滚、排空期间执行超时后的数据保留、冻结后重建管理器/引擎并重试、原 CID 回滚与重放、分片 HTTP 上传、完整回读、未验证/未提交时禁止清理，以及清理后容量归零与入口关闭。恢复端覆盖准备后取消、拒绝延迟准备、HTTP 下载及导入、ready 后重建管理器、固定 CID/进程/绝对 lease 的重试、显式激活、旧 generation 拒绝、普通取消不能删除 active 目标、显式撤销，以及运行中永久删除和禁止重建。测试协调方负责完成分片和授予提交标志；不模拟真实 S3 条件 manifest 或 Console 数据库事务。管理器重建不等同于 Worker SIGKILL、跨机器或独立 lease 验收。

## 目标恢复、撤销与永久删除

同一个 `NativeControlEngine` 处理 prepare_restore、restore、activate 以及 target cancel，和源动作共享准入锁、操作额度、暂存统计及 I/O 槽。prepare_restore 核对精确 runtime 版本、guest 平台、固定镜像与归档身份；本地计算下载归档、规范化归档、SDK 导入工作区和展开磁盘的预算，加上余量。Console 的 staging_bytes 是调度估算，不会降低本地预算。会话容量与 durable target 意图先于下载和 SDK 导入保留，普通命令不能在其间创建空白根。

restore 通过完整 GET 校验归档，再核对格式、scope、规格、展开预算和独立磁盘身份。导入只选择当前操作/generation 派生名称对应的实例，不替换已记录 CID。绝对 lease 和启动意图先写 journal，再启动并探测 Python/sh 就绪。已接受的 import/start 始终等待 SDK 返回；连接断开后不开放入口。重试已启动目标使用原来的绝对 lease，不因重放而续期或重新启动曾经 ready 的停止实例。

ready 仅表示实例已经就绪，执行与预览入口仍关闭。activate 核对实际运行进程与未过期 lease，记录 active 后在当前已恢复连接上显式绑定和开放。重复 activate 保留健康的在途工作。普通 cancel 只清理未激活目标；Console 的 delete=true 撤销授权允许先关闭 active 目标入口，排空已接受工作，再停止和移除该实例。撤销保留 session 的可恢复性，与永久删除不同。

purge_session 使用更高 generation 的 Delete 意图永久封锁 scope。该意图与门禁更新在同一个会话锁下完成，随后排空执行/预览、释放该 scope 的传输 pin 并等待实际 I/O，再核对历史 writer、清理所有已记录原生根和归档。只有原生进程退出、物理残留与 artifacts 清理通过后才返回 canceled 并释放容量。删除记录和原生名称占位持续保留；同一 scope 的后续恢复或创建被拒绝。未知 target cancel 不能把仍有普通源实例或未清理 probe 的 scope 改成已取消状态。
