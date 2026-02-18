# Onlyboxes

面向个人与小型团队的代码执行沙箱平台解决方案

## 本地启动（console + worker-docker）

1. 启动 `console`（终端 1）：

```bash
cd /Users/yang/Documents/code/onlyboxes/console
CONSOLE_HTTP_ADDR=:8089 \
CONSOLE_GRPC_ADDR=:50051 \
CONSOLE_REPLAY_WINDOW_SEC=60 \
CONSOLE_HEARTBEAT_INTERVAL_SEC=5 \
CONSOLE_DASHBOARD_USERNAME=admin \
CONSOLE_DASHBOARD_PASSWORD=change-me \
go run ./cmd/console
```

`console` 启动后，worker 数量默认为 `0`。worker 凭据（`worker_id/worker_secret`）由控制台 UI（或对应 API）按需新增生成，不再写启动凭据文件。

`console` 还会在终端打印控制台登录账号密码：
- 若 `CONSOLE_DASHBOARD_USERNAME` / `CONSOLE_DASHBOARD_PASSWORD` 有设置，则直接使用设置值。
- 若任一未设置，仅缺失项会随机生成。

2. 登录控制台（保存 Cookie）：

```bash
curl -c /tmp/onlyboxes-console.cookie -X POST "http://127.0.0.1:8089/api/v1/console/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"<dashboard_username>","password":"<dashboard_password>"}'
```

3. 新增 worker 并获取启动命令（接口需登录）：

```bash
curl -b /tmp/onlyboxes-console.cookie -X POST "http://127.0.0.1:8089/api/v1/workers"
```

响应示例：

```json
{
  "node_id": "2f51f8f9-77f2-4c1a-a4f5-2036fc9fcb9e",
  "command": "WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=... WORKER_SECRET=... WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 go run ./cmd/worker-docker"
}
```

4. 启动 `worker-docker`（终端 2，使用上一步返回的 `command`）：

```bash
cd /Users/yang/Documents/code/onlyboxes/worker/worker-docker
<command_from_create_worker_api>
```

5. 查看已注册 worker（仪表盘接口需登录）：

```bash
curl -b /tmp/onlyboxes-console.cookie "http://127.0.0.1:8089/api/v1/workers?page=1&page_size=20&status=all"
```

6. 一键复制场景对应的接口：按 worker 获取启动命令（接口需登录，响应仅返回命令文本）：

```bash
curl -b /tmp/onlyboxes-console.cookie "http://127.0.0.1:8089/api/v1/workers/<worker_id>/startup-command"
```

7. 删除 worker（接口需登录，若 worker 在线将被立即断开）：

```bash
curl -b /tmp/onlyboxes-console.cookie -X DELETE "http://127.0.0.1:8089/api/v1/workers/<worker_id>"
```

8. 调用 echo 命令链路（阻塞等待 worker 返回，执行类接口无需登录）：

```bash
curl -X POST "http://127.0.0.1:8089/api/v1/commands/echo" \
  -H "Content-Type: application/json" \
  -d '{"message":"hello onlyboxes","timeout_ms":5000}'
```

成功响应示例：

```json
{
  "message": "hello onlyboxes"
}
```

9. 提交通用任务（`mode=auto`，先等 `wait_ms`，未完成则返回 `202`）：

```bash
curl -X POST "http://127.0.0.1:8089/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -d '{"capability":"echo","input":{"message":"hello task"},"mode":"auto","wait_ms":1500,"timeout_ms":60000}'
```

10. 查询任务状态：

```bash
curl "http://127.0.0.1:8089/api/v1/tasks/<task_id>"
```

## 前端开发（Vite 反向代理）

`web` 项目开发服务器会将 `/api/*` 代理到 `http://127.0.0.1:8089`。
首次访问仪表盘需要先登录，登录凭据来自 `console` 启动日志。

```bash
yarn --cwd /Users/yang/Documents/code/onlyboxes/web dev
```

如需改代理目标可设置：

```bash
VITE_API_TARGET=http://127.0.0.1:8089 yarn --cwd /Users/yang/Documents/code/onlyboxes/web dev
```
