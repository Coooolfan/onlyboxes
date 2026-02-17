# Console Overview

The console service hosts:
- a gRPC registry endpoint with bidirectional stream `Connect` for worker registration + heartbeat + command dispatch/result.
- REST APIs for worker data (dashboard, authentication required):
  - `GET /api/v1/workers` for paginated worker listing.
  - `GET /api/v1/workers/stats` for aggregated worker status metrics.
- command APIs (execution, no authentication required):
  - `POST /api/v1/commands/echo` for blocking echo command execution.
  - `POST /api/v1/tasks` for sync/async/auto task submission.
  - `GET /api/v1/tasks/:task_id` for task status and result lookup.
  - `POST /api/v1/tasks/:task_id/cancel` for best-effort task cancellation.
- dashboard authentication APIs:
  - `POST /api/v1/console/login` with `{"username":"...","password":"..."}`.
  - `POST /api/v1/console/logout`.

Credential behavior:
- `console` generates worker credentials at startup (`worker_id` + `worker_secret`).
- credentials are written to `CONSOLE_WORKER_CREDENTIALS_FILE` (default `./worker-credentials.json`).
- all credentials are regenerated on every startup; old worker credentials become invalid immediately.

Defaults:
- HTTP: `:8089`
- gRPC: `:50051`
- Replay window: `60s`
- Heartbeat interval: `5s`

Dashboard credential behavior:
- startup resolves dashboard credentials and logs them to stdout.
- username env: `CONSOLE_DASHBOARD_USERNAME`
- password env: `CONSOLE_DASHBOARD_PASSWORD`
- if either env var is missing, only the missing value is randomly generated.
