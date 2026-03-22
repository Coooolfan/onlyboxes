# Worker-Boxlite

Onlyboxes worker 的 Boxlite 实现。使用 [Boxlite](https://github.com/boxlite-ai/boxlite) 微型 VM 替代 Docker 容器，提供硬件级隔离的沙箱执行环境。

与 worker-docker 功能完全对等，支持四项能力：`echo`、`pythonExec`、`terminalExec`、`terminalResource`。

## 前置条件

- **Rust 工具链**：1.88+（`rustup` 安装）
- **Boxlite 源码**：克隆到 `~/Documents/code/boxlite`
- **系统要求**：macOS（Hypervisor.framework）或 Linux（KVM）
- **protoc**：Protocol Buffers 编译器

## 构建

```bash
cd worker/worker-boxlite
cargo build --release
```

## 运行

```bash
export WORKER_ID="my-worker-01"
export WORKER_SECRET="your-secret"
export WORKER_CONSOLE_GRPC_TARGET="127.0.0.1:50051"
export WORKER_CONSOLE_INSECURE=true

cargo run --release
```

## 测试

```bash
# 单元测试
cargo test

# 集成测试（需要 Boxlite 运行时，自动从 GitHub Releases 下载预编译二进制）
BOXLITE_DEPS_STUB=2 cargo test --features integration
```

## 配置

所有配置通过环境变量提供。

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `WORKER_ID` | *(必填)* | Worker 唯一标识 |
| `WORKER_SECRET` | *(必填)* | 认证密钥 |
| `WORKER_VERSION` | `dev` | 版本号 |
| `WORKER_NODE_NAME` | `worker-boxlite-{ID前8位}` | 节点名称 |
| `WORKER_CONSOLE_GRPC_TARGET` | `127.0.0.1:50051` | Console gRPC 地址 |
| `WORKER_CONSOLE_INSECURE` | `false` | 是否明文连接（不使用 TLS） |
| `WORKER_HEARTBEAT_INTERVAL_SEC` | `5` | 心跳间隔（秒） |
| `WORKER_HEARTBEAT_JITTER_PCT` | `20` | 心跳抖动百分比 |
| `WORKER_CALL_TIMEOUT_SEC` | `ceil(2.5 * heartbeat)` | RPC 超时 |
| `WORKER_PYTHON_EXEC_IMAGE` | `python:slim` | pythonExec 使用的 OCI 镜像 |
| `WORKER_TERMINAL_EXEC_IMAGE` | `coolfan1024/onlyboxes-default-worker:0.0.3` | terminalExec 使用的 OCI 镜像 |
| `WORKER_BOXLITE_DEFAULT_CPUS` | `1` | VM 默认 CPU 数 |
| `WORKER_BOXLITE_DEFAULT_MEMORY_MIB` | `512` | VM 默认内存（MiB） |
| `WORKER_TERMINAL_LEASE_MIN_SEC` | `60` | 最小租约（秒） |
| `WORKER_TERMINAL_LEASE_MAX_SEC` | `1800` | 最大租约（秒） |
| `WORKER_TERMINAL_LEASE_DEFAULT_SEC` | `60` | 默认租约（秒） |
| `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` | `1048576` | 输出截断限制（1MB） |
| `WORKER_LOG_LEVEL` | `info` | 日志级别（debug/info/warn/error） |
| `WORKER_LOG_FORMAT` | `json` | 日志格式（json/text） |
| `WORKER_LABELS` | *(空)* | CSV 标签 `key=value,key=value` |

## 与 worker-docker 的差异

| 方面 | worker-docker | worker-boxlite |
|------|--------------|----------------|
| 实现语言 | Go | Rust |
| 沙箱技术 | Docker 容器（namespace 隔离） | Boxlite 微型 VM（硬件级隔离） |
| 守护进程 | 需要 Docker daemon | 无守护进程 |
| executor_kind | `docker` | `boxlite` |
| 资源默认值 | 256MB / 1 CPU / 128 PIDs | 512MiB / 1 CPU |
| 崩溃清理 | 依赖 Docker daemon | VM 随宿主进程自动回收 |

Console 无需任何修改即可调度任务至 boxlite worker。
