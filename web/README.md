# onlyboxes-web

Onlyboxes worker registry dashboard (Vue 3 + Vite + TypeScript).

## 功能

- 首次访问通过账号密码登录控制台（凭据由 `console` 启动时输出）
- 启动后通过 `GET /api/v1/console/session` 完成会话 bootstrap，并基于 `is_admin` 做角色分流
- 管理员默认进入 `/workers`
- 管理员可进入 `/accounts`：分页查看账号、删除普通账号；在 `registration_enabled=true` 时创建账号
- 通过控制台 Cookie 会话登录的账号可进入 `/tokens`：管理自己的 MCP trusted token
- 所有已登录账号都可进入 `/workers`
- `/accounts` 路由带管理员守卫，非管理员自动重定向 `/tokens`
- 已登录账号可在 `/workers`、`/accounts`、`/tokens` 页面弹窗修改自己的密码（`POST /api/v1/console/password`）
- token 管理来自 `GET/POST/DELETE /api/v1/console/tokens`（仅支持控制台 Cookie 会话；明文 token 仅在创建响应中返回一次）
- 管理员在 `registration_enabled=true` 时可在 `/accounts` 页面创建非管理员账号（`POST /api/v1/console/register`）
- 管理员可在 `/accounts` 页面分页查看账号列表（`GET /api/v1/console/accounts`）
- 管理员可删除普通账号（`DELETE /api/v1/console/accounts/:account_id`，禁止删除自己和管理员）
- worker 列表来自 `GET /api/v1/workers`（普通用户只会拿到本人 `worker-sys`）
- 统计卡片来自 `GET /api/v1/workers/stats`（普通用户仅统计本人 `worker-sys`）
- 创建 worker 使用 `POST /api/v1/workers`，请求体为 `{"type":"normal"|"worker-sys"}`
- 管理员可在 `/workers` 选择创建 `normal` 或 `worker-sys`
- 普通用户在 `/workers` 固定创建 `worker-sys`，且每账号最多一个（重复创建后端返回冲突）
- 后端 Dashboard JIT token 可用于服务端到服务端的 worker-sys 配置，但不能通过 `/tokens` 创建或管理 MCP trusted token
- 创建 worker 后自动展示创建响应中的启动命令（明文 `WORKER_SECRET` 仅创建时返回一次）
- `/tools/worker-startup` 的 worker 配置工具支持两种预览：多行 shell 启动命令，或与之等价的 `config.toml`（可直接下载，放在 worker 二进制同目录即可生效；环境变量优先级高于该文件）
- `config.toml` 预览不适用于 Temporary Probe 安装脚本预设
- `GET /api/v1/workers/:node_id/startup-command` 固定返回 `410 Gone`
- 节点能力列展示 `capabilities[].name` 能力声明
- 支持 `all / online / offline` 筛选、分页、手动刷新和自动刷新

## 目录结构

```
src/
├── components/
│   ├── ui/            # 无业务语义的基础组件：AppButton / AppModal / AppCard /
│   │                  # AppField / AppPagination / CopyButton / ConfirmDialogHost 等，
│   │                  # 图标由 icons.ts 数据驱动，经 AppIcon 渲染
│   ├── layout/        # ConsoleSidebar / PageHeader / UserMenu 等外壳组件
│   ├── workers/       # worker 列表、创建流程相关组件
│   ├── accounts/      # 账号管理相关组件
│   ├── tokens/        # trusted token 相关组件
│   ├── account/       # 当前账号自助操作（改密码、API Keys）
│   └── worker-tool/   # Worker Startup Tool 的表单区块，字段由 workerFieldSpecs.ts 描述
├── composables/       # 跨组件逻辑：useConfirm（替代 window.confirm）、useCopyFeedback、
│                      # useBodyScrollLock、useWorkersRouteSync、useWorkerConfigConstraints 等
├── stores/            # Pinia store，只负责数据与请求，不含格式化/确认等视图层职责
├── services/          # API 调用封装
├── utils/             # async（请求守卫、错误归一）、datetime、clipboard、secret
├── views/             # 路由级页面
├── layouts/           # 路由布局
├── router/  config/  constants/  theme/  types/  style/
└── __tests__/         # Vitest 组件级测试，testkit.ts 提供挂载与交互辅助
```

约定：

- 新增交互控件优先复用 `components/ui/`，避免复制样式类字符串
- 破坏性操作统一走 `useConfirm` 的 `requestConfirm()`，由 `App.vue` 挂载的 `ConfirmDialogHost` 渲染
- store 内的并发/取消统一使用 `utils/async.ts` 的 `createRequestGuard`

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
