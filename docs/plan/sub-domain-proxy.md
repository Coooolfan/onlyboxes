# Onlyboxes 子域名代理计划

## 范围

提供匿名预览地址：

```text
https://<routeKey>.public-preview.example.com
```

任何持有 URL 的访问者都可访问；Onlyboxes 身份只用于创建、查询和删除路由，不参与预览流量鉴权。

本期仅支持：

- 单 Console；
- `worker-docker`、`worker-boxlite` 与 `worker-bridge-e2b`；
- HTTP、SSE、WebSocket；
- Worker 固定代理端口，不映射随机宿主机端口；
- 代理访问不续租，lease 到期后销毁 Sandbox 并断开连接；
- route 默认保留 24 小时、最长 7 天，并持久化到 SQLite；Console 重启后恢复原 URL。

## 数据流

```text
Browser
  -> Nginx（按 Host 提取 routeKey）
  -> Console 内部子请求（解析 Session、Worker、Port）
  -> Docker/Boxlite：Nginx 携带短期 Route Token 请求 Worker 固定端口，再转发到 Sandbox
  -> E2B：Console 向 Worker 查询 sandbox origin，Nginx 携带内部 traffic token 直连 E2B
```

浏览器始终只访问公开子域，不接触 Worker 地址、Session ID 或 Route Token。

## Console

将 route 持久化到 SQLite，并维护内存索引：

```text
routeKey -> ownerID, sessionID, port, workerID, expiresAt
```

管理 API：

```text
POST   /api/v1/proxy-routes
GET    /api/v1/proxy-routes
DELETE /api/v1/proxy-routes/:routeKey
```

管理 API 必须鉴权并按 owner 隔离。`POST` 接收 `{"session_id":"...","port":8080}`，创建时验证 Session 归属、现有 Worker 映射和端口范围，生成 128-bit、全小写、DNS-safe 的 `routeKey`。账号和单 Session 的 route 数量分别由 `CONSOLE_PROXY_ROUTE_MAX_PER_ACCOUNT` 与 `CONSOLE_PROXY_ROUTE_MAX_PER_SESSION` 限制，默认值为 16 和 2。

Nginx 内部接口：

```text
GET /internal/v1/proxy/resolve
X-Onlyboxes-Internal-Token: <nginx-secret>
X-Original-Host: <routeKey>.public-preview.example.com
```

Console 默认关闭代理；启用时配置公开基础域名、Nginx 内部 Token、默认 24 小时且最长 7 天的 route TTL，以及允许 Worker advertise endpoint 的 CIDR/端口 allowlist。Console 校验 route、当前 Session 映射和 Worker 连接，成功返回：

```http
204 No Content
X-Onlyboxes-Upstream: 10.0.2.15:8091
X-Onlyboxes-Route-Token: <token>
```

Console 不判断 Worker 本地 lease；Session 是否仍存在由 Worker 最终确认。Console 根据当前 Docker/Boxlite Worker 连接签发 Route Token；E2B 每次 resolve 都通过内部 `terminalProxy` capability 返回当前 sandbox origin。

## Route Token

格式沿用项目现有紧凑 Token 风格：

```text
obx_route_v1.<base64url(payload)>.<base64url(signature)>
```

```json
{
  "worker_id": "worker-01",
  "session_id": "obx:acc_xxx:session-id",
  "port": 8080,
  "exp": 1730000015000
}
```

`session_id` 使用 owner-scoped 内部 Session ID，`exp` 使用 Unix 毫秒并取 `min(now + 15s, route.expiresAt)`。

每个 Worker 复用现有 `WORKER_SECRET` 派生签名密钥，不新增配置，也不由 Console 下发：

```text
K_route   = HMAC-SHA256(key=worker_secret, message="onlyboxes/proxy-route/v1")
signature = HMAC-SHA256(key=K_route, message="obx_route_v1." + base64url(payload))
```

Worker 启动时本地派生 `K_route`。Console 在 Worker Hello 鉴权成功后使用请求中的 `worker_secret` 派生同一密钥，只在该 Worker 在线期间保存在内存中，断线后删除。Worker 必须校验签名、`worker_id`、端口范围和有效期。Token 不包含 `ownerID`，也不用于浏览器鉴权。

## Worker

启用固定代理入口：

```text
WORKER_PROXY_ENABLED=true
WORKER_PROXY_LISTEN_ADDR=:8091
WORKER_PROXY_ADVERTISE_ADDR=10.0.2.15:8091
```

Worker 通过当前 `ConnectHello` 上报经过校验的单播 endpoint；listen 与 advertise 必须使用同一固定端口：

```text
obx.proxy_endpoint=10.0.2.15:8091
```

Worker 收到请求后：

1. 验证 Route Token、`worker_id`、端口范围和有效期；
2. 在 Session Manager 中确认 Session 存在且 lease 有效；
3. 使用创建容器时缓存的 IP 和 Token 中的 port 构造目标；
4. 删除内部 Route Token Header 后，通过 `httputil.ReverseProxy` 转发。

Terminal 容器加入专用 bridge `onlyboxes-sandbox`，并要求 Docker daemon 启用 iptables/nftables 防火墙管理以落实 `enable_icc=false`。Worker 在容器启动后只解析一次 IP 并缓存，避免每个静态资源请求执行 `docker inspect`。

## Nginx 与安全边界

完整部署步骤见 `docs/nginx/README.md`，Nginx 配置见 `docs/nginx/public-preview.conf.example`。安全约束：

- 配置 wildcard DNS/TLS，并严格匹配单层 `routeKey` Host；
- `auth_request` 位置必须为 `internal`，Console 内部接口仅允许可信 Nginx 调用；
- 捕获 Console 返回的 upstream 和 Token，向 Worker 保留规范化后的公开 Host，并覆盖客户端伪造的内部 Header；
- 保留 Sandbox 应用自己的 Authorization 和 Cookie；
- 配置 WebSocket Upgrade、SSE、流式请求和合理超时；
- Nginx 只能访问已注册 Worker 的固定代理端口，Worker 代理端口也只接受 Nginx 来源；
- 严格校验 Worker endpoint，避免动态 `proxy_pass` 形成内网 SSRF；
- route 删除或过期立即拒绝新请求；已建立连接在 Session/lease 清理时断开。

## 验收

必须覆盖：

1. route 创建、查询、删除、过期和 owner 隔离；
2. 无 Onlyboxes Cookie 的浏览器可直接访问公开 URL；
3. Host、内部 Header 和 Route Token 伪造均被拒绝；
4. HTTP、POST Body、大文件、SSE 和 WebSocket；
5. Sandbox 未监听端口时返回 502；
6. Session/Worker 下线后拒绝新连接，lease 到期后连接断开；
7. 页面多资源请求不会重复执行 `docker inspect`。
