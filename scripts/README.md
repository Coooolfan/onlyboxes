# scripts

## dev.sh — 本地开发进程编排

用一个 tmux 会话托管 console / web / website，避免 `go run` 与 `vite dev` 阻塞终端。所有子命令都立即返回，日志同时落到 `scripts/.dev/<svc>.log`。

```bash
scripts/dev.sh start              # 三个全起
scripts/dev.sh start console web  # 只起控制节点与前端
scripts/dev.sh status             # 会话、端口监听与窗口状态
scripts/dev.sh logs console       # 最近 200 行日志快照
scripts/dev.sh creds              # console 管理员账号
scripts/dev.sh restart web        # 重启前端
scripts/dev.sh stop               # 全部停止并销毁会话
```

### 服务

| 服务 | 目录 | 命令 | 端口 |
| --- | --- | --- | --- |
| console | `console/` | `go run ./cmd/console` | 8089（HTTP）、50051（gRPC） |
| web | `web/` | `yarn dev` | 5178 |
| website | `website/` | `yarn dev` | 5173 |

前端开发只需 `start console web`：`web` 的 vite 已把 `/api` 与 `/mcp` 代理到 `127.0.0.1:8089`，浏览器开 <http://localhost:5178> 即可。

**不含 worker。** 各 worker 实现的启动参数差异较大，部分场景还需同时运行多个实例，请手动启动。

### 管理员账号

console 只在**首次初始化数据库**时生成管理员账号并打印一次。`creds` 会在首次抓到后固化到 `scripts/.dev/console-creds.json`（权限 600），之后可随时查看：

```bash
scripts/dev.sh creds
```

若凭据已丢失，删库重建：

```bash
scripts/dev.sh stop console
rm console/db/onlyboxes-console.db
scripts/dev.sh start console && scripts/dev.sh creds
```

### 配置

tmux server 的环境与调用方 shell 是隔离的，`FOO=bar scripts/dev.sh start` **不会生效**。服务所需配置写入 `scripts/dev.env`（不入库），start 时自动加载：

```bash
cp scripts/dev.env.example scripts/dev.env
```

改动后 `restart` 对应服务生效。

### 说明

- 不提供 `attach`、`logs -f` 等阻塞入口，需要实时查看请 `tmux attach -t onlyboxes-dev`
- 服务进程退出后窗口会保留，`status` 显示「已退出」，再次 `start` 可原位复活
- `stop` 只回收本会话窗口进程树内的进程；端口被手动启动的实例占用时会明确提示并跳过，不会误杀
- 环境变量：`ONLYBOXES_DEV_SESSION`（会话名，默认 `onlyboxes-dev`）、`ONLYBOXES_DEV_ENV`（配置文件路径，默认 `scripts/dev.env`）

## install.sh / install.py

面向部署的安装脚本，与本地开发无关，见 `README/` 下的安装文档。
