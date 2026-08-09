Onlyboxes 是一个面向个人与小型团队的代码执行沙箱平台解决方案

# 项目结构

- 此文件夹为项目根目录。使用 monorepo 管理多个工程，前后端分离。核心服务以 控制节点-执行节点 的形式部署。
- 子工程使用根部 `README.md` 记录概览，并可使用 `docs` 或 `README` 文件夹记录专题说明；如果工作内容涉及对应方面，应当阅读相关 md 文件。
- 根目录的 `docs` 文件夹用于记录跨工程的项目说明，其中 `docs/API.md` 与 `docs/API.zh-CN.md` 为统一 API 参考。
- 本地服务统一使用 `scripts/dev.sh` 编排，所有子命令立即返回且不阻塞终端，不要自行拼装 `go run`、`yarn dev` 或 Worker 启动命令。`console`、`web`、`website` 是默认服务；Worker 是显式启动的可选服务，不加入无参数 `start` 的默认集合。具体用法见 `scripts/README.md`。
- Worker 首次启动时，由 `scripts/dev.sh` 在 Console 就绪后自动调用 Console API 创建凭据，并将本机配置写入被忽略的 `scripts/.dev/<worker>.env`；重复启动应复用仍然有效的 Worker，Console 数据库重建后才重新创建。凭据文件权限必须为 `600`，不得提交到仓库。
- `worker-docker` 的编译、启动、停止、状态与日志统一由 `scripts/dev.sh` 管理。Linux 使用原生进程；macOS 默认由 `dev.sh` 通过 OrbStack Linux VM 运行，开发者不需要手动进入 VM。缺少或未启动 OrbStack 时应明确报错，不自动安装或启动桌面应用。
- 本地公开预览的 Nginx 容器、Worker 和专用 Docker 网络也统一由 `scripts/dev.sh` 管理；`public-preview` 是 `console / worker-docker / nginx` 的组合服务。域名、端口和网络等本机覆盖项写入被忽略的 `scripts/public-preview.env`，渲染后的 Nginx 配置写入 `scripts/.dev/public-preview-nginx.conf`。内部辅助脚本不是开发者入口。

# 项目概述

- **控制节点**：于`console`目录下，Go, Gin。
- **执行节点**：于`worker`目录下，此目录中的不同文件夹表示不同的执行节点实现。
    - `worker-docker`：以 Docker 容器为执行后端
    - `worker-boxlite`：以 boxlite 为执行后端
    - `worker-sys`：以操作系统进程作为执行后端，用于直接控制真实设备
- **前端**：于`web`目录下，Vue, TypeScript, Vite, Pinia, Tailwind CSS。


# 注意事项

- 除非用户主动要求，单次改动只能在单一项目中进行
- `.agents/skills` 文件夹为技能包存放位置，其中包含某一领域的额外文档、脚本等，先探索项目，再决定是否需要读取相关技能
- 所有描述性文字与代码应该始终是面向 开发者/用户 的最终产物，不需要描述中间过程和演变原因。
- 除非用户主动要求，不需要考虑 API/数据库/模式 的向前兼容。
