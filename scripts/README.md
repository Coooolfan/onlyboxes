# scripts

## dev.sh — 本地开发进程编排

统一管理 tmux 中的 console / web / website、本地 Worker，以及公共预览 Nginx 容器和 Docker 网络。所有子命令都立即返回。

```bash
scripts/dev.sh start                       # 启动默认服务：console / web / website
scripts/dev.sh start console web           # 只启动控制节点与前端
scripts/dev.sh start worker-docker         # 显式启动 Docker Worker
scripts/dev.sh start public-preview        # 启动 console / worker-docker / nginx
scripts/dev.sh status                      # 会话、端口监听与窗口状态
scripts/dev.sh logs worker-docker          # 最近 200 行 Worker 日志快照
scripts/dev.sh creds                       # console 管理员账号
scripts/dev.sh restart web                 # 重启前端
scripts/dev.sh stop                        # 全部停止并销毁会话
```

### 服务

| 服务 | 目录 | 命令 | 端口 |
| --- | --- | --- | --- |
| console | `console/` | `go run ./cmd/console` | 8089（HTTP）、50051（gRPC） |
| web | `web/` | `yarn dev` | 5178 |
| website | `website/` | `yarn dev` | 5173 |
| worker-docker | `worker/worker-docker/` | 平台相关，见下文 | 8091（启用公开预览时） |
| snapshot-store | Docker | 本机 S3（固定源码构建 MinIO） | 19000 |
| worker-boxlite / worker-boxlite-peer | `worker/worker-boxlite/` | 独立 Worker 与租约守护器 | 无 TCP 监听 |
| nginx | Docker | `nginx:1.26-alpine` | 80 |

前端开发只需 `start console web`：`web` 的 vite 已把 `/api` 与 `/mcp` 代理到 `127.0.0.1:8089`，浏览器开 <http://localhost:5178> 即可。

Worker 不属于无参数 `start` 的默认服务集合，必须显式指定：

```bash
scripts/dev.sh start console worker-docker
```

`dev.sh` 等待 Console 就绪后调用 `POST /api/v1/workers`，将一次性返回的凭据写入 `scripts/.dev/worker-docker.env` 并设置权限为 `600`。重复执行时复用当前 Console 数据库中仍然存在的 Worker；Console 数据库重建后自动创建新 Worker 并覆盖失效的本机凭据。

Linux 上的 `worker-docker` 由 `dev.sh` 原生运行。macOS 上由 `dev.sh` 自动编译 Linux 二进制并通过 OrbStack Linux VM 运行，开发者不需要手动进入 VM；OrbStack 缺失或未启动时命令会明确报错，不会自动安装或启动桌面应用。可通过 `ONLYBOXES_WORKER_DOCKER_RUNNER=orb|native` 覆盖运行方式。

### 管理员账号

console 只在**首次初始化数据库**时生成管理员账号并打印一次。`creds` 会在首次抓到后固化到 `scripts/.dev/console-creds.json`（权限 600），之后可随时查看：

```bash
scripts/dev.sh creds
```

若凭据已丢失，删库重建：

```bash
scripts/dev.sh stop console
rm console/db/onlyboxes-console.db
scripts/dev.sh start console && scripts/dev.sh creds
```

### 配置

tmux server 的环境与调用方 shell 是隔离的，`FOO=bar scripts/dev.sh start` **不会生效**。服务所需配置写入 `scripts/dev.env`（不入库），start 时自动加载：

```bash
cp scripts/dev.env.example scripts/dev.env
```

改动后 `restart` 对应服务生效。

Worker 凭据和运行参数使用独立文件 `scripts/.dev/<worker>.env`，由 `dev.sh` 自动管理，不应手动复制到 `scripts/dev.env`。

## 本地沙箱持久化验收

`sandbox-persistence` 组合服务启动本地 S3、Console 和两个独立 BoxLite Worker。每个 Worker 有独立的 SDK home、journal、租约账本和 `boxlite-lease-guard` 进程。两个 Worker 运行在同一物理机器，用于验证跨 Worker 调度；跨物理机器和 Linux BoxLite VM 仍需单独部署验收。

BoxLite 守护器就绪通过私有 Unix `ping` 核对协议版本、请求 nonce、Worker/store/epoch 身份和实际服务进程；守护器管理进程还要求回执 PID 匹配自己持有、尚未回收的子进程。只有这些核对通过且子进程仍在运行，才固定首次 store ID 并报告就绪。残留 socket、其他进程的回执、旧 nonce、错误身份或损坏响应均不作为就绪证据。Worker 同样等待匹配的实际就绪回执，Rust 启动路径继续独立校验绑定与执行许可。

本机管理进程先安装信号转发，再创建独立会话中的子进程。创建过程中收到停止请求时，已创建的精确子进程会收到停止信号并被等待；请求先于创建到达时不再创建子进程。不会按进程名/PID 查询结果或进程组发送停止信号，也不因等待过久强制丢弃尚未确认的原生结果。守护器在就绪前退出会报告实际 `returncode`；服务管道保留原始失败退出码，不让日志 `tee` 的成功覆盖服务失败。`start` 仍立即返回，只表示启动指令已提交。

依赖本机 Docker、Go、Rust、tmux、Python 3 及 BoxLite 支持的虚拟化环境。默认从固定官方 MinIO 源码 `RELEASE.2025-10-15T17-29-55Z` 编译本地 S3 镜像，首次启动需要下载依赖和构建；也可在配置中指定 `ONLYBOXES_DEV_MINIO_IMAGE`。此对象存储只监听 loopback，供本机验收使用。

```bash
cp scripts/snapshot.env.example scripts/dev.env
chmod 600 scripts/dev.env
# 按模板说明填写四个独立的随机密钥，再启动：
scripts/dev.sh start sandbox-persistence
scripts/dev.sh status
# 服务就绪后运行真实 API 验收：
python3 scripts/tests/snapshot_e2e.py
scripts/dev.sh stop sandbox-persistence
```

验收脚本通过 Console API 创建会话、写入文件，检查跨 Worker 恢复、同 Worker 恢复、无 Worker 时的 `no_worker`（同步拒绝为 503；异步接受后在操作结果中报告）和重新上线后的恢复，最后删除测试会话并等待该独立验收账号的物理清理积压归零。文件验证包含 SHA-256、权限和符号链接。失败时保留会话与私有 `snapshot-e2e-<scenario>-result.json`，不将未知操作视为成功。脚本需要 `dev.sh` 缓存的本机管理员登录凭据，以创建短期测试执行 token。不要在有其他工作负载的 Worker 上运行此验收。

Docker 使用独立组合 `sandbox-persistence-docker`，不复用公共预览的 Worker、容器名或端口。先按同一模板配置 `WORKER_TERMINAL_EXEC_DOCKER_IMAGE` 为固定 digest，再运行：

```bash
scripts/dev.sh start sandbox-persistence-docker
python3 scripts/tests/snapshot_e2e.py --runtime docker
scripts/dev.sh stop sandbox-persistence-docker
```

`worker-docker-snapshot` 和 `worker-docker-snapshot-peer` 分别拥有 Worker 凭据、journal 和生产独立租约守护器。Linux Worker 二进制和静态守护器由 `dev.sh` 自动编译；守护器镜像按二进制 SHA-256 固定标签，避免重复构建替换正在使用的镜像索引。Linux 使用原生 Worker 进程和本机私有 journal；macOS 要求 OrbStack 已安装且已启动，Worker 通过 Docker CLI 镜像运行在当前 Linux daemon 网络中，各自使用独立 journal 卷。可用 `ONLYBOXES_DEV_DOCKER_WORKER_IMAGE` 指定已有 CLI 镜像；运行时固定为其实际 image ID。Console 连接主机默认 macOS 为 `host.docker.internal`、Linux 为 `127.0.0.1`，可用 `ONLYBOXES_DEV_DOCKER_CONSOLE_HOST` 覆盖。不要将该入口指向远程 daemon：本机二进制挂载、同机 Console/S3 网络和 daemon 本机卷是这一开发入口的部署前提。

Docker 验收支持 `cycle`、`same-worker`、`no-worker`、`console-restart`、源导出中的 `worker-restart` 和目标启动后的 `worker-start-restart`。Docker 结果写入私有 `snapshot-e2e-docker-<scenario>-result.json`。两个 runtime 的验收应使用各自独立会话、配置与状态目录；脚本在创建沙盒前拒绝所选两个 Worker 之外仍有在线 Worker 的 Console。

`python3 scripts/tests/snapshot_e2e.py --runtime docker --scenario worker-restart` 要求命中真实导出窗口：journal 中原 source 仍为 `preparing`，原不可变容器已经 paused，且带同一 source/operation 标签的导出 helper 正在运行。随后仅终止 Worker，核对其实际退出回执，确认冻结源和 journal 仍在原操作上、独立 guard 的完整绑定摘要/实例/状态卷身份不变，再重启同一 Worker。原 operation ID 与源容器 ID 必须完成挂起；同节点唤醒后校验文件内容、权限和符号链接，永久删除后等待积压归零，并只读检查原源容器、该操作的归档/导出临时文件和导出 helper 已移除。窗口已经错过会失败，不能把普通完成后的重启记作中断恢复通过。

该场景在 macOS arm64、OrbStack / Docker 29.4.0、真实 Console/MinIO 与两个独立 Worker 上通过，两个 Worker 分别作为源均命中了 SIGKILL 导出窗口；采用含物理清理检查的入口也已通过。Linux 原生 Worker 复用持有直接子进程句柄的故障注入路径，仍需在目标 Linux 部署验证；这些结果不证明跨物理机器或目标导入/启动中断验收通过。

`python3 scripts/tests/snapshot_e2e.py --runtime docker --scenario worker-start-restart` 先停止另一 Worker 并等待离线，确保本次恢复落在原节点。故障工具预先校验 Worker 容器/卷身份与管理端点，收到匹配 Worker、operation 和不可变容器 ID 的 Docker `start` 事件后，通过管理进程取得退出等待确认，再向已固定的 Worker ID 发送 SIGKILL。杀进程后目标必须仍在运行，journal 仍为 `importing`，独立 guard 的原绑定及匹配 scope/generation 的有效租约必须保留。重启后原 operation 和原容器完成恢复；文件校验与永久删除完成后，检查清理积压、目标容器、归档/临时文件和 guard 租约记录均已清理。未命中窗口时保留现场并失败。

Docker 在启动前已登记 guard 租约；此场景验证带有效租约的已启动目标继续完成恢复。BoxLite 的同名场景验证未绑定 writer 的未知启动结果取消及新实例重试，两者的断言分别对应各自启动协议。事件故障注入由验收脚本内部托管，最多等待 120 秒，事件不匹配或未出现时不发送 SIGKILL；脚本退出时终止自己持有的事件观察进程。Worker 的启动、恢复及其他服务编排仍使用 `dev.sh`，普通 `crash` 行为不变。

该 Docker 目标中断场景已在 macOS arm64、OrbStack / Docker 29.4.0 与真实 Console/MinIO 上通过，两个 Worker 分别作为目标均命中已启动且仍为 `importing` 的窗口；包含容器、归档与 guard 租约清理检查的入口也已通过。Linux 原生 Worker 与跨物理机器仍需独立部署验收。

`--return-to-source` 可与任一 runtime 的 `cycle` 组合：源 A 首次快照后离线，B 从共享存储恢复并新增、修改、删除文件，随后再次快照；无 Worker 报错后，先只读核对 A 的旧缓存文件仍存在、完整 SHA-256 匹配首次快照且 snapshot ID 不同于最新快照，再只启动 A，验证 A 使用最新快照恢复，旧缓存不会遮蔽 B 的修改，最后启动 B 并等待清理。例如 `python3 scripts/tests/snapshot_e2e.py --runtime docker --return-to-source`。缓存已淘汰或关闭时会明确失败，不能据此声称覆盖了旧缓存窗口。该结果文件增加 `-return-to-source` 后缀。

停止 Docker 验收 Worker 保留其 journal、缓存卷和生产租约守护器；守护器继续独立执行已登记的到期停止。开发编排不会删除已绑定的 guard 或卷，也不会因其丢失而重新绑定空目录。配置和本机 `docker-volume.json` 保留卷名、创建身份和 daemon 路径；同名卷被替换会拒绝启动。Console 数据库重建后新 Worker 使用新卷，旧卷仍保留。

连续挂起/唤醒可能在旧容量心跳尚未更新时收到可重试 `no_capacity`。普通验收场景最多重试三次，每次先验证失败已解析、会话仍挂起且快照引用未变，再以新幂等键创建新 operation/generation；持续满载仍报错并保留现场。故障注入场景保持原 operation 身份断言。

`python3 scripts/tests/snapshot_e2e.py --runtime docker --scenario console-restart` 在源容器 paused 且 journal 仍为 `preparing`、目标归档已校验且 journal 仍为 `importing`（尚未登记导入 image/container）两个窗口分别重启 Console。停止后的 SQLite 必须保留未完成的原操作；重启后原 operation 继续完成，已有源容器 ID 和独立 guard 绑定不变，恢复操作最终登记目标容器并进入 `active`。同时检查相同幂等键重放、不同参数返回 409、文件内容/权限/符号链接和最终清理积压。目标窗口发生在容器登记之前，不用于证明启动中途未知结果的恢复。

该 Console 重启场景已在 macOS arm64、OrbStack / Docker 29.4.0 与真实 Console/MinIO 上通过，源、目标两次停止后的操作均仍未完成；同节点恢复与删除后清理积压归零。停止前操作已完成的运行会报告窗口未命中并保留会话，不计为验收通过。

可用 `--scenario same-worker` 或 `--scenario no-worker` 分别验证首次同节点恢复和无 Worker 后的恢复；默认 `cycle` 保留完整循环检查。 BoxLite 的 `--scenario console-restart` 在源停止后导出中、目标已登记原生启动意图时分别停止并重新启动 Console，验证原 operation/实例继续完成、重启后幂等重放及参数冲突。此场景只读检查本机 Worker journal 与停止期间的 SQLite；没有实际命中在途窗口会失败，不把完成后的普通重启记作故障恢复通过。

`scripts/dev.sh crash worker-boxlite`（或 `worker-boxlite-peer`）仅向本机管理进程直接托管的 Worker 子进程发送 SIGKILL，并等待该子进程退出回执；命令不接受 PID 或进程组。Worker 的本机管理端点位于私有 Unix 目录，独立租约守护器使用另一端点和进程。正常 `stop` 仍执行 Worker/守护器的依赖停止顺序。

Docker 使用 `scripts/dev.sh crash worker-docker-snapshot` 或 `worker-docker-snapshot-peer`。macOS 路径先核对已固定 journal 卷、Worker 容器的 namespace/service/Worker 标签、入口与挂载归属，再让本机管理进程接管退出等待；收到等待确认后才向该不可变 Worker 容器 ID 发送 KILL。管理进程保持 `docker run` 客户端存活直到容器退出，回执必须为 `137`，不会把杀死客户端当作 Worker 已退出。独立 guard、源沙箱及持久卷不属于该命令的终止目标。Linux 路径直接使用本机管理进程持有的原生 Worker 子进程句柄。只读 journal 证据不输出环境、恢复规格或 guard token，guard 绑定以摘要比对。

`--scenario worker-restart` 已通过源停止后导出中途 SIGKILL 的真实验收：守护器完整身份与 epoch 不变，新 Worker 继续原 operation 和原 Box ID，完成快照、恢复及最终回收。`--scenario worker-start-restart` 已通过目标 shim 已启动、Worker 与守护器尚未绑定 writer 的故障窗口：原恢复失败后确认取消与所有权释放，保留快照，再以新 operation、新 generation 和新实例恢复，最终回收积压归零。该场景要求配套的守护器协议 v2；没有实际命中未绑定窗口会明确失败。

当前 macOS / BoxLite 0.10.7 已通过真实 S3 的完整多轮检查，包括恢复后新增、修改、删除文件再快照，以及无 Worker 后重新上线恢复和最终清理。Worker 对自包含磁盘直接封装，原生 SDK 的再次 flatten 短读边界仍可独立复现；见 [BoxLite 导出策略](../worker/worker-boxlite/docs/sandbox-snapshot-deployment.md#boxlite-0107-的原生导出边界)。跨物理宿主和 Linux VM 需单独验收。

编排回归可运行 `python3 -m unittest discover -s scripts/tests -p '*test.py'`。测试包含真实 Unix socket 的遗留/错误回执与总交互超时、启动期间的信号交付、Ping 期间子进程退出时凭据不变、独立 Worker/守护进程隔离，以及实际 Bash 管道的退出码保留；这些测试不启动沙盒 VM。

单独 `stop worker-boxlite` 或 `stop worker-boxlite-peer` 会依次停止该 Worker 和其守护器，Console 与对象存储继续运行。再次 `start` 复用相同 Worker 身份和持久目录。Console 数据库重建后创建新身份，旧 SDK home 和账本保留，不会交给新 Worker 使用。

S3 AK/SK 自动生成并存于 `snapshot-store.env`（600），只交给 Console；Worker 不继承 Console 管理密钥或 S3 长期凭据。容器名称和数据卷按状态目录隔离，`stop` 保留数据卷与本机凭据。不要单独删除凭据后复用原数据卷。

可用独立会话、配置文件及状态目录隔离现有开发环境；三项应在每次调用时保持一致：

```bash
ONLYBOXES_DEV_SESSION=onlyboxes-snapshot \
ONLYBOXES_DEV_ENV=/absolute/path/snapshot.env \
ONLYBOXES_DEV_STATE_DIR=/absolute/path/private-state \
scripts/dev.sh start sandbox-persistence
```

状态目录须属于当前用户且权限为 700；首次创建会自动设置。验收脚本同样读取这三个变量。服务配置仍从配置文件加载。模板使用 18089 / 15051 / 19000 端口及独立 SQLite 路径；需要时调整以避免冲突。

## 本地公开预览

`public-preview` 是 `console / worker-docker / nginx` 的组合服务：

首次使用时，在 `scripts/dev.env` 中启用 Console 公共预览配置，字段参考 `scripts/dev.env.example`；需要覆盖默认的 Nginx 端口、Docker 网络或 Worker 最大续租时间时，再创建本机配置：

```bash
cp scripts/public-preview.env.example scripts/public-preview.env
```

```bash
scripts/dev.sh start public-preview
scripts/dev.sh status
scripts/dev.sh logs worker-docker
scripts/dev.sh logs nginx
scripts/dev.sh stop public-preview
```

域名、监听端口和 Docker 网络等本机覆盖项写入 `scripts/public-preview.env`。渲染后的 Nginx 配置位于 `scripts/.dev/public-preview-nginx.conf`，两者均不入库。Nginx 与 Console 使用相同的 `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN`。

启动顺序固定为 Console → Worker 凭据检查/创建 → Worker → Nginx。macOS 上 Worker 在 OrbStack Linux VM 中运行；Linux 默认原生运行。内部适配器 `scripts/dev-public-preview.sh` 只供 `dev.sh` 调用，不是开发者入口。

### 说明

- 不提供 `attach`、`logs -f` 等阻塞入口，需要实时查看请 `tmux attach -t onlyboxes-dev`
- 服务进程退出后窗口会保留，`status` 显示「已退出」，再次 `start` 可原位复活
- `stop` 只回收本会话窗口进程树内的进程；端口被手动启动的实例占用时会明确提示并跳过，不会误杀
- 环境变量：`ONLYBOXES_DEV_SESSION`（会话名，默认 `onlyboxes-dev`）、`ONLYBOXES_DEV_ENV`（配置文件路径，默认 `scripts/dev.env`）、`ONLYBOXES_DEV_STATE_DIR`（日志、凭据和本地持久化状态目录，默认 `scripts/.dev`）、`ONLYBOXES_WORKER_DOCKER_RUNNER`（`worker-docker` 运行方式，macOS 默认 `orb`、Linux 默认 `native`）

## install.sh / install.py

面向部署的安装脚本，与本地开发无关，见 `README/` 下的安装文档。
