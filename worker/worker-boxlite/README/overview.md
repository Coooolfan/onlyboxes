# Worker Boxlite Overview

`worker-boxlite` connects to console over the gRPC bidi stream `Connect`, sends a hello frame (including `worker_secret`), then sends periodic heartbeat frames and handles command dispatch/result in the same stream.
- heartbeat reconnect policy: worker tolerates one heartbeat ack timeout and reconnects after two consecutive heartbeat ack timeouts.
- `WORKER_CALL_TIMEOUT_SEC` default is dynamic: `ceil(2.5 * WORKER_HEARTBEAT_INTERVAL_SEC)`.

Security warning (high risk):
- console gRPC currently has no built-in TLS/mTLS.
- `worker-boxlite` rejects insecure console endpoints by default; plaintext is allowed only with `WORKER_CONSOLE_INSECURE=true`.
- place console HTTP (`:8089`) and gRPC (`:50051`) behind a reverse proxy/gateway and enforce TLS for all external traffic.
- `worker_secret` in hello is visible on the network path without transport encryption.
- run only inside trusted private networks or encrypted tunnels; never expose this channel directly on public internet.
- full mitigation requires TLS/mTLS support (not implemented in this release).

Build and runtime prerequisites:
- supported host matrix follows current Boxlite support: `linux/amd64`, `linux/arm64`, `darwin/arm64`.
- the crate depends on crates.io `boxlite 0.9.7` with `gvproxy` enabled.
- `protoc >= 3.12` must be on `PATH`: the `boxlite-shared` build script invokes `protoc` directly and does not honour the `PROTOC` environment variable, so the vendored `protoc-bin-vendored` used for this crate's own protobuf codegen does not satisfy it.
- a local clone of Boxlite is optional and only useful for upstream source reading or local debugging; it is not required to build `worker-boxlite`.
- terminal images must contain `/bin/sh` and `python`.

Required identity:
- `WORKER_ID`
- `WORKER_SECRET`

These values are returned by `console` when calling `POST /api/v1/workers` (startup command response).
`WORKER_SECRET` is only returned once at creation time; if lost, delete and recreate the worker in dashboard/API.

Version report:
- worker registers `version` in `ConnectHello`.
- default source is binary embedded build version (`dev` when not injected).
- can be overridden with `WORKER_VERSION`.

Capability behavior:
- `worker-boxlite` hardcodes capability declarations to `echo`, `pythonExec`, `terminalExec`, and `terminalResource`.
- each capability declaration includes `max_inflight` (default `4`, configurable per capability via environment variables).
- startup logs include execution config summaries for `pythonExec` and `terminalExec` (image/lease/output-limit).
- command dispatch logs are summary-only and do not include raw command/code/path/message content.
- when receiving an `echo` command, worker returns the exact input string unchanged.
- when receiving a `pythonExec` command, worker expects `payload_json` with `{"code":"..."}` and runs `python -c <code>` inside a one-shot Boxlite box.
- `pythonExec` image is configured by `WORKER_PYTHON_EXEC_BOXLITE_IMAGE`.
- if command deadline/cancel happens during execution, worker kills the Boxlite execution, force-removes the box, then returns `deadline_exceeded`.
- `pythonExec` result always uses JSON payload:
  - `{"output":"...","stderr":"...","exit_code":0}`
- non-zero Python exit code is returned in `exit_code` and does not become command error by itself.
- when receiving a `terminalExec` command, worker expects `payload_json` with:
  - `{"command":"...","session_id":"optional","create_if_missing":false,"lease_ttl_sec":60}`
- `terminalExec` image is configured by `WORKER_TERMINAL_EXEC_BOXLITE_IMAGE`.
- `terminalExec` session behavior:
  - same `session_id` reuses the same box and keeps filesystem state.
  - missing `session_id` creates a new box/session automatically.
  - unknown `session_id` returns `session_not_found`, unless `create_if_missing=true`.
  - concurrent commands on the same `session_id` are capped by `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` (default `1`); exceeding the cap returns `session_busy`.
  - lease extension is monotonic: shorter `lease_ttl_sec` does not reduce current expiry.
- `terminalExec` session concurrency:
  - `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` counts `terminalExec` and `terminalResource` commands together, so both can run at once in one session once it is above `1`.
  - concurrent commands share the box and therefore its filesystem, but each runs as an independent `sh -lc` process with its own boxlite `execution_id` and does not share shell state (cwd, environment, shell variables).
  - concurrent commands share one microVM's resource budget (`WORKER_TERMINAL_EXEC_MEMORY_MIB`, `WORKER_TERMINAL_EXEC_CPUS`, `WORKER_TERMINAL_EXEC_MAX_PROCESSES`); raise those alongside the concurrency cap.
  - the worker-level caps (`WORKER_TERMINAL_EXEC_MAX_INFLIGHT`, `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT`) bound the per-session cap. Raise them too, or a single session can consume the worker's entire quota.
  - a session whose box is still being created makes concurrent callers wait for it; if creation fails they all receive the same error.
- `terminalExec` cleanup behavior:
  - command timeout/cancel marks the session for destruction and stops it accepting new commands; the box is removed once in-flight commands drain, so one command's timeout does not kill its siblings.
  - idle sessions are reaped after lease expiry by an internal janitor loop; a session with in-flight commands is never reaped.
  - worker shutdown force-removes all managed terminal boxes.
  - `SIGINT`/`SIGTERM` performs best-effort cleanup; `SIGKILL`/process crash does not guarantee cleanup.
- `terminalExec` result uses JSON payload:
  - `{"session_id":"...","created":true,"stdout":"...","stderr":"...","exit_code":0,"stdout_truncated":false,"stderr_truncated":false,"lease_expires_unix_ms":...}`
- output truncation:
  - `stdout` and `stderr` are individually truncated by `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES`.
  - truncation flags are exposed via `stdout_truncated` and `stderr_truncated`.
- when receiving a `terminalResource` command, worker expects `payload_json` with:
  - `{"session_id":"required","file_path":"required","action":"validate|read"}`
  - `action` defaults to `validate` when omitted.
  - target `file_path` must exist and must not be a directory.
  - `read` action returns file content in `blob` as a base64 JSON string.
  - `read` action rejects files larger than `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` with `file_too_large`.
  - session concurrency follows terminal session rules:
    - unknown `session_id` returns `session_not_found`.
    - commands beyond `WORKER_TERMINAL_SESSION_MAX_INFLIGHT` on the same `session_id` return `session_busy`; the cap is shared with `terminalExec`.
- `terminalResource` result uses JSON payload:
  - validate: `{"session_id":"...","file_path":"...","mime_type":"...","size_bytes":123}`
  - read: `{"session_id":"...","file_path":"...","mime_type":"...","size_bytes":123,"blob":"...base64..."}`
- `terminalResource` domain error codes:
  - `file_not_found`
  - `path_is_directory`
  - `file_too_large`

Defaults:
- Console target: `127.0.0.1:50051`
- Heartbeat interval: `5s`
- Heartbeat jitter: `20%`
- Call timeout: `ceil(2.5 * WORKER_HEARTBEAT_INTERVAL_SEC)` (default heartbeat `5s` => `13s`)
- pythonExec image: `ghcr.io/astral-sh/uv:python3.12-bookworm-slim`
- terminalExec image: `coolfan1024/onlyboxes-default-worker:0.0.5`
- pythonExec memory / cpus / max processes: `256 MiB` / `1` / `128`
- terminalExec memory / cpus / max processes: `256 MiB` / `1` / `128`
- terminal lease min/max/default: `60s` / `1800s` / `60s`
- terminal output limit: `1048576` bytes per stream (`stdout`/`stderr`) and per `terminalResource read`
- capability max_inflight: `4` per capability
- log level: `info`
- log format: `json`
- log add source: `false`

Main environment variables:
- `WORKER_CONSOLE_GRPC_TARGET`
- `WORKER_CONSOLE_INSECURE`
- `WORKER_NODE_NAME`
- `WORKER_VERSION`
- `WORKER_LABELS`
- `WORKER_HEARTBEAT_INTERVAL_SEC`
- `WORKER_HEARTBEAT_JITTER_PCT`
- `WORKER_CALL_TIMEOUT_SEC`
- `WORKER_BOXLITE_HOME`
- `WORKER_PYTHON_EXEC_BOXLITE_IMAGE`
- `WORKER_PYTHON_EXEC_MEMORY_MIB`
- `WORKER_PYTHON_EXEC_CPUS`
- `WORKER_PYTHON_EXEC_MAX_PROCESSES`
- `WORKER_TERMINAL_EXEC_BOXLITE_IMAGE`
- `WORKER_TERMINAL_EXEC_MEMORY_MIB`
- `WORKER_TERMINAL_EXEC_CPUS`
- `WORKER_TERMINAL_EXEC_MAX_PROCESSES`
- `WORKER_TERMINAL_LEASE_MIN_SEC`
- `WORKER_TERMINAL_LEASE_MAX_SEC`
- `WORKER_TERMINAL_LEASE_DEFAULT_SEC`
- `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES`
- `WORKER_ECHO_MAX_INFLIGHT`
- `WORKER_PYTHON_EXEC_MAX_INFLIGHT`
- `WORKER_TERMINAL_EXEC_MAX_INFLIGHT`
- `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT`
- `WORKER_TERMINAL_SESSION_MAX_INFLIGHT`
- `WORKER_LOG_LEVEL`
- `WORKER_LOG_FORMAT`
- `WORKER_LOG_ADD_SOURCE`

Build and run:
- build from `worker/worker-boxlite` with `cargo build --release`
- run with `cargo run --release`
- production packaging should inject the build version through the existing binary version mechanism; otherwise the worker reports `dev`

Logging config:
- `WORKER_LOG_LEVEL`: `debug|info|warn|error` (default `info`)
- `WORKER_LOG_FORMAT`: `json|text` (default `json`)
- `WORKER_LOG_ADD_SOURCE`: include source file/line in logs (default `false`)

Capability concurrency:
- `WORKER_ECHO_MAX_INFLIGHT`: maximum concurrent echo commands (default `4`)
- `WORKER_PYTHON_EXEC_MAX_INFLIGHT`: maximum concurrent pythonExec commands (default `4`)
- `WORKER_TERMINAL_EXEC_MAX_INFLIGHT`: maximum concurrent terminalExec commands (default `4`)
- `WORKER_TERMINAL_RESOURCE_MAX_INFLIGHT`: maximum concurrent terminalResource commands (default `4`)
- `WORKER_TERMINAL_SESSION_MAX_INFLIGHT`: maximum concurrent commands per terminal session, counting `terminalExec` and `terminalResource` together (default `1`)
- invalid values (non-positive integers) fall back to the default.

Recommended setting:
- `WORKER_CALL_TIMEOUT_SEC >= 2 * WORKER_HEARTBEAT_INTERVAL_SEC`

Manual smoke checklist:
- start `console`, create a `worker-boxlite`, and launch the worker with the returned `WORKER_ID` and `WORKER_SECRET`
- verify startup logs show `pythonExec configured`, `terminalExec configured`, and `worker connected`
- send `echo` with `{"message":"hello"}` and verify the same payload is returned
- send `pythonExec` with `{"code":"print('hello')"}`
- send a timeout-bounded `pythonExec` and verify the worker returns `deadline_exceeded`
- send `terminalExec` without `session_id`, write a file, then send another `terminalExec` with the returned `session_id` and verify the file is still present
- send `terminalResource` validate/read against that file and verify MIME, size, and base64 blob
- send `terminalResource` against a missing file, a directory, and an oversized file and verify `file_not_found`, `path_is_directory`, and `file_too_large`

Backend smoke test:
- `cargo run --example boxlite_smoke` exercises the boxlite API directly, without `console` or gRPC: runtime init, box create/start, exec, exit codes and stderr, filesystem state across executions, concurrent execution on a single box, `kill()` isolation between executions, `copy_out`, removal, and shutdown.
- environment overrides: `BOXLITE_SMOKE_IMAGE` (default `alpine:latest`), `BOXLITE_SMOKE_HOME` (default a fresh temp dir), `BOXLITE_SMOKE_CONCURRENCY` (default `4`), `BOXLITE_SMOKE_KEEP_HOME`.
- point `BOXLITE_SMOKE_HOME` at an existing boxlite home to exercise on-disk database migrations after a dependency upgrade.
