# Worker Sys Overview: !!!POC Only!!!

`worker-sys` connects to console over gRPC bidi stream `Connect`, sends hello (`worker_secret`), sends periodic heartbeats, and handles `computerUse` command dispatch/result in the same stream.
- heartbeat reconnect policy: worker tolerates one heartbeat ack timeout and reconnects after two consecutive heartbeat ack timeouts.
- `WORKER_CALL_TIMEOUT_SEC` default is dynamic: `ceil(2.5 * WORKER_HEARTBEAT_INTERVAL_SEC)`.

Security warning (high risk):
- `computerUse` runs shell directly on the worker host. Shell selection is platform-dependent:
  - Unix-like (Linux, macOS): `/bin/sh -lc <command>`
  - Windows: `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command <command>`, with `[Console]::OutputEncoding` / `$OutputEncoding` forced to UTF-8 so stdout/stderr stay byte-compatible with the `WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES` truncation logic.
- this worker is **not container-sandboxed**; commands can read/modify host files and processes under the worker OS account.
- there are no resource limits: process isolation is limited to `setpgid` for signal propagation, with no cgroup, memory, CPU, or process-count constraints. Raising `WORKER_COMPUTER_USE_MAX_INFLIGHT` or `WORKER_READ_IMAGE_MAX_INFLIGHT` above `1` therefore lets concurrent commands exhaust the entire host, including the worker process itself. Only do so on hosts that impose their own cgroup or ulimit constraints.
- run only on dedicated hosts with strict OS-level isolation and least-privilege service accounts.
- do not deploy on shared machines.
- console gRPC has no built-in TLS/mTLS; plaintext transport can expose `worker_secret`.
- place console gRPC behind trusted private networking or encrypted tunnels.

Configuration sources:
- environment variables and `config.toml` (see `README/config-file.md`).
- priority is environment variable > `config.toml` > default.

Required identity:
- `WORKER_ID`
- `WORKER_SECRET`

These values are returned by `console` when calling `POST /api/v1/workers`.
`WORKER_SECRET` is returned once at creation time; if lost, delete and recreate the worker.

Worker type and capability contract:
- worker type is `worker-sys`.
- hello declares two capabilities: `computerUse` and `readImage`.
- `computerUse.max_inflight` and `readImage.max_inflight` default to `1` and are configured by `WORKER_COMPUTER_USE_MAX_INFLIGHT` and `WORKER_READ_IMAGE_MAX_INFLIGHT`. Console keeps the declared values; it only pins the capability set.
- the two capabilities have independent concurrency slots, so a `readImage` call does not block a `computerUse` call.
- console enforces that `worker-sys` cannot register any other capability.
- commands beyond a capability's limit are answered with `session_busy`.

`computerUse` behavior:
- expected payload: `{"command":"..."}`
- `command` is required and executed via the platform shell (`/bin/sh -lc` on Unix-like, `powershell.exe -Command` on Windows). The whitelist entries must match the shell form used on the worker's platform; an entry such as `ls` will not match a PowerShell-form command.
- whitelist policy can block commands before execution:
  - mode env: `WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE`
  - whitelist env: `WORKER_COMPUTER_USE_COMMAND_WHITELIST` (JSON string array, e.g. `["echo","time"]`)
  - mode values:
    - `exact` (default): command must equal one whitelist entry
    - `prefix`: command must start with one whitelist entry
    - `allow_all`: allow all commands (whitelist value is ignored)
  - in `exact`/`prefix` mode, empty or invalid whitelist blocks all commands.
- output fields:
  - `stdout`
  - `stderr`
  - `exit_code`
  - `stdout_truncated`
  - `stderr_truncated`
- non-zero process exit is returned in `exit_code` (not a command error by itself).
- output truncation is per stream and controlled by `WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES`.
- worker startup logs include whitelist mode and whitelist entry count.

`readImage` behavior:
- expected payload: `{"session_id":"computerUse","file_path":"...","action":"validate|read"}`
- accepts only `session_id="computerUse"`; any other value returns `session_not_found`.
- `file_path` is required.
- allowed path policy env: `WORKER_READ_IMAGE_ALLOWED_PATHS` (JSON string array, supports file and directory entries).
- deny by default: empty/missing/invalid `WORKER_READ_IMAGE_ALLOWED_PATHS` blocks all `readImage` access.
- path check is two-stage (normalized lexical check + symlink-resolved real path check).
- read flow binds path validation to the opened file descriptor and verifies path/file identity consistency to mitigate TOCTOU path replacement.
- if file path is outside policy or symlink-resolved path escapes allowlist, returns `path_not_allowed`.
- `action` defaults to `validate`; `read` returns file bytes in `blob`.
- output fields:
  - `session_id`
  - `file_path`
  - `mime_type`
  - `size_bytes`
  - `blob` (read only)
- MIME detection order: file extension first, then content sniff, fallback `application/octet-stream`.

Defaults:
- Console target: `127.0.0.1:50051`
- Heartbeat interval: `5s`
- Heartbeat jitter: `20%`
- Call timeout: `ceil(2.5 * WORKER_HEARTBEAT_INTERVAL_SEC)` (default heartbeat `5s` => `13s`)
- Output limit: `1048576` bytes per stream (`stdout`/`stderr`)
- log level: `info`
- log format: `json`
- log add source: `false`

Recommended setting:
- `WORKER_CALL_TIMEOUT_SEC >= 2 * WORKER_HEARTBEAT_INTERVAL_SEC`

Config env:
- `WORKER_CONSOLE_GRPC_TARGET`
- `WORKER_CONSOLE_INSECURE`
- `WORKER_ID`
- `WORKER_SECRET`
- `WORKER_NODE_NAME`
- `WORKER_VERSION`
- `WORKER_LABELS`
- `WORKER_HEARTBEAT_INTERVAL_SEC`
- `WORKER_HEARTBEAT_JITTER_PCT`
- `WORKER_CALL_TIMEOUT_SEC`
- `WORKER_LOG_LEVEL`
- `WORKER_LOG_FORMAT`
- `WORKER_LOG_ADD_SOURCE`
- `WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES`
- `WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE`
- `WORKER_COMPUTER_USE_COMMAND_WHITELIST`
- `WORKER_READ_IMAGE_ALLOWED_PATHS`
- `WORKER_COMPUTER_USE_MAX_INFLIGHT`
- `WORKER_READ_IMAGE_MAX_INFLIGHT`

Startup examples:

```bash
# Example 1: exact mode (default). Only exact "echo" or "time" is allowed.
# WORKER_CONSOLE_INSECURE=true is for local plaintext demo only.
WORKER_CONSOLE_INSECURE=true \
WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 \
WORKER_ID=<worker_id> \
WORKER_SECRET=<worker_secret> \
WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE=exact \
WORKER_COMPUTER_USE_COMMAND_WHITELIST='["echo","time"]' \
./onlyboxes-worker-sys
```

```bash
# Example 2: prefix mode. Allows commands starting with "echo " or "time ".
WORKER_CONSOLE_INSECURE=true \
WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 \
WORKER_ID=<worker_id> \
WORKER_SECRET=<worker_secret> \
WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE=prefix \
WORKER_COMPUTER_USE_COMMAND_WHITELIST='["echo ","time "]' \
./onlyboxes-worker-sys
```

```bash
# Example 3: allow_all mode. Whitelist value is ignored in this mode.
WORKER_CONSOLE_INSECURE=true \
WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 \
WORKER_ID=<worker_id> \
WORKER_SECRET=<worker_secret> \
WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE=allow_all \
./onlyboxes-worker-sys
```

```powershell
# Example 4: Windows / PowerShell. Whitelist entries must be in PowerShell form.
$env:WORKER_CONSOLE_INSECURE = "true"
$env:WORKER_CONSOLE_GRPC_TARGET = "127.0.0.1:50051"
$env:WORKER_ID = "<worker_id>"
$env:WORKER_SECRET = "<worker_secret>"
$env:WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE = "prefix"
$env:WORKER_COMPUTER_USE_COMMAND_WHITELIST = '["Get-ChildItem","Get-Process"]'
.\onlyboxes-worker-sys.exe
```
