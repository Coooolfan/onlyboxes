# Onlyboxes

[简体中文](README.zh-CN.md)

Onlyboxes is a self-hosted code execution sandbox platform for individuals and small teams.

It uses a control-plane (`console`) and execution-plane (`worker`) architecture, and exposes both REST APIs and MCP tools.

## Key Features

- Self-hosted all components: control node (`console`) + worker nodes (`worker`)
- Separated control and execution planes:
  - Workers support **horizontal scaling**
  - Workers support multi-language **heterogeneous** implementations
  - Workers support **multiple runtimes**
- Full account system: resource isolation (stateful containers, sessions) between accounts
- MCP tools:
  - `pythonExec`: Python code execution
  - `terminalExec`: stateful terminal sessions
  - `readImage`: model-readable images
- REST API: all MCP tools also available via HTTP + async task API

> [!WARNING]
>
> In the current release, console (gRPC + HTTP) does not provide built-in TLS/mTLS.
>
> `worker` rejects insecure console endpoints by default; plaintext is allowed only when `WORKER_CONSOLE_INSECURE=true` is explicitly set.
>
> Put both console HTTP (`:8089`) and gRPC (`:50051`) endpoints behind your reverse proxy/gateway and enforce TLS for external traffic.

## Architecture

![Architecture](static/architecture.svg#gh-light-mode-only)
![Architecture](static/architecture-dark.svg#gh-dark-mode-only)

## One-Click Installer (Linux)

For a single-machine deployment of `console` + `worker-docker`, run:

```bash
curl -fsSL https://onlybox.es/install.sh | bash
```

The installer will:

1. Check the environment (Linux, Docker, Docker Compose v2, systemd)
2. Download and render the compose template with auto-generated credentials
3. Start the console via `docker compose up -d`
4. Create a `normal` worker
5. Download the architecture-matched `worker-docker` release binary for the default latest version, or your `--tag` override
6. Generate and enable a systemd service for the worker
7. Poll until the worker comes online and print a result summary

Available options:

| Flag | Default | Description |
| --- | --- | --- |
| `--tag` | latest published release | Optional release version override |
| `--workdir` | `$PWD/onlyboxes` | Working directory |
| `--yes` / `-y` | `false` | Non-interactive mode, skip confirmations |
| `--console-http-port` | `8089` | Console HTTP port (host side) |
| `--console-grpc-port` | `50051` | Console gRPC port (host side) |
| `--service-name` | `onlyboxes-worker-docker` | systemd service name |

Requirements: Linux, systemd, Docker Engine, Docker Compose v2, Python 3.

## Quick Start (Manual)

### 1) Prerequisites

- Control node:
  - Docker Engine (binaries are also available in releases — no Docker needed if deploying via binary)
- Worker node:
  - Docker Engine (required by `worker-docker`)

### 2) Start the console service

1. Download the `docker-compose.yml` file:

    ```bash
    mkdir -p onlyboxes-console && cd onlyboxes-console
    wget https://raw.githubusercontent.com/Coooolfan/onlyboxes/refs/heads/main/docker/docker-compose.yml

    ```

2. Edit `docker-compose.yml` and replace at least:
   - `CONSOLE_HASH_KEY`
   - `CONSOLE_DASHBOARD_PASSWORD`
3. Start console:

    ```bash
    docker compose up -d
    ```

Default endpoints:

- Console Web UI / HTTP REST API / MCP endpoint: `http://127.0.0.1:8089`
- gRPC: `127.0.0.1:50051`

### 3) Sign in and create an access token

- Open `http://127.0.0.1:8089` in your browser.
- Sign in with the initialized admin account.
![Console login page](static/docs/quickstart-login.png)
- Go to the token management page and create an access token.
![Token created dialog (one-time plaintext token)](static/docs/quickstart-token-modal.png)
- Save the plaintext token immediately (it is returned only once).

### 4) Create a worker

- Go to Workers page and create a worker.
![Workers page](static/docs/quickstart-workers-page.png)
- Copy and securely store the startup command from the creation dialog (`WORKER_SECRET` is one-time visible).
![Worker created dialog (startup command and one-time secret)](static/docs/quickstart-worker-created-modal.jpg)
- (Optional) Click `Open in Startup Tool with Id and Secret` to open the startup command builder with the worker id and secret pre-filled.
  - On the opened page, you can edit all available options. The generated startup command appears at the bottom of the page — copy and save it.
![Startup command builder](static/docs/quickstart-worker-startup-tool.jpg)


### 5) Run worker

> [!WARNING]
> Workers support different runtimes and environments. The current release only provides `worker-docker`. This section uses the Docker runtime as an example.

1. Log in to the machine where the worker will be deployed.
    - Ensure Docker Engine is installed.
    - Ensure the worker can reach the console gRPC endpoint.
2. Download the latest `worker-docker` binary from GitHub Releases:
    - `https://github.com/onlyboxes/onlyboxes/releases/latest`
3. Use the startup command values from the dashboard, and replace the executable path on the last line with your downloaded binary.
    - Workers reject insecure console endpoints by default; set `WORKER_CONSOLE_INSECURE=true` only to allow plaintext connections.

    ```bash
    # Example
    WORKER_CONSOLE_INSECURE=true \
    WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 \
    WORKER_ID=<worker_id> \
    WORKER_SECRET=<worker_secret> \
    /path/to/onlyboxes-worker-docker
    ```

### 6) Verify readiness

- Confirm the worker is `online` on the dashboard Workers page.
- For REST API request examples, see `README/API.md`.
- If no tokens are configured, `/mcp` and execution APIs return `401` by design.
- Add the MCP endpoint `http://127.0.0.1:8089/mcp` in any LLM Chat Client, set the token, and verify it works correctly.
![claude-code-demo](static/claude-code-demo.jpg)

## FAQ

- Q: Worker stays `offline` after starting?
  A: Check that `WORKER_CONSOLE_GRPC_TARGET` points to the correct console gRPC address and verify network connectivity.

- Q: Can a worker be deployed on the same machine as the console?
  A: Yes.

- Q: Can a worker run inside Docker?
  A: In theory, yes. However, it is not recommended because the worker needs access to the host Docker daemon. You would need to handle Docker-in-Docker yourself.

## Production Checklist

- Replace all default credentials.
- Use a reverse proxy to enforce TLS for `:8089` and `:50051`.
- Persist and back up the SQLite data directory (`CONSOLE_DB_PATH`).
- Run workers on isolated hosts to avoid sharing the Docker daemon with the console.
- Read the `Configuration Reference` below for all available options and adjust as needed.

## Configuration Reference

### Console (`console`)

| Environment Variable | Default | Notes |
| --- | --- | --- |
| `CONSOLE_HTTP_ADDR` | `:8089` | Dashboard + REST API listen address |
| `CONSOLE_GRPC_ADDR` | `:50051` | Worker registry gRPC listen address |
| `CONSOLE_HASH_KEY` | _(required)_ | HMAC key for hashing worker secrets and access tokens |
| `CONSOLE_DB_PATH` | `./db/onlyboxes-console.db` | SQLite database path |
| `CONSOLE_DB_BUSY_TIMEOUT_MS` | `5000` | SQLite busy timeout |
| `CONSOLE_TASK_RETENTION_DAYS` | `30` | Retention for completed task records |
| `CONSOLE_ENABLE_REGISTRATION` | `false` | Allow admin to register non-admin accounts |
| `CONSOLE_DASHBOARD_USERNAME` | _(empty)_ | Used only for first admin initialization |
| `CONSOLE_DASHBOARD_PASSWORD` | _(empty)_ | Used only for first admin initialization |

### Worker (`worker-docker`)

| Environment Variable | Default | Notes |
| --- | --- | --- |
| `WORKER_ID` | _(required)_ | Issued by `POST /api/v1/workers` |
| `WORKER_SECRET` | _(required)_ | Issued once by `POST /api/v1/workers` |
| `WORKER_CONSOLE_GRPC_TARGET` | `127.0.0.1:50051` | Console gRPC target |
| `WORKER_CONSOLE_INSECURE` | `false` | `false` enforces TLS endpoint; set `true` only to allow plaintext console gRPC |
| `WORKER_HEARTBEAT_INTERVAL_SEC` | `5` | Worker heartbeat interval |
| `WORKER_HEARTBEAT_JITTER_PCT` | `20` | Heartbeat jitter percent |
| `WORKER_PYTHON_EXEC_DOCKER_IMAGE` | `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` | Runtime image for `pythonExec` |
| `WORKER_TERMINAL_EXEC_DOCKER_IMAGE` | `coolfan1024/onlyboxes-runtime:default` | Runtime image for `terminalExec` |
| `WORKER_TERMINAL_OUTPUT_LIMIT_BYTES` | `1048576` | Per-stream output limit |

## API Surfaces

- Dashboard auth: `/api/v1/console/*`
- Worker management (admin): `/api/v1/workers*`
- Command execution: `/api/v1/commands/echo`, `/api/v1/commands/terminal`
- Task execution: `/api/v1/tasks*`
- MCP (Streamable HTTP): `POST /mcp`

## Development

### Run backend from source

```bash
cd console
CONSOLE_HASH_KEY=$(openssl rand -hex 32) go run ./cmd/console
```

### Run web dev server

```bash
yarn --cwd web install
yarn --cwd web dev
```

Web dev URL defaults to `http://127.0.0.1:5178` and proxies `/api/*` to `http://127.0.0.1:8089`.

### Useful docs

- Unified API reference: `README/API.md`
- Console internals: `console/README/overview.md`
- Worker internals: `worker/worker-docker/README/overview.md`
- API/proto guide: `api/README/proto.md`
- Web app guide: `web/README.md`

## Release & Images

- GitHub workflow: `.github/workflows/package-release.yml` — run it manually from the Actions tab, pick the branch and enter the version (e.g. `0.7.2`); the tag is created from that branch. Turn off `latest` when patching an older line so the newest release keeps the label
- Website-only deploy: `.github/workflows/deploy-website.yml` — manual, independent of a release
- Console Docker image: `coolfan1024/onlyboxes:<version>` and `coolfan1024/onlyboxes:latest`
- Terminal runtime images: `coolfan1024/onlyboxes-runtime:<version>-default`, `<version>-default-cn`, and `<version>-lobehub`; stable aliases are `default`, `default-cn`, `lobehub`, and `latest` (same as `default`)
- Console binary includes embedded web assets

## Security and Operational Notes

- Console does not provide built-in TLS/mTLS in this release; `worker-docker` requires explicit `WORKER_CONSOLE_INSECURE=true` to connect over plaintext.
- Put console HTTP (`:8089`) and gRPC (`:50051`) behind a reverse proxy/gateway and enforce TLS on public/external links.
- `WORKER_SECRET` and access token plaintext values are returned only at creation time.
- Dashboard login sessions are in-memory and are invalidated when `console` restarts.

## Links

- [linux.do](https://linux.do/)
- [boxlite.ai](https://boxlite.ai/)

## License

[GNU AGPL v3.0](LICENSE)
