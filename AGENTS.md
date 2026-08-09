Onlyboxes 是一个面向个人与小型团队的代码执行沙箱平台解决方案

# 项目结构

- 此文件夹为项目根目录。使用 monorepo 管理多个工程，前后端分离。核心服务以 控制节点-执行节点 的形式部署。
- 子工程使用根部 `README.md` 记录概览，并可使用 `docs` 或 `README` 文件夹记录专题说明；如果工作内容涉及对应方面，应当阅读相关 md 文件。
- 根目录的 `docs` 文件夹用于记录跨工程的项目说明，其中 `docs/API.md` 与 `docs/API.zh-CN.md` 为统一 API 参考。
- 本地启动服务统一使用`scripts/dev.sh`（tmux 编排 console / web / website，所有子命令立即返回，不阻塞终端），不要自行拼装`go run`或`yarn dev`。用法见`scripts/README.md`。worker 不在编排范围内，需手动启动。

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
