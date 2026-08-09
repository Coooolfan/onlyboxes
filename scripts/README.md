# scripts

## dev.sh — 本地开发进程编排

用一个 tmux 会话托管 console / web / website 以及显式启动的 Worker，避免开发进程阻塞终端。所有子命令都立即返回，日志同时落到 `scripts/.dev/<svc>.log`。

```bash
scripts/dev.sh start                       # 启动默认服务：console / web / website
scripts/dev.sh start console web           # 只启动控制节点与前端
scripts/dev.sh start worker-docker         # 显式启动 Docker Worker
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

前端开发只需 `start console web`：`web` 的 vite 已把 `/api` 与 `/mcp` 代理到 `127.0.0.1:8089`，浏览器开 <http://localhost:5178> 即可。

Worker 不属于无参数 `start` 的默认服务集合，必须显式指定。启动前先生成对应的本机凭据配置：

```bash
scripts/dev.sh start console
scripts/dev-worker.sh provision worker-docker
scripts/dev.sh start worker-docker
```

`provision` 在 Console 已启动的情况下调用 `POST /api/v1/workers`，将一次性返回的凭据写入 `scripts/.dev/worker-docker.env` 并设置权限为 `600`。重复执行时复用当前 Console 数据库中仍然存在的 Worker；Console 数据库重建后自动创建新 Worker 并覆盖失效的本机凭据。

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

Worker 凭据和运行参数使用独立文件 `scripts/.dev/<worker>.env`，由 `scripts/dev-worker.sh provision` 管理，不应手动复制到 `scripts/dev.env`。

## dev-worker.sh — Worker 本机凭据

Console 启动后，为指定 Worker 创建或复用本地开发凭据：

```bash
scripts/dev-worker.sh provision worker-docker
```

该脚本只负责 Console API 认证、Worker 凭据生命周期和 `.env` 文件生成，不负责启动 Worker、Nginx 或 Docker 网络。

## dev-nginx.sh — 本地公开预览入口

Nginx 容器和公开预览专用 Docker 网络独立于 tmux 服务编排：

```bash
scripts/dev-nginx.sh start
scripts/dev-nginx.sh status
scripts/dev-nginx.sh logs
scripts/dev-nginx.sh stop
```

域名、监听端口和 Docker 网络等本机覆盖项写入 `scripts/public-preview.env`。渲染后的 Nginx 配置位于 `scripts/.dev/public-preview-nginx.conf`，两者均不入库。Nginx 与 Console 使用相同的 `CONSOLE_PROXY_INTERNAL_AUTH_TOKEN`。

`dev-nginx.sh` 只管理自己创建的 Nginx 容器和专用网络，不会隐式启动或停止 Console 与 Worker。完整的本地公开预览启动顺序为：

```bash
scripts/dev.sh start console web
scripts/dev-worker.sh provision worker-docker
scripts/dev.sh start worker-docker
scripts/dev-nginx.sh start
```

首次完成 provision 后，日常开发通常只需：

```bash
scripts/dev.sh start console web worker-docker
scripts/dev-nginx.sh start
```

### 说明

- 不提供 `attach`、`logs -f` 等阻塞入口，需要实时查看请 `tmux attach -t onlyboxes-dev`
- 服务进程退出后窗口会保留，`status` 显示「已退出」，再次 `start` 可原位复活
- `stop` 只回收本会话窗口进程树内的进程；端口被手动启动的实例占用时会明确提示并跳过，不会误杀
- 环境变量：`ONLYBOXES_DEV_SESSION`（会话名，默认 `onlyboxes-dev`）、`ONLYBOXES_DEV_ENV`（配置文件路径，默认 `scripts/dev.env`）、`ONLYBOXES_WORKER_DOCKER_RUNNER`（`worker-docker` 运行方式，macOS 默认 `orb`、Linux 默认 `native`）

## install.sh / install.py

面向部署的安装脚本，与本地开发无关，见 `README/` 下的安装文档。
