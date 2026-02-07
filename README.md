# Onlyboxes

面向个人用户的开箱即用沙盒 MCP 服务。  
启动后即可通过 MCP 调用 Python 执行能力（无状态 / 有状态）。

## TODO
- [ ] 支持自定义镜像
- [ ] 支持自定义容器命令
- [ ] 支持直接读取文件

## 工具说明

| 工具 | 入参 | 出参 |
| --- | --- | --- |
| `pythonExecute` | `code: string` | `ExecResult`：`exitCode`、`stdout`、`stderr`、`errorMessage`、`success` |
| `pythonExecuteStateful` | `name?: string`、`code: string`、`leaseSeconds?: long`（不传时默认 `30` 秒） | `ExecuteStatefulResponse`：`boxId`、`output`（`ExecResult`） |
| `metrics` | 无 | `RuntimeMetricsView`：`boxesCreatedTotal`、`boxesFailedTotal`、`boxesStoppedTotal`、`numRunningBoxes`、`totalCommandsExecuted`、`totalExecErrors` |

MCP Endpoint（Streamable）：`http://127.0.0.1:8080/mcp`

## 环境要求

- JDK 25+

## 快速开始

```bash
./gradlew :app:assemble
java -jar app/build/libs/app-all.jar
```

启动后默认监听 `8080` 端口。

## 可选配置

- `SERVER_PORT`：服务端口（默认 `8080`）
- `ONLYBOXES_DEFAULT_LEASE_SECONDS`：默认租约秒数（默认 `30`）

示例：

```bash
SERVER_PORT=8081 ONLYBOXES_DEFAULT_LEASE_SECONDS=600 java -jar app/build/libs/app-all.jar
```

## 客户端接入示例

以下是通用的 streamable-http 配置示例（字段名按你的 MCP 客户端要求微调）：

```json
{
  "mcpServers": {
    "onlyboxes": {
      "url": "http://127.0.0.1:8080/mcp"
    }
  }
}
```

## 第三方依赖与许可证

本节仅列出本仓库手工放入 `libs/` 的第三方 JAR 依赖。

### 依赖关系（核心链路）

`app` -> `infra-boxlite` -> `libs/boxlite-java-highlevel-allplatforms-0.5.9.jar`

说明：

- `app` 模块依赖 `infra-boxlite` 模块。
- `infra-boxlite` 模块通过 `fileTree` 从 `libs/` 加载 `*.jar`。
- 当前运行时使用的核心第三方组件是 `boxlite-java-highlevel-allplatforms-0.5.9.jar`。

### 第三方组件清单

| 组件 | 版本 | 本仓库位置 | 上游源码仓库 | 打包仓库 | 许可证 |
| --- | --- | --- | --- | --- | --- |
| boxlite-java-highlevel-allplatforms | 0.5.9 | `libs/boxlite-java-highlevel-allplatforms-0.5.9.jar` | https://github.com/boxlite-ai/boxlite | https://github.com/coooolfan/boxlite | Apache-2.0 |

补充：

- 相较于上游源码仓库，`coooolfan/boxlite` 提供了 Java SDK 及其打包支持。
- 该依赖 JAR 内包含许可证与通知文件（如 `META-INF/LICENSE`、`META-INF/NOTICE`）。
