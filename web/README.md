# onlyboxes-web

Onlyboxes worker registry dashboard (Vue 3 + Vite + TypeScript).

## 功能

- 实时节点注册与心跳状态仪表台
- 列表数据来自 `GET /api/v1/workers`
- 统计卡片数据来自 `GET /api/v1/workers/stats`
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
