# Console Overview

The console service hosts:
- a gRPC registry endpoint with bidirectional stream `Connect` for worker registration + heartbeat.
- REST APIs for worker data:
  - `GET /api/v1/workers` for paginated worker listing.
  - `GET /api/v1/workers/stats` for aggregated worker status metrics.

Credential behavior:
- `console` generates worker credentials at startup (`worker_id` + `worker_secret`).
- credentials are written to `CONSOLE_WORKER_CREDENTIALS_FILE` (default `./worker-credentials.json`).
- all credentials are regenerated on every startup; old worker credentials become invalid immediately.

Defaults:
- HTTP: `:8089`
- gRPC: `:50051`
- Replay window: `60s`
- Heartbeat interval: `5s`
