# Onlyboxes

[English](../README.md)

Onlyboxes 是一个面向个人与小型团队的自托管代码执行沙箱平台。

系统采用控制面（`console`）与执行面（`worker`）分离架构，并同时提供 REST API 与 MCP 接口。

## 核心能力

- 自托管所有组件：控制节点（`console`）+ 执行节点（`worker`）
- 控制面分离：
  - 执行节点支持 **横向扩展**
  - 执行节点支持 多语言**异构**
  - 执行节点支持 **多种运行时**
- 完整的账号体系：账号间资源（有状态容器、会话）横向隔离
- MCP 接口：
  - `pythonExec`：Python 代码执行
  - `terminalExec`：有状态终端会话
  - `readImage`：模型可读的图片
- REST API 接口：所有 MCP 接口均支持 HTTP 调用 + 异步任务接口

> [!WARNING]
>
> 当前版本中，console (gRPC + HTTP) 不提供内建 TLS/mTLS 支持。
>
> `worker` 默认会拒绝不安全的 console 端点；只有显式设置 `WORKER_CONSOLE_INSECURE=true` 才允许明文连接。
>
> 请将 console 的 HTTP（`:8089`）和 gRPC（`:50051`）端点放在反向代理/网关之后，并对外部流量强制开启 TLS。

## 架构

![架构](../static/architecture.zh-CN.svg#gh-light-mode-only)
![架构](../static/architecture.zh-CN-dark.svg#gh-dark-mode-only)

## 一键安装脚本（Linux）

在单台机器上部署 `console` + `worker-docker`：

```bash
curl -fsSL https://onlybox.es/install.sh | bash
```

安装器将自动完成：

1. 环境检查（Linux、Docker、Docker Compose v2、systemd）
2. 下载 compose 模板并使用自动生成的凭据渲染
3. 通过 `docker compose up -d` 启动 console
4. 创建 `normal` worker
5. 下载与当前架构匹配的 `worker-docker` release 二进制，默认使用最新版本，也可被 `--tag` 覆盖
6. 生成并启用 systemd 服务
7. 轮询直到 worker 上线，输出安装结果摘要

可用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--tag` | 最新发布版本 | 可选的 Release 版本覆盖参数 |
| `--workdir` | `$PWD/onlyboxes` | 工作目录 |
| `--yes` / `-y` | `false` | 非交互模式，跳过确认 |
| `--console-http-port` | `8089` | Console HTTP 端口（宿主机侧） |
| `--console-grpc-port` | `50051` | Console gRPC 端口（宿主机侧） |
| `--service-name` | `onlyboxes-worker-docker` | systemd 服务名称 |

运行要求：Linux、systemd、Docker Engine、Docker Compose v2、Python 3。

## 快速开始（手动部署）

### 1）前置条件

- 控制节点：
  - Docker Engine（release 中也提供二进制文件，使用二进制文件部署无需安装 Docker）
- 执行节点
  - Docker Engine（`worker-docker` 依赖）

### 2）启动 console

1. 创建工作目录，并在其中新建 `docker-compose.yml`：

    ```bash
    mkdir -p onlyboxes-console && cd onlyboxes-console
    ```

    ```yaml
    services:
      console:
        image: coolfan1024/onlyboxes:latest
        container_name: onlyboxes-console
        restart: unless-stopped
        environment:
          CONSOLE_HASH_KEY: "replace-with-long-random-key"
          CONSOLE_ENABLE_REGISTRATION: "true"
          CONSOLE_DASHBOARD_USERNAME: "admin"     # only for first run
          CONSOLE_DASHBOARD_PASSWORD: "change-me" # only for first run
        ports:
          - "8089:8089"
          - "50051:50051"
        volumes:
          - ./db:/app/db
    ```

2. 至少替换：
   - `CONSOLE_HASH_KEY`
   - `CONSOLE_DASHBOARD_PASSWORD`
3. 启动服务：

    ```bash
    docker compose up -d
    ```

默认访问地址：

- 控制台 Web UI / HTTP REST API / MCP 端点：`http://127.0.0.1:8089`
- gRPC：`127.0.0.1:50051`

### 3）登录并创建访问 token

- 浏览器打开 `http://127.0.0.1:8089`。
- 使用初始化的管理员账号登录。
![控制台登录页](../static/docs/quickstart-login.png)
- 进入 token 管理页面创建访问 token。
![Token 创建完成弹窗（一次性明文）](../static/docs/quickstart-token-modal.png)
- token 明文只返回一次，请立即安全保存。

### 4）创建 worker

- 在 Workers 页面创建 worker。
![Workers 页面](../static/docs/quickstart-workers-page.png)
- 在创建弹窗中复制并安全保存启动命令（`WORKER_SECRET` 仅一次可见）。
![Worker 创建完成弹窗（启动命令与一次性密钥）](../static/docs/quickstart-worker-created-modal.jpg)
- （可选）点击 `Open in Startup Tool with Id and Secret` 携带 id 与 key 进入启动命令编辑工具
  - 在弹出的页面中，你可以编辑所有可用的选项，页面最下方会生成对应的启动命令，复制并保存
![启动命令编辑工具](../static/docs/quickstart-worker-startup-tool.jpg)


### 5）启动 worker

> [!WARNING]
> worker 支持不同运行时与运行环境，当前版本仅提供 `worker-docker`。本小节以此 docker 运行时为例。

1. 登陆到需要部署 worker 的机器。
    - 确保 Docker Engine 已安装。
    - 确保 worker 可以访问 console gRPC 端点。
2. 从 GitHub Releases 下载最新 `worker-docker` 二进制：
    - `https://github.com/onlyboxes/onlyboxes/releases/latest`
3. 将控制台中创建 worker 返回的参数替换到启动命令中，并将最后一行的可执行文件路径替换为你下载的二进制文件。
    - worker 默认拒绝不安全的 console 端点；只有显式设置 `WORKER_CONSOLE_INSECURE=true` 才允许明文连接。

    ```bash
    # 示例
    WORKER_CONSOLE_INSECURE=true \
    WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 \
    WORKER_ID=<worker_id> \
    WORKER_SECRET=<worker_secret> \
    /path/to/onlyboxes-worker-docker
    ```

### 6）验证运行状态

- 在控制台 Workers 页面确认 worker 状态为 `online`。
- REST API 调用示例请参考 `API.zh-CN.md`。
- 若系统中没有任何 token，`/mcp` 与执行类 API 会按预期返回 `401`。
- 在任意 LLM Chat Client 中添加 MCP 端点 `http://127.0.0.1:8089/mcp`，并设置 token，确认可以正常工作。
![claude-code-demo](../static/claude-code-demo.jpg)

## 常见问题

- Q: worker 启动后状态一直是 `offline`？
  A: 检查 worker 的 `WORKER_CONSOLE_GRPC_TARGET` 是否正确指向 console 的 gRPC 地址，确保网络连通性。

- Q: worker 可以部署在控制台所在的机器上吗？
  A: 可以。

- Q: worker 可以使用 docker 部署/运行吗？
  A: 理论可以。但不推荐，因为 worker 需要访问宿主机的 Docker 守护进程。或者您需要自行解决 docker in docker 的问题。

## 检查清单

- 替换所有默认账号和默认密钥。
- 使用反向代理为 `:8089`、`:50051` 提供强制 TLS 支持。
- 持久化并备份 SQLite 数据目录（`CONSOLE_DB_PATH`）。
- 将 worker 部署在独立主机上，避免与控制台共用 Docker 守护进程。
- 阅读下文`配置参考`，了解所有配置项，根据需求调整配置。

## 配置参考

### Console（`console`）

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CONSOLE_HTTP_ADDR` | `:8089` | 控制台与 REST API 监听地址 |
| `CONSOLE_GRPC_ADDR` | `:50051` | Worker 注册 gRPC 监听地址 |
| `CONSOLE_HASH_KEY` | _(必填)_ | 用于哈希 `worker_secret` 和访问 token 的 HMAC 密钥 |
| `CONSOLE_DB_PATH` | `./db/onlyboxes-console.db` | SQLite 数据库路径 |
| `CONSOLE_DB_BUSY_TIMEOUT_MS` | `5000` | SQLite busy timeout |
| `CONSOLE_TASK_RETENTION_DAYS` | `30` | 已完成任务保留天数 |
| `CONSOLE_ENABLE_REGISTRATION` | `false` | 是否允许管理员创建非管理员账号 |
| `CONSOLE_DASHBOARD_USERNAME` | _(空)_ | 仅首次初始化管理员账号时生效 |
| `CONSOLE_DASHBOARD_PASSWORD` | _(空)_ | 仅首次初始化管理员账号时生效 |

### Worker（`worker-docker`）

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `WORKER_ID` | _(必填)_ | 由 `POST /api/v1/workers` 下发 |
| `WORKER_SECRET` | _(必填)_ | 由 `POST /api/v1/workers` 一次性下发 |
| `WORKER_CONSOLE_GRPC_TARGET` | `127.0.0.1:50051` | Console gRPC 目标地址 |
| `WORKER_CONSOLE_INSECURE` | `false` | `false` 表示要求 TLS 端点；仅在需要明文 console gRPC 时设置为 `true` |
| `WORKER_HEARTBEAT_INTERVAL_SEC` | `5` | 心跳周期 |
| `WORKER_HEARTBEAT_JITTER_PCT` | `20` | 心跳抖动百分比 |
| `WORKER_PYTHON_EXEC_DOCKER_IMAGE` | `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` | `pythonExec` 运行镜像 |
| `WORKER_TERMINAL_EXEC_DOCKER_IMAGE` | `coolfan1024/onlyboxes-runtime:default` | `terminalExec` 运行镜像 |
| `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` | `1048576` | 单路输出流字节上限 |

## API 面

- 控制台认证：`/api/v1/console/*`
- Worker 管理（管理员）：`/api/v1/workers*`
- 命令执行：`/api/v1/commands/echo`、`/api/v1/commands/terminal`
- 任务接口：`/api/v1/tasks*`
- MCP（Streamable HTTP）：`POST /mcp`

## 开发说明

### 本地开发编排

`scripts/dev.sh` 用一个 tmux 会话托管 console / web / website，所有子命令立即返回，日志落到 `scripts/.dev/<svc>.log`。

```bash
scripts/dev.sh start              # 三个全起
scripts/dev.sh start console web  # 只起控制节点与前端
scripts/dev.sh status             # 会话、端口监听与窗口状态
scripts/dev.sh logs console       # 最近 200 行日志
scripts/dev.sh creds              # console 管理员账号
scripts/dev.sh stop               # 全部停止
```

前端开发默认地址为 `http://127.0.0.1:5178`，并将 `/api/*` 与 `/mcp` 代理到 `http://127.0.0.1:8089`。

worker 不在编排范围内：各实现启动参数差异较大，需手动启动。完整用法见 `scripts/README.md`。

### 延伸文档

- 统一 API 文档：`API.zh-CN.md`
- Console 细节：`console/README.md`
- Worker 细节：`worker/worker-docker/README.md`
- API/proto 说明：`api/README/proto.md`
- Web 说明：`web/README.md`

## 发布与镜像

- GitHub 工作流：`.github/workflows/package-release.yml` —— 在 Actions 页面手动触发，选择分支并填写版本号（如 `0.7.2`），tag 会基于该分支创建。为旧版本线补发时关闭 `latest`，避免抢走最新版本的标记
- 仅部署官网：`.github/workflows/deploy-website.yml` —— 手动触发，不依赖发版流程
- Console 镜像：`coolfan1024/onlyboxes:<version>` 与 `coolfan1024/onlyboxes:latest`
- 终端运行时镜像：`coolfan1024/onlyboxes-runtime:<version>-default`、`<version>-default-cn` 与 `<version>-lobehub`；稳定别名为 `default`、`default-cn`、`lobehub` 和 `latest`（等同于 `default`）
- Console 二进制已内置前端静态资源

## 安全与运维注意事项

- 当前版本 console 不提供内建 TLS/mTLS；`worker-docker` 只有在显式设置 `WORKER_CONSOLE_INSECURE=true` 时才会走明文连接。
- 请将 console HTTP（`:8089`）和 gRPC（`:50051`）放在反向代理/网关之后，对公网或外部链路强制 TLS。
- `WORKER_SECRET` 与 token 明文都只在创建时返回一次。
- 控制台登录会话为内存态，`console` 重启后会失效。

## 友情链接

- [linux.do](https://linux.do/)
- [boxlite.ai](https://boxlite.ai/)

## 许可证

[GNU AGPL v3.0](../LICENSE)
