Onlyboxes 是一个面向个人与小型团队的代码执行沙箱平台解决方案

# 项目结构

- 此文件夹为项目根目录。使用 monorepo 管理多个工程，前后端分离。核心服务以 控制节点-执行节点 的形式部署。
- 所有子工程都有各自的`README`文件夹，每个 md 文件代表某个方面的说明，如果工作内容涉及对应方面，应当阅读对应 md 文件。

# 项目概述

- **控制节点**：于`console`目录下，Go, Gin。
- **执行节点**：于`worker`目录下，此目录中的不同文件夹表示不同的执行节点实现。
    - `worker-docker`：以 Docker 容器为执行后端
- **前端**：于`web`目录下，Vue, TypeScript, Vite, Pinia, Tailwind CSS。


# 注意事项

- 除非用户主动要求，单次改动只能在单一项目中进行
- `./skills` 文件夹为技能包存放位置，其中包含某一领域的额外文档、脚本等，先探索项目，再决定是否需要读取相关技能
  - `./skills/skill-creator` 创建技能包：Guide for creating effective skills. This skill should be used when users want to create a new skill (or update an existing skill) that extends Claude's capabilities with specialized knowledge, workflows, or tool integrations.
- yarn --cwd <project> <command> 用于指定工作路径
- 所有描述性文字与代码应该始终是面向 开发者/用户 的最终产物，不需要描述中间过程和演变原因。
- 除非用户主动要求，不需要考虑 API/数据库/模式 的向前兼容。
