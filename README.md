# Onlyboxes

面向个人用户的开箱即用沙盒 MCP 服务。  
启动后即可通过 MCP 调用 Python 执行能力（无状态 / 有状态）。
服务端实现基于 Spring Boot + Spring AI MCP Server（WebMVC / Streamable HTTP）。

## TODO
- [ ] 支持自定义镜像
- [ ] 支持自定义容器命令

## 工具说明

| 工具 | 入参 | 出参 |
| --- | --- | --- |
| `pythonExecute` | `code: string` | `ExecResult`：`exitCode`、`stdout`、`stderr`、`errorMessage`、`success` |
| `pythonExecuteStateful` | `name?: string`、`code: string`、`leaseSeconds: long`（必填） | `ExecuteStatefulResponse`：`boxId?`、`destroyed`、`remainingDestroySeconds`、`output`（`ExecResult`） |
| `fetchBlob` | `path: string`、`name: string` | `CallToolResult`：图片返回 `ImageContent(base64 + mimeType)`，其他文件返回 `mimeType` 与 base64 文本 |
| `listActiveContainers` | 无 | `[{ boxId, name, remainingDestroySeconds }]`：仅返回当前 token 的活跃容器 |
| `metrics` | 无 | `RuntimeMetricsView`：`boxesCreatedTotal`、`boxesFailedTotal`、`boxesStoppedTotal`、`numRunningBoxes`、`totalCommandsExecuted`、`totalExecErrors` |

`fetchBlob` 内部实现基于 boxlite `copyOut` 将容器文件拉取到宿主临时目录后读取，不再依赖容器内 Python 文件读取。
`fetchBlob` 不会续租容器，只检查容器是否已过期。
`listActiveContainers` 不会续租容器，只返回当前 token 下尚未过期的容器剩余秒数。

MCP Endpoint（Streamable）：`http://127.0.0.1:8084/mcp`（请求需携带鉴权 header）

## 环境要求

- JDK 25+

## Boxlite 依赖来源

`infra-boxlite` 会解析 `boxlite-java-highlevel-allplatforms`：

- 若 `libs/` 下存在与 `boxliteVersion` 对应的同名 jar，优先使用本地文件。
- 否则自动从 GitHub Release 下载：`https://github.com/<owner>/<repo>/releases/download/v<version>/...jar`。

可通过 Gradle 属性覆盖（`gradle.properties` 或 `-P`）：

- `boxliteVersion`（默认 `0.5.10-coooolfan.1`）
- `boxliteGithubOwner`（默认 `Coooolfan`）
- `boxliteGithubRepo`（默认 `boxlite`）

## 快速开始

```bash
./gradlew :app:bootJar
java -jar app/build/libs/app-all.jar
```

启动后默认监听 `8084` 端口。

## Docker

```bash
./gradlew :app:bootJar
docker build -t onlyboxes:local .
docker run --rm -p 8084:8084 onlyboxes:local
```

CI 会持续产出 `app-all.jar` artifact，并在发布（release published）时推送镜像到 `ghcr.io/<owner>/<repo>`。

## 可选配置(环境变量)

- `SERVER_PORT`：服务端口（默认 `8084`）
- `ONLYBOXES_MIN_LEASE_SECONDS`：租约最小秒数（默认 `30`）
- `ONLYBOXES_MAX_LEASE_SECONDS`：租约最大秒数（默认 `3600`）
- `ONLYBOXES_AUTH_TOKENS`：允许访问 `/mcp` 的 token 列表（逗号分隔；仅允许 `a-z0-9`）
- `ONLYBOXES_AUTH_HEADER_NAME`：客户端传 token 的 header 名（默认 `X-Onlyboxes-Token`）

说明：
- 未配置 `ONLYBOXES_AUTH_TOKENS`（或配置为空）时，默认拒绝全部 `/mcp` 请求。
- `ONLYBOXES_MIN_LEASE_SECONDS` 大于 `ONLYBOXES_MAX_LEASE_SECONDS` 时，服务启动失败。
- `pythonExecuteStateful` 租约语义：
  - `leaseSeconds >= 0`：先收束到 `[min,max]`，并从命令执行完成后开始计时。
  - `leaseSeconds < 0`：命令执行后立即销毁容器，返回 `destroyed=true`、`boxId=null`、`remainingDestroySeconds=0`。
- 同一个容器多次调用时，过期时间只会延长，不会被更短租约缩短。

示例：

```bash
SERVER_PORT=8081 \
ONLYBOXES_MIN_LEASE_SECONDS=30 \
ONLYBOXES_MAX_LEASE_SECONDS=3600 \
ONLYBOXES_AUTH_TOKENS=dev01,prod9 \
java -jar app/build/libs/app-all.jar
```

## 客户端接入示例

以下是通用的 streamable-http 配置示例（字段名按你的 MCP 客户端要求微调）：

```json
{
  "mcpServers": {
    "onlyboxes": {
      "url": "http://127.0.0.1:8084/mcp",
      "headers": {
        "X-Onlyboxes-Token": "dev01"
      }
    }
  }
}
```

如果你通过 `ONLYBOXES_AUTH_HEADER_NAME` 修改了 header 名称，客户端也要同步改成相同的 header。

## 第三方依赖与许可证

本节说明运行时用到的第三方 JAR 依赖（可来自 `libs/` 本地覆盖或 GitHub Release）。

### 依赖关系（核心链路）

`app` -> `infra-boxlite` -> `boxlite-java-highlevel-allplatforms-${boxliteVersion}.jar`

说明：

- `app` 模块依赖 `infra-boxlite` 模块。
- `infra-boxlite` 模块优先从 `libs/` 读取同版本 jar，缺失时回退到 GitHub Release 下载。
- 当前默认版本由 `gradle.properties` 中的 `boxliteVersion` 控制（默认 `0.5.10-coooolfan.1`）。

### 第三方组件清单

| 组件 | 版本 | 本仓库位置 | 上游源码仓库 | 打包仓库 | 许可证 |
| --- | --- | --- | --- | --- | --- |
| boxlite-java-highlevel-allplatforms | `${boxliteVersion}`（默认 `0.5.10-coooolfan.1`） | `libs/boxlite-java-highlevel-allplatforms-${boxliteVersion}.jar`（可选，本地覆盖） | https://github.com/boxlite-ai/boxlite | https://github.com/coooolfan/boxlite | Apache-2.0 |

补充：

- 相较于上游源码仓库，`coooolfan/boxlite` 提供了 Java SDK 及其打包支持。
- 该依赖 JAR 内包含许可证与通知文件（如 `META-INF/LICENSE`、`META-INF/NOTICE`）。
