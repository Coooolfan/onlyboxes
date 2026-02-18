# Onlyboxes

面向个人与小型团队的代码执行沙箱平台解决方案

## 本地启动（console + worker-docker）

1. 启动 `console`（终端 1）：

```bash
cd console
CONSOLE_HTTP_ADDR=:8089 \
CONSOLE_GRPC_ADDR=:50051 \
CONSOLE_REPLAY_WINDOW_SEC=60 \
CONSOLE_HEARTBEAT_INTERVAL_SEC=5 \
CONSOLE_DASHBOARD_USERNAME=admin \
CONSOLE_DASHBOARD_PASSWORD=change-me \
go run ./cmd/console
```

`console` 启动后，worker 数量默认为 `0`。worker 凭据（`worker_id/worker_secret`）由控制台 UI（或对应 API）按需新增生成。

`console` 会在终端打印控制台登录账号密码：
- 若 `CONSOLE_DASHBOARD_USERNAME` / `CONSOLE_DASHBOARD_PASSWORD` 有设置，则直接使用设置值。
- 若任一未设置，仅缺失项会随机生成。

可信 token 由控制台 API/UI 运行时管理（内存态，不走环境变量）。  
若当前未配置任何 token，`/mcp` 与执行类 API 会返回 `401`。

2. 登录控制台（保存 Cookie）：

```bash
curl -c /tmp/onlyboxes-console.cookie -X POST "http://127.0.0.1:8089/api/v1/console/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"<dashboard_username>","password":"<dashboard_password>"}'
```

3. 查看当前可信 token（接口需登录）：

```bash
curl -b /tmp/onlyboxes-console.cookie "http://127.0.0.1:8089/api/v1/console/tokens"
```

4. 新增可信 token（可手动指定 token，或省略 token 自动生成）：

```bash
curl -b /tmp/onlyboxes-console.cookie -X POST "http://127.0.0.1:8089/api/v1/console/tokens" \
  -H "Content-Type: application/json" \
  -d '{"name":"ci-prod"}'
```

5. 新增 worker 并获取启动命令（接口需登录）：

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

6. 启动 `worker-docker`（终端 2，使用上一步返回的 `command`）：

```bash
cd worker/worker-docker
<command_from_create_worker_api>
```

7. 查看已注册 worker（仪表盘接口需登录）：

```bash
curl -b /tmp/onlyboxes-console.cookie "http://127.0.0.1:8089/api/v1/workers?page=1&page_size=20&status=all"
```

8. 一键复制场景对应的接口：按 worker 获取启动命令（接口需登录，响应仅返回命令文本）：

```bash
curl -b /tmp/onlyboxes-console.cookie "http://127.0.0.1:8089/api/v1/workers/<worker_id>/startup-command"
```

9. 删除 worker（接口需登录，若 worker 在线将被立即断开）：

```bash
curl -b /tmp/onlyboxes-console.cookie -X DELETE "http://127.0.0.1:8089/api/v1/workers/<worker_id>"
```

10. 调用 echo 命令链路（阻塞等待 worker 返回，执行类接口需携带可信 token）：

```bash
curl -X POST "http://127.0.0.1:8089/api/v1/commands/echo" \
  -H "Content-Type: application/json" \
  -H "X-Onlyboxes-Token: <trusted_token>" \
  -d '{"message":"hello onlyboxes","timeout_ms":5000}'
```

成功响应示例：

```json
{
  "message": "hello onlyboxes"
}
```

11. 提交通用任务（`mode=auto`，先等 `wait_ms`，未完成则返回 `202`）：

```bash
curl -X POST "http://127.0.0.1:8089/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -H "X-Onlyboxes-Token: <trusted_token>" \
  -d '{"capability":"echo","input":{"message":"hello task"},"mode":"auto","wait_ms":1500,"timeout_ms":60000}'
```

12. 查询任务状态：

```bash
curl -H "X-Onlyboxes-Token: <trusted_token>" "http://127.0.0.1:8089/api/v1/tasks/<task_id>"
```

13. 调用 MCP 接口（必须带可信 token）：

```bash
curl -X POST "http://127.0.0.1:8089/mcp" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "X-Onlyboxes-Token: <trusted_token>" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

## 前端开发（Vite 反向代理）

`web` 项目开发服务器会将 `/api/*` 代理到 `http://127.0.0.1:8089`。
首次访问仪表盘需要先登录，登录凭据来自 `console` 启动日志。

```bash
yarn --cwd /web dev
```

如需改代理目标可设置：

```bash
VITE_API_TARGET=http://127.0.0.1:8089 yarn --cwd /web dev
```
