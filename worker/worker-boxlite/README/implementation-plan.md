# Worker-Boxlite 实施计划：对标 Worker-Docker 完整能力

> **目标**：以 Boxlite 微型 VM 替代 Docker 容器，使用 Rust 实现与 worker-docker 完全对等的四项能力（echo、pythonExec、terminalExec、terminalResource），并接入现有 Onlyboxes 控制台。

---

## 一、现状分析

### 1.1 Worker-Docker 已有能力

| 能力 | 类型 | 会话制 | 容器模型 | 输出限制 | 核心功能 |
|------|------|--------|----------|----------|----------|
| `echo` | 简单回显 | 否 | 无 | 无 | 连通性测试 |
| `pythonExec` | 隔离执行 | 否 | 临时容器 | 按次 | Python 代码沙箱执行 |
| `terminalExec` | 有状态终端 | 是 | 持久容器（租约制） | 1MB/流 | 交互式 Shell 会话 |
| `terminalResource` | 文件操作 | 是 | 复用 terminalExec | 1MB | 会话内文件读取/验证 |

### 1.2 Worker-Boxlite 当前状态

- 仅有 `README/overview.md` 文档
- **无任何源代码实现**
- 无构建系统（Cargo.toml）
- 无测试

### 1.3 Boxlite Rust SDK 可用接口

Boxlite 本身是 Rust 项目，worker-boxlite 直接使用其**原生 Rust SDK**，无需任何 FFI 桥接：

| Rust SDK 接口 | 用途 | 对应 Docker 操作 |
|---------------|------|-----------------|
| `BoxliteRuntime::with_defaults()` | 初始化运行时 | — |
| `runtime.create(BoxOptions{...})` | 创建 VM | `docker create` |
| `box.start()` | 启动 VM | `docker start` |
| `box.run(BoxCommand::new(...))` | 执行命令 | `docker exec` |
| `box.stop()` | 停止 VM | `docker stop` |
| `execution.stdout()` / `stderr()` | 流式读取输出 | 附加模式 stdout/stderr |
| `execution.wait()` | 等待执行完成 | 等待容器退出 |
| `box.copy_in()` / `box.copy_out()` | 文件传输 | `docker cp` |
| `BoxOptions { cpus, memory_mib, env, volumes, .. }` | 资源配置 | `--memory` / `--cpus` 等 |

**关键优势**：
- 直接使用 Rust 原生 SDK，零 FFI 开销
- 与 Boxlite 共享相同的 async 运行时（tokio），天然协作
- 编译时类型安全，避免运行时序列化错误
- Boxlite 是硬件级隔离的微型 VM，无需 Docker daemon，安全隔离更强

---

## 二、架构设计

### 2.1 技术选型

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 实现语言 | **Rust** | Boxlite 原生语言，直接使用 Rust SDK，零桥接开销 |
| 异步运行时 | **tokio** | Boxlite SDK 基于 tokio，gRPC (tonic) 也基于 tokio |
| gRPC 框架 | **tonic + prost** | Rust 生态标准 gRPC 实现 |
| Proto 编译 | **tonic-build** | 构建时自动从 .proto 生成 Rust 代码 |
| 沙箱后端 | **boxlite crate（直接依赖）** | 原生 Rust SDK，async-first |
| 序列化 | **serde + serde_json** | Rust 标准 JSON 处理 |
| 日志 | **tracing + tracing-subscriber** | Rust 生态结构化日志标准 |
| executor_kind | `boxlite` | Hello 帧中的执行器类型标识 |

### 2.2 目录结构

```
worker/worker-boxlite/
├── README/
│   ├── overview.md                     # 已有
│   └── implementation-plan.md          # 本文档
├── Cargo.toml                          # 项目依赖和元数据
├── build.rs                            # Proto 编译脚本
├── src/
│   ├── main.rs                         # 入口：tokio runtime、信号处理、启动
│   ├── config.rs                       # 环境变量配置加载与验证
│   ├── session_client.rs               # gRPC 双向流连接管理
│   ├── heartbeat.rs                    # 心跳循环（发送/接收/超时/重连）
│   ├── capability/
│   │   ├── mod.rs                      # Capability trait 定义 + 命令路由分发
│   │   ├── echo.rs                     # echo 能力
│   │   ├── python_exec.rs             # pythonExec 能力
│   │   ├── terminal_exec.rs           # terminalExec 能力 + SessionManager
│   │   └── terminal_resource.rs       # terminalResource 能力
│   ├── boxlite_runtime.rs             # Boxlite 运行时初始化与生命周期
│   └── proto.rs                        # 生成的 proto 模块引入
└── tests/
    ├── echo_test.rs
    ├── python_exec_test.rs
    ├── terminal_exec_test.rs
    ├── terminal_resource_test.rs
    └── session_client_test.rs
```

### 2.3 Proto 代码生成（新增 Rust 支持）

当前项目仅有 Go 代码生成（`api/scripts/gen-go.sh`）。需要为 Rust 新增：

**方案：build.rs 内联编译**

在 `worker-boxlite/build.rs` 中通过 `tonic-build` 直接从 proto 生成 Rust 代码：

```rust
// build.rs
fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        .build_server(false)    // worker 只需 client
        .build_client(true)
        .compile_protos(
            &["../../api/proto/registry/v1/registry.proto"],
            &["../../api/proto"],
        )?;
    Ok(())
}
```

生成的代码通过 `include!` 或 `tonic::include_proto!` 引入：

```rust
// src/proto.rs
pub mod onlyboxes {
    pub mod registry {
        pub mod v1 {
            tonic::include_proto!("onlyboxes.registry.v1");
        }
    }
}
```

**优势**：无需额外脚本或工具链，`cargo build` 自动完成编译。

### 2.4 依赖清单（Cargo.toml）

```toml
[package]
name = "worker-boxlite"
version = "0.1.0"
edition = "2021"

[dependencies]
# Boxlite 原生 SDK
boxlite = { version = "0.7.5", features = ["gvproxy"] }

# gRPC
tonic = { version = "0.12", features = ["tls"] }
prost = "0.13"
prost-types = "0.13"

# Async
tokio = { version = "1", features = ["full"] }
tokio-stream = "0.1"

# Serialization
serde = { version = "1", features = ["derive"] }
serde_json = "1"

# Logging
tracing = "0.1"
tracing-subscriber = { version = "0.3", features = ["json", "env-filter"] }

# Utils
uuid = { version = "1", features = ["v4"] }
rand = "0.8"
base64 = "0.22"
thiserror = "2"

[build-dependencies]
tonic-build = "0.12"
```

### 2.5 概念映射：Docker → Boxlite（Rust）

| Docker 概念 | Boxlite Rust 对应 | 说明 |
|-------------|-------------------|------|
| `docker create + start` | `runtime.create(opts).await` + `box.start().await` | VM 创建与启动 |
| `docker exec sh -lc <cmd>` | `box.run(BoxCommand::new("sh").arg("-lc").arg(cmd)).await` | 命令执行 |
| `docker start -a`（附加） | `execution.stdout()` + `execution.wait().await` | 异步流式等待结果 |
| `docker rm -f` | `box.stop().await` | 终止 VM |
| Docker 标签 | 内存中 `HashMap` 跟踪 | Boxlite 无标签系统 |
| `--memory 256m --cpus 1.0` | `BoxOptions { cpus: 1, memory_mib: 512, .. }` | 资源限制 |
| Docker 镜像 | OCI 镜像引用 | Boxlite 兼容 OCI 镜像 |
| 容器命名 `onlyboxes-pythonexec-<hex>` | VM 命名 `obx-pythonexec-<hex>` | 命名规则 |

---

## 三、各能力详细实施方案

### 3.1 Echo 能力

**复杂度**：低 | **预计工作量**：0.5 天

纯逻辑实现，无需 Boxlite 交互。

```
请求: {"message": "string"}
响应: {"message": "string"}
错误码: invalid_payload, encode_failed
```

```rust
pub async fn handle_echo(payload: &[u8]) -> Result<Vec<u8>, CapabilityError> {
    let req: EchoRequest = serde_json::from_slice(payload)
        .map_err(|_| CapabilityError::new("invalid_payload", "bad JSON"))?;
    let resp = EchoResponse { message: req.message };
    serde_json::to_vec(&resp)
        .map_err(|_| CapabilityError::new("encode_failed", "JSON encode failed"))
}
```

---

### 3.2 pythonExec 能力

**复杂度**：中 | **预计工作量**：2 天

**请求/响应协议**（与 worker-docker 一致）：
```
请求: {"code": "string"}
响应: {"output": "string", "stderr": "string", "exit_code": 0}
错误码: invalid_payload, execution_failed, encode_failed, deadline_exceeded
```

**实现流程**：

```rust
pub async fn handle_python_exec(
    runtime: &BoxliteRuntime,
    payload: &[u8],
    deadline: Option<Instant>,
) -> Result<Vec<u8>, CapabilityError> {
    let req: PythonExecRequest = serde_json::from_slice(payload)?;

    // 1. 创建临时 VM
    let vm_name = format!("obx-pythonexec-{}", random_hex(8));
    let litebox = runtime.create(BoxOptions {
        image: config.python_exec_image.clone(),  // "python:slim"
        cpus: 1,
        memory_mib: 512,
        ..Default::default()
    }, Some(&vm_name)).await?;

    // 2. 启动 VM
    litebox.start().await?;

    // 3. 执行 Python 代码（带超时）
    let result = async {
        let execution = litebox.run(
            BoxCommand::new("python")
                .arg("-c")
                .arg(&req.code)
        ).await?;

        let stdout = collect_stream(execution.stdout()).await;
        let stderr = collect_stream(execution.stderr()).await;
        let exec_result = execution.wait().await?;

        Ok(PythonExecResponse {
            output: stdout,
            stderr,
            exit_code: exec_result.exit_code,
        })
    };

    // 4. 应用 deadline
    let resp = match deadline {
        Some(dl) => tokio::time::timeout_at(dl, result).await
            .map_err(|_| CapabilityError::new("deadline_exceeded", "timed out"))?,
        None => result.await,
    }?;

    // 5. 清理 VM（defer 等效，无论成功失败）
    // 通过 Drop trait 或显式 stop
    let _ = litebox.stop().await;

    serde_json::to_vec(&resp)
        .map_err(|_| CapabilityError::new("encode_failed", "JSON encode failed"))
}
```

**与 worker-docker 的差异**：
| 方面 | worker-docker (Go) | worker-boxlite (Rust) |
|------|-------------------|----------------------|
| 沙箱创建 | `exec.Command("docker", "create", ...)` | `runtime.create(BoxOptions{...}).await` |
| 输出获取 | bytes.Buffer 同步收集 | `tokio::io::AsyncRead` 异步流 |
| 清理 | `defer runDockerCommand("rm", "-f", ...)` | `Drop` trait 或显式 `litebox.stop().await` |
| 资源限制 | 256MB / 1 CPU / 128 PIDs | 512MiB / 1 CPU（microVM 最低要求） |
| 隔离级别 | 容器级（namespace） | 硬件级（microVM） |
| 错误处理 | `if err != nil` | `Result<T, E>` + `?` 运算符 |

---

### 3.3 terminalExec 能力

**复杂度**：高 | **预计工作量**：4 天

核心能力——有状态终端会话管理。

**请求/响应协议**（与 worker-docker 一致）：
```
请求: {
    "command":           "string (必填)",
    "session_id":        "string (可选)",
    "create_if_missing": false,
    "lease_ttl_sec":     60
}
响应: {
    "session_id":             "string (UUID)",
    "created":                true/false,
    "stdout":                 "string (截断后)",
    "stderr":                 "string (截断后)",
    "exit_code":              0,
    "stdout_truncated":       false,
    "stderr_truncated":       false,
    "lease_expires_unix_ms":  1234567890
}
错误码: session_not_found, session_busy, invalid_payload,
        deadline_exceeded, execution_failed
```

**会话管理器设计**：

```rust
use std::sync::Arc;
use tokio::sync::Mutex;
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};

struct BoxSession {
    id: String,                        // UUID
    litebox: boxlite::LiteBox,         // Boxlite VM 句柄
    busy: AtomicBool,                  // 并发锁
    lease_expiry: Mutex<Instant>,      // 租约到期时间
    created_at: Instant,
}

pub struct SessionManager {
    sessions: Arc<Mutex<HashMap<String, Arc<BoxSession>>>>,
    runtime: Arc<BoxliteRuntime>,
    config: TerminalConfig,
}

impl SessionManager {
    /// 创建新会话
    async fn create_session(&self) -> Result<Arc<BoxSession>, CapabilityError> {
        let session_id = Uuid::new_v4().to_string();
        let vm_name = format!("obx-terminalexec-{}", random_hex(8));

        let litebox = self.runtime.create(BoxOptions {
            image: self.config.terminal_exec_image.clone(),
            cpus: 1,
            memory_mib: 512,
            ..Default::default()
        }, Some(&vm_name)).await?;

        litebox.start().await?;
        // Boxlite VM 启动后天然保持运行，无需保活进程

        let session = Arc::new(BoxSession {
            id: session_id.clone(),
            litebox,
            busy: AtomicBool::new(false),
            lease_expiry: Mutex::new(Instant::now() + Duration::from_secs(self.config.lease_default_sec)),
            created_at: Instant::now(),
        });

        self.sessions.lock().await.insert(session_id, session.clone());
        Ok(session)
    }

    /// 获取或创建会话
    async fn get_or_create(
        &self,
        session_id: Option<&str>,
        create_if_missing: bool,
    ) -> Result<(Arc<BoxSession>, bool), CapabilityError> { ... }

    /// Janitor：清理过期会话（每 5 秒）
    async fn janitor_loop(&self, mut shutdown: tokio::sync::watch::Receiver<bool>) {
        let mut interval = tokio::time::interval(Duration::from_secs(5));
        loop {
            tokio::select! {
                _ = interval.tick() => {
                    let mut sessions = self.sessions.lock().await;
                    let now = Instant::now();
                    let expired: Vec<String> = sessions.iter()
                        .filter(|(_, s)| !s.busy.load(Ordering::Acquire)
                                      && *s.lease_expiry.blocking_lock() < now)
                        .map(|(id, _)| id.clone())
                        .collect();
                    for id in expired {
                        if let Some(session) = sessions.remove(&id) {
                            let _ = session.litebox.stop().await;
                        }
                    }
                }
                _ = shutdown.changed() => break,
            }
        }
    }

    /// 关闭所有会话
    async fn shutdown_all(&self) {
        let mut sessions = self.sessions.lock().await;
        for (_, session) in sessions.drain() {
            let _ = session.litebox.stop().await;
        }
    }
}
```

**并发控制**：
```rust
// CAS 获取会话独占权
if !session.busy.compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire).is_ok() {
    return Err(CapabilityError::new("session_busy", "session is executing another command"));
}
// 执行完毕释放
defer! { session.busy.store(false, Ordering::Release); }
```

**租约管理**：
- 租约延长单调递增：新 TTL 只在延长到期时间时生效
- 范围：`WORKER_TERMINAL_LEASE_MIN_SEC` (60) ~ `WORKER_TERMINAL_LEASE_MAX_SEC` (1800)

**清理机制**：

| 触发条件 | 行为 |
|----------|------|
| 租约到期（janitor） | `litebox.stop().await` + 移除会话 |
| 命令超时/取消 | `execution.kill()` + `litebox.stop().await` + 移除会话 |
| Worker 优雅关闭 (SIGINT/SIGTERM) | `shutdown_all()` 遍历清理 |
| Worker 崩溃 (SIGKILL) | Boxlite 无守护进程，宿主进程终止后 VM 自然回收 |

**Boxlite 优势**：Docker 的 `docker rm -f` 在进程崩溃时无法保证清理，而 Boxlite 无守护进程，宿主进程终止后 VM 自然回收。

---

### 3.4 terminalResource 能力

**复杂度**：中 | **预计工作量**：2 天

**请求/响应协议**（与 worker-docker 一致）：
```
请求: {
    "session_id": "string (必填)",
    "file_path":  "string (必填)",
    "action":     "validate|read"   // 默认 validate
}
响应 (validate): { "session_id", "file_path", "mime_type", "size_bytes" }
响应 (read):     { "session_id", "file_path", "mime_type", "size_bytes", "blob": "base64" }

领域错误码: file_not_found, path_is_directory, file_too_large
通用错误码: invalid_payload, session_not_found, session_busy,
           deadline_exceeded, execution_failed
```

**实现方式**：

与 worker-docker 相同，通过在 VM 内执行 Python 探测脚本实现：

```rust
const PROBE_SCRIPT: &str = r#"
import os, sys, json, mimetypes, base64
# ... (复用 worker-docker 的探测脚本)
"#;

pub async fn handle_terminal_resource(
    session_mgr: &SessionManager,
    payload: &[u8],
) -> Result<Vec<u8>, CapabilityError> {
    let req: TerminalResourceRequest = serde_json::from_slice(payload)?;
    let session = session_mgr.get(&req.session_id)?;

    // CAS 获取独占权
    acquire_busy(&session)?;
    let _guard = scopeguard::guard((), |_| {
        session.busy.store(false, Ordering::Release);
    });

    let action = req.action.as_deref().unwrap_or("validate");
    let execution = session.litebox.run(
        BoxCommand::new("python3")
            .arg("-c")
            .arg(PROBE_SCRIPT)
            .arg("--action").arg(action)
            .arg("--file-path").arg(&req.file_path)
            .arg("--max-read-bytes").arg(config.output_limit_bytes.to_string())
    ).await?;

    let stdout = collect_stream(execution.stdout()).await;
    let result = execution.wait().await?;

    match result.exit_code {
        0  => { /* 解析 stdout JSON 返回 */ }
        10 => Err(CapabilityError::new("file_not_found", "...")),
        11 => Err(CapabilityError::new("path_is_directory", "...")),
        12 => Err(CapabilityError::new("file_too_large", "...")),
        _  => Err(CapabilityError::new("execution_failed", "...")),
    }
}
```

---

## 四、gRPC 协议与连接管理

### 4.1 连接管理（tonic 客户端）

```rust
use tonic::transport::Channel;
use proto::worker_registry_service_client::WorkerRegistryServiceClient;

pub struct SessionClient {
    config: Config,
    session_id: Option<String>,
}

impl SessionClient {
    pub async fn connect_and_run(
        &mut self,
        capability_executor: Arc<CapabilityExecutor>,
        mut shutdown: tokio::sync::watch::Receiver<bool>,
    ) -> Result<(), Error> {
        loop {
            match self.run_session(&capability_executor, &mut shutdown).await {
                Ok(()) => break,  // 正常关闭
                Err(e) => {
                    tracing::warn!(error = %e, "session disconnected, reconnecting...");
                    self.exponential_backoff().await;  // 1s → 2s → 4s → ... → 15s
                }
            }
        }
        Ok(())
    }

    async fn run_session(&mut self, executor: &CapabilityExecutor, shutdown: &mut watch::Receiver<bool>) -> Result<(), Error> {
        // 1. 建立 gRPC 双向流
        let channel = self.build_channel().await?;
        let mut client = WorkerRegistryServiceClient::new(channel);
        let (tx, rx) = tokio::sync::mpsc::channel(32);
        let response = client.connect(ReceiverStream::new(rx)).await?;
        let mut inbound = response.into_inner();

        // 2. 发送 Hello
        tx.send(self.build_hello()).await?;

        // 3. 接收 ConnectAck
        let ack = inbound.message().await?;
        self.session_id = Some(ack.session_id);

        // 4. 启动心跳 + 命令接收循环
        let heartbeat_handle = tokio::spawn(self.heartbeat_loop(tx.clone()));
        self.receive_loop(inbound, tx, executor, shutdown).await?;

        heartbeat_handle.abort();
        Ok(())
    }
}
```

### 4.2 Hello 帧

```rust
fn build_hello(&self) -> ConnectRequest {
    ConnectRequest {
        payload: Some(connect_request::Payload::Hello(ConnectHello {
            node_id: self.config.worker_id.clone(),
            node_name: self.config.node_name.clone(),
            executor_kind: "boxlite".to_string(),
            labels: self.config.labels.clone(),
            version: self.config.version.clone(),
            worker_secret: self.config.worker_secret.clone(),
            capabilities: vec![
                CapabilityDeclaration { name: "echo".into(),             max_inflight: 4 },
                CapabilityDeclaration { name: "pythonExec".into(),       max_inflight: 4 },
                CapabilityDeclaration { name: "terminalExec".into(),     max_inflight: 4 },
                CapabilityDeclaration { name: "terminalResource".into(), max_inflight: 4 },
            ],
        })),
    }
}
```

### 4.3 心跳与重连

- 默认 5 秒间隔 + 20% 抖动
- 容忍 1 次心跳超时，第 2 次触发重连
- 指数退避重连：1s → 2s → 4s → ... → 15s（上限）

### 4.4 命令路由

```rust
pub struct CapabilityExecutor {
    runtime: Arc<BoxliteRuntime>,
    session_mgr: Arc<SessionManager>,
    config: Config,
}

impl CapabilityExecutor {
    pub async fn execute(&self, dispatch: CommandDispatch) -> CommandResult {
        let deadline = dispatch.deadline_unix_ms
            .map(|ms| Instant::now() + Duration::from_millis(ms as u64 - now_unix_ms()));

        let result = match dispatch.capability.to_lowercase().as_str() {
            "echo"             => handle_echo(&dispatch.payload_json).await,
            "pythonexec"       => handle_python_exec(&self.runtime, &dispatch.payload_json, deadline).await,
            "terminalexec"     => handle_terminal_exec(&self.session_mgr, &dispatch.payload_json, deadline).await,
            "terminalresource" => handle_terminal_resource(&self.session_mgr, &dispatch.payload_json).await,
            other              => Err(CapabilityError::new("unsupported_capability", &format!("unknown: {other}"))),
        };

        match result {
            Ok(payload) => CommandResult {
                command_id: dispatch.command_id,
                payload_json: payload,
                error: None,
                completed_unix_ms: now_unix_ms(),
            },
            Err(e) => CommandResult {
                command_id: dispatch.command_id,
                payload_json: vec![],
                error: Some(CommandError { code: e.code, message: e.message }),
                completed_unix_ms: now_unix_ms(),
            },
        }
    }
}
```

---

## 五、配置体系

### 5.1 环境变量清单

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| **身份认证** | | |
| `WORKER_ID` | *(必填)* | Worker 唯一标识 |
| `WORKER_SECRET` | *(必填)* | 认证密钥 |
| `WORKER_NODE_NAME` | `worker-boxlite-<ID前8位>` | 节点名称 |
| **控制台连接** | | |
| `WORKER_CONSOLE_GRPC_TARGET` | `127.0.0.1:50051` | gRPC 地址 |
| `WORKER_CONSOLE_INSECURE` | `false` | 是否明文连接 |
| **心跳** | | |
| `WORKER_HEARTBEAT_INTERVAL_SEC` | `5` | 心跳间隔 |
| `WORKER_HEARTBEAT_JITTER_PCT` | `20` | 抖动百分比 |
| `WORKER_CALL_TIMEOUT_SEC` | `ceil(2.5 * heartbeat)` | 心跳 ACK 超时 |
| **Boxlite 配置** | | |
| `WORKER_PYTHON_EXEC_BOXLITE_IMAGE` | `python:slim` | pythonExec 使用的 OCI 镜像 |
| `WORKER_PYTHON_EXEC_MEMORY_MIB` | `256` | pythonExec VM 内存（MiB） |
| `WORKER_PYTHON_EXEC_CPUS` | `1` | pythonExec VM CPU 数 |
| `WORKER_PYTHON_EXEC_MAX_PROCESSES` | `128` | pythonExec VM 最大进程数 |
| `WORKER_TERMINAL_EXEC_BOXLITE_IMAGE` | `coolfan1024/onlyboxes-default-worker:0.0.5` | terminalExec 使用的 OCI 镜像 |
| `WORKER_TERMINAL_EXEC_MEMORY_MIB` | `256` | terminalExec VM 内存（MiB） |
| `WORKER_TERMINAL_EXEC_CPUS` | `1` | terminalExec VM CPU 数 |
| `WORKER_TERMINAL_EXEC_MAX_PROCESSES` | `128` | terminalExec VM 最大进程数 |
| **终端会话** | | |
| `WORKER_TERMINAL_LEASE_MIN_SEC` | `60` | 最小租约 |
| `WORKER_TERMINAL_LEASE_MAX_SEC` | `1800` | 最大租约 |
| `WORKER_TERMINAL_LEASE_DEFAULT_SEC` | `60` | 默认租约 |
| `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` | `1048576` | 输出截断限制（1MB） |
| **日志** | | |
| `WORKER_LOG_LEVEL` | `info` | 日志级别（通过 `RUST_LOG` 或此变量） |
| `WORKER_LOG_FORMAT` | `json` | `json` 或 `text` |
| **标签** | | |
| `WORKER_LABELS` | *(空)* | CSV 标签 `key=value,key=value` |

---

## 六、Console 侧适配

worker-boxlite 使用与 worker-docker 相同的 proto 定义和 capability 名称，**Console 无需任何代码修改**。

Console 通过以下方式自动兼容：
1. `executor_kind = "boxlite"`（仅存储在 DB 中，不影响路由逻辑）
2. Capability 名称一致（echo、pythonExec、terminalExec、terminalResource）
3. 负载均衡：按 capability 名称 round-robin，与 executor_kind 无关

**可选增强**（非阻塞）：
- 在 `store_helpers.go` 中新增 `WorkerTypeBoxlite` 常量
- Dashboard 新增 Boxlite 图标

---

## 七、测试策略

### 7.1 单元测试

```rust
// 可替换的 trait 抽象，便于 mock
#[async_trait]
pub trait BoxRuntime: Send + Sync {
    async fn create(&self, opts: BoxOptions, name: Option<&str>) -> Result<Box<dyn LiteBoxHandle>>;
}

#[async_trait]
pub trait LiteBoxHandle: Send + Sync {
    async fn start(&self) -> Result<()>;
    async fn run(&self, cmd: BoxCommand) -> Result<Box<dyn ExecutionHandle>>;
    async fn stop(&self) -> Result<()>;
}

// 测试中使用 mock 实现
struct MockRuntime { ... }
struct MockLiteBox { ... }
```

### 7.2 测试对标清单

确保以下 worker-docker 测试场景全部覆盖：

- [ ] Echo 请求/响应往返
- [ ] Echo 无效 payload 处理
- [ ] pythonExec 正常执行 + 输出
- [ ] pythonExec 非零退出码
- [ ] pythonExec 超时强制清理
- [ ] terminalExec 新建会话
- [ ] terminalExec 复用已有会话
- [ ] terminalExec session_not_found 错误
- [ ] terminalExec create_if_missing 行为
- [ ] terminalExec session_busy 并发拒绝
- [ ] terminalExec 超时后销毁会话
- [ ] terminalExec 输出截断（stdout/stderr 各自）
- [ ] terminalExec 租约续期（单调递增）
- [ ] terminalExec janitor 清理过期会话
- [ ] terminalResource validate 操作
- [ ] terminalResource read 操作 + base64
- [ ] terminalResource file_not_found
- [ ] terminalResource path_is_directory
- [ ] terminalResource file_too_large
- [ ] terminalResource session_not_found / session_busy
- [ ] gRPC 握手流程（Fake gRPC Server via tonic）
- [ ] 心跳容忍（单次超时恢复）
- [ ] 心跳失败（双次超时触发重连）
- [ ] 优雅关闭清理所有 VM

### 7.3 集成测试

```rust
#[cfg(feature = "integration")]
#[tokio::test]
async fn test_python_exec_e2e() {
    // 需要本地 Boxlite 运行时可用
    let runtime = BoxliteRuntime::with_defaults().await
        .expect("Boxlite runtime required for integration tests");
    // ...
}
```

---

## 八、分阶段实施路线

### Phase 0：项目脚手架 + Proto 编译（1.5 天）

**目标**：`cargo build` 可编译通过，proto 生成成功

- [ ] 初始化 `Cargo.toml`，引入所有依赖
- [ ] 编写 `build.rs`，配置 tonic-build 从 proto 生成 Rust 代码
- [ ] 编写 `src/proto.rs`，引入生成的模块
- [ ] 实现 `src/config.rs`（环境变量解析、验证、默认值）
- [ ] 实现 `src/main.rs` 骨架（tokio runtime、信号处理、tracing 初始化）
- [ ] 验证 Boxlite crate 可作为依赖引入并编译

**交付物**：`cargo build --release` 编译通过

---

### Phase 1：gRPC 连接 + Echo（2.5 天）

**目标**：Worker 能连接 Console 并响应 echo 命令

- [ ] 实现 `src/session_client.rs`（tonic 双向流管理）
- [ ] 实现 `src/heartbeat.rs`（心跳发送/接收/超时/重连）
- [ ] 实现 `src/capability/mod.rs`（Capability trait + 路由框架）
- [ ] 实现 `src/capability/echo.rs`
- [ ] 集成到 `main.rs`（启动连接循环、优雅关闭）
- [ ] 编写 session_client 测试（tonic mock server）

**验证标准**：
1. Worker 启动后成功连接 Console
2. Console Dashboard 显示 worker 在线（executor_kind=boxlite）
3. Echo 命令端到端成功

---

### Phase 2：pythonExec（2 天）

**目标**：支持隔离 Python 代码执行

- [ ] 实现 `src/boxlite_runtime.rs`（BoxliteRuntime 初始化、单例管理）
- [ ] 实现 `src/capability/python_exec.rs`
  - 临时 VM 创建/启动/执行/清理
  - 异步流式输出收集
  - 超时处理（`tokio::time::timeout`）
  - 确保 VM 清理（scopeguard 或手动 finally 模式）
- [ ] 编写单元测试（Mock BoxRuntime）
- [ ] 编写集成测试

**验证标准**：
1. `pythonExec` 能正确执行 Python 代码并返回 stdout/stderr/exit_code
2. 超时能正确终止 VM
3. VM 每次执行后被清理

---

### Phase 3：terminalExec（4 天）

**目标**：完整的有状态终端会话管理

- [ ] 实现 `SessionManager`
  - 会话创建（VM + UUID）
  - 会话查找/复用
  - `AtomicBool` 并发控制
  - 租约管理（创建/续期/到期）
- [ ] 实现 terminalExec 命令处理
  - 无 session_id → 创建
  - 有 session_id → 复用
  - create_if_missing 逻辑
  - 输出截断
- [ ] 实现 Janitor（`tokio::time::interval` 5 秒扫描）
- [ ] 实现 shutdown_all（SIGINT/SIGTERM 处理）
- [ ] 编写全套单元测试
- [ ] 编写并发测试（`tokio::spawn` 多任务竞争）

**验证标准**：
1. 会话状态跨命令保持
2. 并发请求正确返回 session_busy
3. 过期会话被自动清理
4. Worker 关闭时所有 VM 被清理

---

### Phase 4：terminalResource（2 天）

**目标**：支持会话内文件读取和验证

- [ ] 移植 Python 探测脚本（内联为 `const PROBE_SCRIPT: &str`）
- [ ] 实现 `src/capability/terminal_resource.rs`
  - validate / read 操作
  - 域错误处理
  - 复用 SessionManager 并发控制
- [ ] 编写单元测试

**验证标准**：
1. validate 返回文件元数据
2. read 返回 base64 编码内容
3. 域错误码正确
4. 大文件被拒绝

---

### Phase 5：端到端验证与加固（2 天）

**目标**：生产就绪

- [ ] 端到端：Console HTTP API / MCP → worker-boxlite 完整流程
- [ ] 压力测试：max_inflight=4 并发
- [ ] 资源泄漏检测（长时间运行无 VM 残留）
- [ ] 日志隐私审查（不泄露命令内容/代码）
- [ ] 断连恢复验证
- [ ] 文档更新

---

## 九、风险与缓解策略

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| Boxlite Rust crate API 不稳定 | 编译失败或行为变化 | 锁定 git commit 或版本；与 Boxlite 团队协调 |
| Boxlite crate 依赖树复杂 | 编译时间长 | 使用 `cargo build --release` 缓存；CI 分层构建 |
| tonic 与 Boxlite 的 tokio 版本冲突 | 编译失败 | 统一 tokio 版本；必要时用 workspace 管理 |
| VM 启动延迟高于 Docker | pythonExec 性能下降 | 可后续引入 VM 池预热（不影响 MVP） |
| VM 内存最低 512MiB | 资源消耗增加 | 接受差异，安全隔离优势弥补成本 |
| 平台限制（需要 KVM/Hypervisor.framework） | 部署范围受限 | 初始支持 macOS ARM64 + Linux x86_64/ARM64 |

---

## 十、里程碑时间线（总计约 14 天）

```
Week 1:
  Day 1-1.5  → Phase 0: 项目脚手架 + Proto 编译 ✓
  Day 2-4    → Phase 1: gRPC 连接 + Echo ✓
  Day 5-6    → Phase 2: pythonExec ✓

Week 2:
  Day 7-10   → Phase 3: terminalExec ✓
  Day 11-12  → Phase 4: terminalResource ✓

Week 3:
  Day 13-14  → Phase 5: 端到端验证与加固 ✓
```

**前置条件**（开始 Phase 0 之前）：
1. Cargo 可从 crates.io 下载 `boxlite 0.7.5` 及其依赖
2. Rust 工具链已安装（rustup，stable 1.75+）
3. 系统满足 Boxlite 运行要求（macOS Hypervisor.framework 或 Linux KVM）
4. 确认 `boxlite` crate 可独立编译

---

## 十一、验收标准

worker-boxlite 视为完成的条件：

1. **功能对等**：echo、pythonExec、terminalExec、terminalResource 四项能力行为与 worker-docker 一致
2. **协议兼容**：使用相同 proto 定义，Console 无需修改即可调度任务至 boxlite worker
3. **错误码一致**：所有错误码和错误消息格式与 worker-docker 相同
4. **测试覆盖**：第七节中的测试对标清单全部通过
5. **零泄漏**：长时间运行后无 VM 残留
6. **文档完整**：README 包含部署说明、配置参考、架构概述
