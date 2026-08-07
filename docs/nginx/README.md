# Public preview deployment

本功能仅支持单 Console、`worker-docker` 和 Linux Docker Engine。Nginx 示例按 `1.26` 验证。公开 URL 是匿名能力链接，routeKey 等同于访问凭据。

## 网络要求

仅开放以下链路：

| 来源 | 目标 | 端口 | 用途 |
| --- | --- | --- | --- |
| Internet | Nginx | `443/tcp` | 公开预览 |
| Nginx | Console | Console HTTP 端口 | `auth_request` |
| Nginx | Worker | 固定代理端口，默认 `8091/tcp` | Sandbox 流量 |
| Worker | Console | Console gRPC 端口 | Worker 注册与命令 |

Console HTTP 端口和 Worker 代理端口不得直接暴露给 Internet。Worker 代理入口只允许 Nginx 来源，Console 的 `/internal/v1/proxy/resolve` 也应只允许 Nginx 所在网络访问。

## 1. DNS 与证书

1. 将 `*.public-preview.example.com` 的 wildcard DNS 指向 Nginx。
2. 为同一 wildcard 域名签发 TLS 证书。wildcard 不覆盖基础域名本身，预览只使用单层子域。
3. 将证书和私钥部署到 Nginx，限制私钥文件权限。

## 2. Console

生成只在 Nginx 与 Console 之间共享的随机 Token，例如：

```bash
openssl rand -hex 32
```

配置 Console：

```bash
CONSOLE_PROXY_ENABLED=true
CONSOLE_PROXY_PUBLIC_BASE_DOMAIN=public-preview.example.com
CONSOLE_PROXY_INTERNAL_AUTH_TOKEN=<nginx-console-secret>
CONSOLE_PROXY_ALLOWED_WORKER_CIDRS=10.20.0.0/16
CONSOLE_PROXY_ALLOWED_WORKER_PORTS=8091
CONSOLE_PROXY_ROUTE_TTL_SEC=86400
```

CIDR 和端口 allowlist 必须只包含 Nginx 实际可达的 Worker 入口。Console 只接受单播 IP endpoint，不接受主机名、loopback、unspecified 或 allowlist 外地址。Route TTL 最大为 `604800` 秒（7 天），超出时 Console 拒绝启动。

## 3. Worker

在 Linux Docker host 上配置每个 `worker-docker`：

```bash
WORKER_PROXY_ENABLED=true
WORKER_PROXY_LISTEN_ADDR=:8091
WORKER_PROXY_ADVERTISE_ADDR=10.20.1.15:8091
```

listen 与 advertise 端口必须相同，advertise IP 必须可由 Nginx 直接访问并匹配 Console allowlist。Worker 启动时创建或验证 `onlyboxes-sandbox` bridge，并要求 `com.docker.network.bridge.enable_icc=false`。Docker daemon 必须启用自己的 iptables/nftables 防火墙管理；禁止使用会绕过 bridge 隔离的 `iptables=false` 或等价配置。代理 listener 启动失败时 Worker 不会向 Console 注册。

## 4. Nginx

1. 将 `public-preview.conf.example` 中 Host map 与 `server_name` 的域名、证书路径、Console 地址和内部 Token 全部替换为部署值。
2. 将文件中的 `map` 放在 `http {}` 中，`server` 块也放在同一 `http {}` 中。
3. 验证并平滑重载：

```bash
nginx -t
nginx -s reload
```

Nginx 从 Console 接收的 upstream 必须保持为 IP 加端口；不要从客户端 Header 构造 `proxy_pass`。示例会覆盖客户端提供的 Onlyboxes 内部 Header，同时保留 Sandbox 应用自己的 `Authorization` 和 Cookie。

## 5. Smoke test

先通过已鉴权的 Terminal API 创建 Session 并在其中启动 HTTP 服务，再创建 route：

```bash
curl -sS https://console.example.com/api/v1/proxy-routes \
  -H 'Authorization: Bearer <console-api-key>' \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"<session-id>","port":8080}'
```

随后在不携带 Onlyboxes Cookie 或 Authorization 的情况下访问返回 URL：

```bash
curl -i https://<route-key>.public-preview.example.com/
```

应同时验证：

- 直接请求 Worker 固定端口且不带有效 Route Token 返回 `401`；
- 删除 route 后，公开 URL 的新请求立即被拒绝；
- Worker 断线后新请求被拒绝；
- Sandbox lease 到期时已建立的 SSE/WebSocket 连接断开，容器被回收；
- Sandbox 未监听签名端口时公开请求返回 `502`。

route 存储在 Console 内存中，Console 重启后全部失效。预览访问不会续租。Route Token 只限制新请求到达 Worker 的 15 秒窗口，不会单独终止已建立连接。访问日志可能包含 routeKey，应按凭据日志处理并限制留存和读取权限。

## Nginx 自动验证

在仓库根目录执行以下脚本，可使用临时 Nginx 容器验证配置语法、匿名访问、`auth_request`、Host 拒绝、动态 upstream 及内部 Header 覆盖。脚本退出时会清理容器、网络和证书：

```bash
docs/nginx/verify-public-preview.sh
```

## Linux Docker 集成测试

在原生 Linux Docker host 上可执行 Worker 的 opt-in 集成测试。测试会创建隔离 bridge 和临时 Session 容器，启动真实 Nginx Sandbox 服务，验证固定代理入口、单次 IP inspect，并确认同 bridge 的第二个容器无法横向访问目标：

```bash
cd worker/worker-docker
ONLYBOXES_DOCKER_INTEGRATION=1 go test ./internal/runner \
  -run '^TestSandboxProxyLinuxDockerIntegration$' -count=1 -v
```

部署前必须在目标 Docker host 上通过该测试；失败表示 daemon 的 bridge 防火墙或 host 网络策略没有提供方案要求的 Sandbox 横向隔离。
