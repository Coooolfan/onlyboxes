# worker-sys-macos：Swift 原生实现方案与难度

> 用 Swift 把 worker-sys 实现为常驻 **状态栏（MenuBarExtra）** 的 macOS App，配置走 GUI + Keychain（不再用一堆 `WORKER_*` 环境变量），最终产出 **DMG** 直接分发（**不上架 App Store**）。
>
> 难度图例：🟢 低 ・ 🟡 中 ・ 🔴 高。

## 0. 范围裁剪（相对 Go 版）

**保留：**
- gRPC bidi `Connect` 连接、hello、心跳、指数退避重连、`FailedPrecondition`（session 抢占）处理。
- `computerUse`：执行任意 `/bin/sh -lc <cmd>`，输出按字节截断，deadline 超时。
- whitelist 模式：**仅 `exact` 和 `allow_all`**。

**砍掉：**
- ❌ `readImage` 的**实现**（TOCTOU/symlink/`SameFile` 绑定、MIME 嗅探、`export`）—— 原 Go 版唯一的高风险模块，去掉后整体已无高风险代码。
- ❌ whitelist 的 `prefix` 模式。
- ❌ Windows PowerShell 分支（macOS 专用，本就不需要）。

⚠️ **但 readImage 的能力声明必须保留**。console 强制校验（`console/internal/grpcserver/service_connect.go:160-162`，测试 `TestConnectRejectsWorkerSysMissingReadImageCapability` 守护）：worker-sys 的 hello 必须**同时声明** `computerUse` 和 `readImage`，否则 `PermissionDenied` 拒绝连接。

因此采用 **方案 A**：hello 仍声明两个能力（各 max_inflight=1）骗过注册校验，但收到 `readImage` dispatch 时直接返回 `unsupported` 错误 `CommandResult`，不实现真实读图逻辑。这样既满足 console 契约，又砍掉全部高风险代码、不改共享的 console。
（方案 B = 改 console 放宽校验，波及 Go worker 契约，不推荐。）

## 1. 待移植行为清单（裁剪后）

| 来源（Go） | 行为 | 备注 |
|---|---|---|
| `runner.go` | 指数退避重连 1s→15s；`FailedPrecondition` 重置退避 | 状态机核心 |
| `session_client.go` | bidi stream；hello→connect_ack→心跳；连续 2 次 ack 超时才重连；并发槽 max_inflight=1，满槽回 `session_busy`；心跳抖动；间隔由服务端 ack 动态下发 | gRPC + 并发 |
| `hello_builder.go` | 只声明 `computerUse`；node_name 缺省派生 | |
| `computer_use.go` | `/bin/sh -lc`；whitelist(exact/allow_all)；输出按字节截断；deadline kill | |
| `config.go` | `WORKER_*` env → GUI/UserDefaults/Keychain | |
| `logging.go`/`buildinfo.go` | 日志 + 版本注入 | |

## 2. 逐模块难度

### 2.1 proto / gRPC 代码生成 — 🟡 中
- 协议 `api/proto/registry/v1/registry.proto` 极简（oneof + 标量 + map + bytes）。
- 工具链：`swift-protobuf`（消息）+ `grpc-swift 2`（bidi 流），SwiftPM build plugin 生成或 `protoc` 预生成。
- bidi `Connect(stream)↔stream` 在 grpc-swift 2 用 async/await + `RPCWriter`/`RPCAsyncSequence` 表达，能力没问题。
- 风险：grpc-swift 2 的最低 macOS/Swift 版本要求需核实，与 MenuBarExtra(macOS 13+) 取交集；proto 双份维护（Go+Swift）进 CI 防漂移。
- **这是全路线唯一真正的"未知数"，建议 M1 先打通。**

### 2.2 连接 / 心跳 / 重连状态机 — 🟢🟡
- async/await + `Task` 直译。还原细节：容忍 1 次 ack 超时（连续 2 次才断）、`FailedPrecondition` 重置退避、`session_busy` 槽位、心跳间隔动态下发。
- gRPC 错误码：Swift 侧从 `RPCError.code` 取等价于 `FailedPrecondition` 的判断。

### 2.3 computerUse — 🟢
- `Process`：`/bin/sh -lc <cmd>`，stdout/stderr 各接 `Pipe`。
- 输出按字节截断：用 `Data` 前缀 N 字节，最后 lossy 转 String，对齐 Go 语义。
- deadline 超时：`Task` 超时后 `terminate()`（必要时 SIGKILL）。
- whitelist：只剩 `exact`（字符串相等）和 `allow_all`，`isCommandAllowed` 极简。

### 2.4 配置与密钥 — 🟢（体验提升点）
- SwiftUI 设置窗 + `UserDefaults`；`WORKER_SECRET` 存 **Keychain**（`kSecClassGenericPassword`）。
- 直接满足"不走一堆环境变量"的诉求。

### 2.5 状态栏 UI — 🟢
- `MenuBarExtra`（macOS 13+）；`LSUIElement=true` 无 Dock 图标。
- 菜单：连接状态 / session_id / 最近命令 / 启停 / 设置 / 退出。
- 开机自启：`SMAppService`（ServiceManagement）。

### 2.6 日志 — 🟢
- slog json/text → `Logger`(os.log) 或内存环形缓冲 +「导出日志」。

### 2.7 测试迁移 — 🟢🟡
- computerUse / 状态机 / config 解析：XCTest 直译（去掉 readImage 后，测试量从大头降为轻量）。
- 建议补一个对 mock gRPC server 或真实 console 的端到端连通测试。

### 2.8 打包 / 签名 / 公证 / DMG — 🟡🔴（工程负担，非技术难点）
- 构建：`xcodebuild archive` 或 SwiftPM 组 .app bundle。
- 签名：Developer ID Application 证书（Apple Developer 账号）。**不开 App Sandbox**（见 §4）。
- 公证：`notarytool submit` + `stapler staple`，否则别人机器 Gatekeeper 拦截。
- DMG：`hdiutil`/`create-dmg`，内含 .app + /Applications 软链；DMG 也建议签名+公证。
- 版本：Info.plist `CFBundleShortVersionString`（对应 Go 的 ldflags）。
- 完全不签名也能打 DMG，但用户需手动去 quarantine —— 自用可接受，对外体验差。

## 3. 依赖与工具链
- Xcode（Swift 6 toolchain）。
- SwiftPM：`grpc-swift`(2.x) + `grpc-swift-nio-transport` + `swift-protobuf`（+生成插件）。
- `protoc`（若预生成）。
- 分发：Developer ID 证书、`notarytool`、`create-dmg`/`hdiutil`。

## 4. 任意 bash 执行（核心确认）
**能跑任意 bash。** 决定因素是 **App Sandbox 而非签名**：
- App Sandbox 只有上架 App Store 才强制；本路线不上架 ⇒ 不开 `com.apple.security.app-sandbox` ⇒ `Process` 跑 `/bin/sh -lc <任意命令>` 无系统限制。
- 签名/公证只影响 Gatekeeper 是否放行启动，不限制启动后能执行什么。
- whitelist 设 `allow_all` 即真·任意命令；保留 `exact` 供需要收紧时用。
- **TCC** 注意：访问「桌面/文稿/下载/全盘」等受保护目录时，子进程继承宿主 App 的授权，首次可能弹框或需在「隐私与安全性」授予 Full Disk Access。不阻止 bash 运行，只影响可触达目录。

## 5. 风险登记表（裁剪后）
1. 🟡 grpc-swift 2 ↔ console 的 bidi 互通与最低系统版本 → M1 先验证。
2. ✅ 已确证：console 强制 worker-sys 同时声明 computerUse+readImage（否则 PermissionDenied）→ 采用方案 A，声明保留、readImage dispatch 回 unsupported。
3. 🟡 公证/签名/entitlements 流水线踩坑 → 预留调试时间。
4. 🟢 proto 双份生成漂移 → 进 CI 同步校验。

## 6. 里程碑
1. **M1 连通性**：proto 生成 + 裸 CLI 跑通 hello/心跳/重连，验证 grpc-swift 2 与 console 互通（**最大未知数，先做**）。
2. **M2 computerUse**：执行 + whitelist(exact/allow_all) + 截断 + deadline + 测试。
3. **M3 GUI**：MenuBarExtra + 设置窗 + Keychain + 开机自启。
4. **M4 打包**：签名 + 公证 + DMG + 版本注入。

> 粗估（单人、熟悉 Swift+gRPC）：M1≈2–3d，M2≈1–2d，M3≈2–3d，M4≈2–4d（首次公证偏上限）。合计约 **1–1.5 周**。去掉 readImage 后已无高风险模块，剩余变量集中在 M1 连通与 M4 公证。
