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
- 环境变量：`ONLYBOXES_DEV_SESSION`（会话名，默认 `onlyboxes-dev`）、`ONLYBOXES_DEV_ENV`（配置文件路径，默认 `scripts/dev.env`）、`ONLYBOXES_WORKER_DOCKER_RUNNER`（`worker-docker` 运行方式，macOS 默认 `orb`、Linux 默认 `native`）

## install.sh / install.py

面向部署的安装脚本，与本地开发无关，见 `README/` 下的安装文档。
