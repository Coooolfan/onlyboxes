# Console Overview

The console service hosts:
- a gRPC registry endpoint for worker register/heartbeat.
- REST APIs for worker data:
  - `GET /api/v1/workers` for paginated worker listing.
  - `GET /api/v1/workers/stats` for aggregated worker status metrics.

Defaults:
- HTTP: `:8089`
- gRPC: `:50051`
