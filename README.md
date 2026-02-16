# Onlyboxes

面向个人与小型团队的代码执行沙箱平台解决方案

## 本地启动（console + worker-docker）

1. 启动 `console`（终端 1）：

```bash
cd /Users/yang/Documents/code/onlyboxes/console
CONSOLE_HTTP_ADDR=:8089 \
CONSOLE_GRPC_ADDR=:50051 \
CONSOLE_GRPC_SHARED_TOKEN=onlyboxes-dev-token,onlyboxes-backup-token \
go run ./cmd/console
```

`CONSOLE_GRPC_SHARED_TOKEN` 支持逗号分隔列表，任意一个 token 命中都视为合法。

2. 启动 `worker-docker`（终端 2）：

```bash
cd /Users/yang/Documents/code/onlyboxes/worker/worker-docker
WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 \
WORKER_GRPC_SHARED_TOKEN=onlyboxes-backup-token \
WORKER_HEARTBEAT_INTERVAL_SEC=5 \
go run ./cmd/worker-docker
```

3. 查看已注册 worker：

```bash
curl "http://127.0.0.1:8089/api/v1/workers?page=1&page_size=20&status=all"
```

## 前端开发（Vite 反向代理）

`web` 项目开发服务器会将 `/api/*` 代理到 `http://127.0.0.1:8089`。

```bash
yarn --cwd /Users/yang/Documents/code/onlyboxes/web dev
```

如需改代理目标可设置：

```bash
VITE_API_TARGET=http://127.0.0.1:8089 yarn --cwd /Users/yang/Documents/code/onlyboxes/web dev
```
