# BoxLite 文件系统持久化部署

Worker 通过 `persistence.enabled` 显式开启文件系统快照。在取得专用 SDK home、journal 和独立租约守护器的绑定后，才连接 Console 并声明 `boxlite / 0.10.7 / boxlite-archive-v1`、guest platform、管理动作并发和可用暂存字节数。心跳持续报告管理动作占用与暂存空间，取 SDK home 和归档文件系统的剩余空间及配置额度的较小值，并扣除在途和持久化保留预算。默认关闭，未启用或启动校验失败的节点不能接收生命周期动作；持久化未开启时若指定了已受保护的 SDK home，会在 Connect 前报错，不能降级注册。

这项能力保存 guest 文件系统，不保存内存和进程状态。恢复使用相同 runtime、版本、guest platform 和归档格式；没有兼容 Worker 时由 Console 返回现有的无可用 Worker/容量错误。共享对象存储及 Console API 的要求见[跨工程存储与 API 设计](../../../docs/plan/sandbox-session-persistence-storage-api.zh-CN.md)。Worker 使用 Console 下发的短期对象授权，不配置长期对象存储密钥。

## 目录与配置

以下配置既可写入 TOML 的 `[persistence]`，也可使用 `WORKER_PERSISTENCE_` 前缀的环境变量。例如 `lease_store_id` 对应 `WORKER_PERSISTENCE_LEASE_STORE_ID`。专用 terminal home 仍使用顶层 `boxlite_home` / `WORKER_BOXLITE_HOME`。

| 配置 | 默认值 | 含义与约束 |
| --- | --- | --- |
| `enabled` | `false` | 显式开启；拼写错误直接导致启动失败 |
| `journal_dir` | 必填 | 本机生命周期 journal、归档和本地缓存目录 |
| `lease_state_dir` | 必填 | 独立守护器已经建立的账本目录 |
| `lease_socket_dir` | 必填 | 守护器私有 Unix 控制目录；使用短路径以满足 Unix socket 长度限制 |
| `lease_store_id` | 必填 | 已建立账本的非零、规范 UUID；不能在每次启动时重新发现替代值 |
| `python_home` | 必填 | 无状态 pythonExec 的独立 SDK home，不混入持久化实例清单 |
| `max_inflight` | `2` | 生命周期动作并发，范围 `1..64`；相同 scope 始终串行 |
| `tmp_quota_bytes` | `64 GiB` | 暂存预留上限，范围 `1..2^60` 字节；同时受真实磁盘剩余空间限制 |
| `cache_max_bytes` | `4 GiB` | 可选不可变归档缓存额度，范围 `0..tmp_quota_bytes`；`0` 关闭缓存 |
| `cache_ttl_sec` | `3600` | 缓存有效期，范围 `1..2592000` 秒 |
| `drain_timeout_sec` | `30` | 等待原会话执行和预览排空的时间，范围 `1..600` 秒 |
| `readiness_timeout_sec` | `30` | 恢复后检查 guest 契约的时间，范围 `1..600` 秒 |
| `transfer_concurrency` | `4` | 归档传输并发，范围 `1..16` |
| `transfer_idle_sec` | `30` | 传输空闲超时，范围 `1..600` 秒 |

数值解析错误及越界值导致启动失败，不自动改成另一个额度。五个目录（terminal home、journal、账本、控制目录、python home）必须是绝对、互不嵌套的独立目录；校验解析已有父目录别名，防止不同配置字符串指向同一目录。目录属于运行用户且权限为 `700`。Worker 不修复已有异常权限、链接、丢失身份或未登记的原生数据。

SDK home 和 journal 建议使用本机 SSD。暂存预留按最大归档预算的两倍，加展开预算，再增加 20% 余量计算；展开预算还必须覆盖 qcow 虚拟容量，不能只按 guest 当前已写入文件大小配置。例如 `512 MiB` 归档、`48 GiB` 展开预算约需要 `58.8 GiB` 的预留，能放入默认 `64 GiB`；增大并发前应相应增加本地空间和额度。本地缓存只减少共享归档下载，不降低共享提交或完整校验要求。

```toml
boxlite_home = "/var/lib/onlyboxes/terminal-home"

[persistence]
enabled = true
journal_dir = "/var/lib/onlyboxes/journal"
lease_state_dir = "/var/lib/onlyboxes/execution-leases"
lease_socket_dir = "/run/onlyboxes-lease"
lease_store_id = "填写首次建立账本后固定的 UUID"
python_home = "/var/lib/onlyboxes/python-home"
```

## 独立守护器

`boxlite-lease-guard` 与 Worker 使用相同宿主、用户和 PID namespace，由独立服务监督；Worker 不创建或终止这个服务。不能把它放在会随 Worker SIGKILL 一同退出的进程组或监督单元中，否则会失去 Worker 故障后的独立到期执行。

首次建立时，`--state-dir` 必须不存在旧 owner；由守护器建立私有账本。取得 `owner.json` 的 `store_id` 后固定在守护器监督配置和 Worker 配置中。后续守护器启动必须提供相同 `--store-id`，不能删除 owner 或改成无身份打开。下面是生产服务监督器所需的命令参数；本地开发服务仍由仓库 `scripts/dev.sh` 编排。

```bash
boxlite-lease-guard \
  --state-dir /var/lib/onlyboxes/execution-leases \
  --socket-dir /run/onlyboxes-lease \
  --worker-id '<Console 创建的 Worker ID>'

# 首次建立后读取并保留标识，不修改 owner.json。
jq -r .store_id /var/lib/onlyboxes/execution-leases/owner.json

# 后续由服务监督器使用已固定的标识启动。
boxlite-lease-guard \
  --state-dir /var/lib/onlyboxes/execution-leases \
  --socket-dir /run/onlyboxes-lease \
  --worker-id '<同一 Worker ID>' \
  --store-id '<已固定的 store_id>'
```

Worker 在 journal 的 `execution-store.json` 固定 home/journal 配对、store ID 和账本目录的设备号/inode。已有会话历史时缺失该文件会拒绝启动，不通过重新配对重置所有权。守护器 RPC 同时固定进程 epoch；服务重启不会使旧会话自动获得新授权。

## 启动与故障

启动顺序是：严格配置和目录隔离校验 → 只读核对已有账本与私有控制端点 → 取得 SDK home/journal Guard 并固定 store 绑定 → SDK 初始化前清单与已授权残留恢复 → 注册原生动作引擎和会话门禁 → Connect Hello。无状态 pythonExec 的临时实例只进入独立 home。

运行中每秒检查一次守护器和 store/home 绑定。失败时调度器拒绝新动作、取消新副作用授权，会话入口关闭，`run` 返回错误；Worker 先对 journal 中已固定的 writer 发出精确内核停止请求；主程序随后等待已接受动作，关闭会话管理器，并再次扫描未确认启动的可执行文件证据以覆盖迟到启动，再关闭 SDK、保留持久根。停止不依赖 SDK 的活跃句柄缓存，信号投递也不代替退出或清理确认。正常命令也独立核对精确 writer 和有效执行租约。退出或超时不代表旧 writer 已停止；Console 必须等待明确的退出和清理凭据，才能转移可写 owner。

守护器重启会撤销旧租约。未知启动结果、宿主 boot/PID namespace 变化及不一致历史保留并拒绝开放，不通过创建空白根恢复服务。这些异常的自动协调、账本维护/压缩和完整跨机器验收仍待完成；不能把此配置入口视为已经覆盖所有故障恢复。

## 验证范围

`cargo test --locked` 覆盖严格配置、目录别名/嵌套拒绝、运行中账本的只读核对、跨 store 重新绑定和丢失绑定拒绝，以及原有控制、代际、租约、传输和清理约束。

`cargo run --locked --example boxlite_worker_persistence_smoke` 使用真实 `runner::run`、本机 gRPC 注册服务夹具、实际守护器和 BoxLite VM，验证启动后 Hello/心跳、初始创建、独立 pythonExec home、控制帧快照与回滚、generation 1 执行，以及守护器故障后的关闭和磁盘保留。先执行 `cargo build --locked --bin boxlite-lease-guard`。注册服务为夹具，不能替代真实 Console/S3 事务、Linux VM 或两台物理 Worker 的验收。
