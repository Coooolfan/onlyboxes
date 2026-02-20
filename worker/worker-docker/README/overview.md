# Worker Docker Overview

`worker-docker` connects to console over gRPC bidi stream `Connect`, sends a hello frame (including `worker_secret`), then sends periodic heartbeat frames and handles command dispatch/result in the same stream.

Security warning (high risk):
- worker-to-console gRPC is currently plaintext by default (no TLS/mTLS).
- `worker_secret` in hello is visible on the network path without transport encryption.
- run only inside trusted private networks or encrypted tunnels; never expose this channel directly on public internet.
- full mitigation requires TLS/mTLS support (not implemented in this release).

Required identity:
- `WORKER_ID`
- `WORKER_SECRET`

These values are returned by `console` when calling `POST /api/v1/workers` (startup command response).
`WORKER_SECRET` is only returned once at creation time; if lost, delete and recreate the worker in dashboard/API.

Capability behavior:
- `worker-docker` hardcodes capability declarations to `echo`, `pythonExec`, `terminalExec`, and `terminalResource`.
- each capability declaration includes `max_inflight=4`.
- when receiving an `echo` command, worker returns the exact input string unchanged.
- when receiving a `pythonExec` command, worker expects `payload_json` with `{"code":"..."}` and runs:
  - `docker create --name <generated-name> --label onlyboxes.managed=true --label onlyboxes.capability=pythonExec --label onlyboxes.runtime=worker-docker --memory 256m --cpus 1.0 --pids-limit 128 <python_exec_image> python -c <code>`
  - `docker start -a <generated-name>`
  - `docker rm -f <generated-name>` for unified cleanup
- `pythonExec` image is configured by `WORKER_PYTHON_EXEC_DOCKER_IMAGE`.
- if command deadline/cancel happens during execution, worker still performs forced cleanup via an independent short-timeout `docker rm -f`, then returns `deadline_exceeded`.
- `pythonExec` result always uses JSON payload:
  - `{"output":"...","stderr":"...","exit_code":0}`
- non-zero Python exit code is returned in `exit_code` and does not become command error by itself.
- when receiving a `terminalExec` command, worker expects `payload_json` with:
  - `{"command":"...","session_id":"optional","create_if_missing":false,"lease_ttl_sec":60}`
- `terminalExec` image is configured by `WORKER_TERMINAL_EXEC_DOCKER_IMAGE`.
- `terminalExec` session behavior:
  - same `session_id` reuses the same container and keeps filesystem state.
  - missing `session_id` creates a new container/session automatically.
  - unknown `session_id` returns `session_not_found`, unless `create_if_missing=true`.
  - concurrent execution on the same `session_id` returns `session_busy`.
  - lease extension is monotonic: shorter `lease_ttl_sec` does not reduce current expiry.
- `terminalExec` cleanup behavior:
  - command timeout/cancel triggers forced `docker rm -f` and drops the session.
  - idle sessions are reaped after lease expiry by an internal janitor loop.
  - worker shutdown force-removes all managed terminal containers.
  - `SIGINT`/`SIGTERM` (for example Ctrl+C) performs best-effort cleanup; `SIGKILL`/process crash does not guarantee cleanup.
- `terminalExec` result uses JSON payload:
  - `{"session_id":"...","created":true,"stdout":"...","stderr":"...","exit_code":0,"stdout_truncated":false,"stderr_truncated":false,"lease_expires_unix_ms":...}`
- output truncation:
  - `stdout` and `stderr` are individually truncated by `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES`.
  - truncation flags are exposed via `stdout_truncated` and `stderr_truncated`.
- when receiving a `terminalResource` command, worker expects `payload_json` with:
  - `{"session_id":"required","file_path":"required","action":"validate|read"}`
  - `action` defaults to `validate` when omitted.
  - target `file_path` must exist and must not be a directory.
  - `read` action returns file content as base64 JSON bytes in `blob`.
  - `read` action rejects files larger than `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` with `file_too_large`.
  - session concurrency follows terminal session rules:
    - unknown `session_id` returns `session_not_found`.
    - concurrent operation on same `session_id` returns `session_busy`.
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
- pythonExec image: `python:slim`
- terminalExec image: `coolfan1024/onlyboxes-default-worker:0.0.3`
- terminal lease min/max/default: `60s` / `1800s` / `60s`
- terminal output limit: `1048576` bytes per stream (`stdout`/`stderr`)
