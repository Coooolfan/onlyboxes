# onlyboxes-web

Onlyboxes worker registry dashboard (Vue 3 + Vite + TypeScript).

## 功能

- 首次访问需先通过账号密码登录控制台（凭据由 `console` 启动时输出）
- 实时节点注册与心跳状态仪表台
- 列表数据来自 `GET /api/v1/workers`
- 统计卡片数据来自 `GET /api/v1/workers/stats`
- 可信 token 管理来自 `GET/POST/DELETE /api/v1/console/tokens`（明文 token 仅在创建响应中返回一次）
- 创建 worker 后自动复制创建响应中的启动命令（明文 `WORKER_SECRET` 仅创建时返回一次）
- 不提供按 worker 重新获取启动命令（`GET /api/v1/workers/:node_id/startup-command` 返回 `410 Gone`）
- 节点能力列展示 `capabilities[].name` 能力声明
- 支持 `all / online / offline` 筛选、分页、手动刷新和自动刷新

## 开发

```bash
yarn
```

```bash
yarn dev
```

默认开发端口：`5178`

默认开发代理：

- `/api/*` -> `http://127.0.0.1:8089`

可通过环境变量覆盖：

```bash
VITE_API_TARGET=http://127.0.0.1:8089 yarn dev
```

## 构建与测试

```bash
yarn build
yarn test:unit
```
