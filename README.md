# Onlyboxes

面向个人与小型团队的代码执行沙箱平台解决方案

## 安全告警（高危）

- `worker -> console` 的 gRPC 连接当前默认是明文传输（未启用 TLS/mTLS）。
- `ConnectHello` 中包含 `worker_secret`，在不受信网络路径上可能被窃听。
- 仅建议部署在受信私网或已加密隧道内，禁止跨公网裸连。
- 根治方案需要引入 TLS/mTLS（本版本尚未实现）。

## 本地启动（console + worker-docker）

1. 启动 `console`（终端 1）：

```bash
cd console
CONSOLE_HTTP_ADDR=:8089 \
CONSOLE_GRPC_ADDR=:50051 \
CONSOLE_HEARTBEAT_INTERVAL_SEC=5 \
CONSOLE_HASH_KEY=replace-with-long-random-key \
CONSOLE_DASHBOARD_USERNAME=admin \
CONSOLE_DASHBOARD_PASSWORD=change-me \
go run ./cmd/console
```

`console` 启动后，worker 数量默认为 `0`。worker 凭据（`worker_id/worker_secret`）由控制台 UI（或对应 API）按需新增生成，并以哈希形式持久化到 SQLite。

`console` 会在终端打印控制台登录账号密码：
- 若 `CONSOLE_DASHBOARD_USERNAME` / `CONSOLE_DASHBOARD_PASSWORD` 有设置，则直接使用设置值。
- 若任一未设置，仅缺失项会随机生成。

可信 token 由控制台 API/UI 管理并持久化到 SQLite（仅存哈希，不存明文）。  
若当前未配置任何 token，`/mcp` 与执行类 API 会返回 `401`。

数据库相关环境变量：
- `CONSOLE_DB_PATH`（默认 `./onlyboxes-console.db`）
- `CONSOLE_DB_BUSY_TIMEOUT_MS`（默认 `5000`）
- `CONSOLE_TASK_RETENTION_DAYS`（默认 `30`）

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

8. 历史接口（已废弃）：按 worker 获取启动命令（接口需登录）：

```bash
curl -b /tmp/onlyboxes-console.cookie "http://127.0.0.1:8089/api/v1/workers/<worker_id>/startup-command"
```

该接口现为破坏性变更：固定返回 `410 Gone`。`worker_secret` 仅在创建 worker 时返回一次，恢复路径为删除后重建 worker。

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

补充说明：
- `GET /api/v1/console/tokens/:token_id/value` 固定返回 `410 Gone`，token 明文仅在 `POST /api/v1/console/tokens` 创建响应返回一次。
- dashboard 登录会话为内存态，`console` 重启后会失效，需要重新登录。

## 发布打包（GitHub Actions）

仓库提供 `package-release` 工作流（`.github/workflows/package-release.yml`）：

- `workflow_dispatch`：手动触发，必须传入 `version`，仅上传 workflow artifact。
- `release.published`：发布触发，自动构建并上传二进制到对应 GitHub Release。

产物固定为 Linux amd64 两个可执行文件：

- `onlyboxes-console_<version>_linux_amd64`
- `onlyboxes-worker-docker_<version>_linux_amd64`

其中 `console` 二进制内嵌 `web` 前端静态资源，部署后可直接提供页面服务。

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
