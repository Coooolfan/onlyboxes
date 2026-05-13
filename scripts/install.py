#!/usr/bin/env python3
"""Onlyboxes one-click installer for Linux and macOS."""

import argparse
import atexit
import json
import os
import platform
import re
import secrets
import shutil
import signal
import stat
import string
import subprocess
import sys
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path
from typing import Dict, List, Optional, Tuple

MIN_PYTHON = (3, 6)
if sys.version_info < MIN_PYTHON:
    print(
        "Onlyboxes installer requires Python {}.{} or newer.".format(*MIN_PYTHON),
        file=sys.stderr,
    )
    sys.exit(1)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

GITHUB_REPO = "Coooolfan/onlyboxes"
DEFAULT_TAG = "0.5.1"
COMPOSE_TEMPLATE_URL = (
    "https://raw.githubusercontent.com/{repo}/{tag}/scripts/docker-compose.install.yml"
)
RELEASE_ASSET_URL = (
    "https://github.com/{repo}/releases/download/{tag}/{filename}"
)
PLACEHOLDERS = ["{{IMAGE_TAG}}", "{{HASH_KEY}}", "{{ADMIN_PASSWORD}}", "{{INITIAL_API_KEY}}", "{{HTTP_PORT}}", "{{GRPC_PORT}}"]
ARCH_MAP = {
    "x86_64": "amd64",
    "amd64": "amd64",
    "aarch64": "arm64",
    "arm64": "arm64",
}
POLL_INTERVAL = 3
CONSOLE_READY_TIMEOUT = 60
WORKER_ONLINE_TIMEOUT = 60

CONSOLE_SERVICE_NAME = "onlyboxes-console"

PLATFORM_NAME = "macos" if platform.system() == "Darwin" else "linux"

CONSOLE_ASSET_TEMPLATE = f"onlyboxes-console_{{version_safe}}_{PLATFORM_NAME}_{{arch}}.zip"
WORKER_DOCKER_ASSET_TEMPLATE = f"onlyboxes-worker-docker_{{version_safe}}_{PLATFORM_NAME}_{{arch}}.zip"
WORKER_BOXLITE_ASSET_TEMPLATE = f"onlyboxes-worker-boxlite_{{version_safe}}_{PLATFORM_NAME}_{{arch}}.zip"

# ---------------------------------------------------------------------------
# Deployment plan
# ---------------------------------------------------------------------------


class DeploymentPlan:
    def __init__(self, console_start: str, worker_runtime: str, worker_start: str):
        self.console_start = console_start      # "docker" | "systemd" | "foreground"
        self.worker_runtime = worker_runtime    # "docker" | "boxlite"
        self.worker_start = worker_start        # "systemd" | "foreground"

    @property
    def worker_service_name(self) -> str:
        return f"onlyboxes-worker-{self.worker_runtime}"

    @property
    def worker_binary_name(self) -> str:
        return f"onlyboxes-worker-{self.worker_runtime}"

    @property
    def worker_asset_template(self) -> str:
        if self.worker_runtime == "docker":
            return WORKER_DOCKER_ASSET_TEMPLATE
        return WORKER_BOXLITE_ASSET_TEMPLATE

    @property
    def has_foreground(self) -> bool:
        return self.console_start == "foreground" or self.worker_start == "foreground"


# ---------------------------------------------------------------------------
# Foreground process tracking & cleanup (atexit)
# ---------------------------------------------------------------------------

_fg_processes: List[subprocess.Popen] = []
_fg_compose_workdir: Optional[str] = None


def _cleanup_foreground():
    """Terminate all managed foreground processes. Registered via atexit so
    that fatal() -> sys.exit() never leaves orphan children."""
    for proc in reversed(_fg_processes):
        if proc.poll() is None:
            proc.terminate()
    for proc in _fg_processes:
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
    if _fg_compose_workdir:
        subprocess.run(
            ["docker", "compose", "down"],
            cwd=_fg_compose_workdir,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )


atexit.register(_cleanup_foreground)


# ---------------------------------------------------------------------------
# Step counter
# ---------------------------------------------------------------------------


class StepCounter:
    def __init__(self):
        self._n = 0

    def next(self, title: str) -> None:
        self._n += 1
        step(self._n, title)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

USE_COLOR = sys.stdout.isatty() and sys.stderr.isatty()


def print_banner() -> None:
    print(r"""
   ____        __      ____                      
  / __ \____  / /_  __/ __ )____  _  _____  _____
 / / / / __ \/ / / / / __  / __ \| |/_/ _ \/ ___/
/ /_/ / / / / / /_/ / /_/ / /_/ />  </  __(__  ) 
\____/_/ /_/_/\__, /_____/\____/_/|_|\___/____/  
             /____/                              
""")


def _c(code: str, text: str) -> str:
    return f"\033[{code}m{text}\033[0m" if USE_COLOR else text


def fatal(stage: str, message: str) -> None:
    print(f"\n{_c('31', '✗')} [{stage}] {message}", file=sys.stderr)
    sys.exit(1)


def info(message: str) -> None:
    print(f"  {message}")


def step(number: int, title: str) -> None:
    print(f"\n{_c('36', '●')} Step {number}: {_c('1', title)}")


def generate_password(length: int = 24) -> str:
    alphabet = string.ascii_letters + string.digits
    return "".join(secrets.choice(alphabet) for _ in range(length))


def generate_hash_key(length: int = 64) -> str:
    return secrets.token_hex(length)


def sanitize_version(tag: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]+", "-", tag)


def run_cmd(args: List[str], cwd: Optional[str] = None, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(
        args,
        cwd=cwd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        universal_newlines=True,
    )
    if check and result.returncode != 0:
        stderr = result.stderr.strip() or result.stdout.strip()
        fatal("command", f"`{' '.join(args)}` failed:\n{stderr}")
    return result


def api_request(
    url: str,
    method: str = "GET",
    data: Optional[dict] = None,
    api_key: Optional[str] = None,
) -> dict:
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if api_key:
        req.add_header("Authorization", f"Bearer {api_key}")
    try:
        with urllib.request.urlopen(req) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace") if exc.fp else ""
        fatal("api", f"{method} {url} returned {exc.code}: {detail}")
    except urllib.error.URLError as exc:
        fatal("api", f"{method} {url} failed: {exc.reason}")
    return {}  # unreachable


# ---------------------------------------------------------------------------
# Environment detection
# ---------------------------------------------------------------------------


def has_docker() -> bool:
    if shutil.which("docker") is None:
        return False
    result = run_cmd(["docker", "info"], check=False)
    return result.returncode == 0


def has_docker_compose() -> bool:
    result = run_cmd(["docker", "compose", "version"], check=False)
    return result.returncode == 0


def has_systemd() -> bool:
    if shutil.which("systemctl") is None:
        return False
    result = run_cmd(["systemctl", "--version"], check=False)
    return result.returncode == 0


def can_write_systemd() -> bool:
    return os.access("/etc/systemd/system", os.W_OK)


def detect_environment(args: argparse.Namespace) -> DeploymentPlan:
    if platform.system() not in ("Linux", "Darwin"):
        fatal("environment", "This installer only supports Linux and macOS.")

    machine = platform.machine()
    if ARCH_MAP.get(machine) is None:
        fatal("environment", f"Unsupported architecture: {machine}")

    docker_ok = has_docker()
    compose_ok = docker_ok and has_docker_compose()
    systemd_ok = has_systemd() and can_write_systemd()

    info(f"Docker available:         {'yes' if docker_ok else 'no'}")
    info(f"Docker Compose v2:        {'yes' if compose_ok else 'no'}")
    info(f"systemd writable:         {'yes' if systemd_ok else 'no'}")

    # Console start mode
    if args.console_start == "auto":
        if compose_ok:
            console_start = "docker"
        elif systemd_ok:
            console_start = "systemd"
        else:
            console_start = "foreground"
    else:
        console_start = args.console_start
        if console_start == "docker" and not compose_ok:
            fatal("environment", "--console-start=docker requires Docker and Docker Compose v2.")
        if console_start == "systemd" and not systemd_ok:
            fatal("environment", "--console-start=systemd requires systemd with write permission.")

    # Worker runtime
    if args.worker_runtime == "auto":
        worker_runtime = "docker" if docker_ok else "boxlite"
    else:
        worker_runtime = args.worker_runtime
        if worker_runtime == "docker" and not docker_ok:
            fatal("environment", "--worker-runtime=docker requires Docker.")

    if platform.system() == "Darwin" and worker_runtime == "boxlite" and ARCH_MAP.get(machine) == "amd64":
        fatal("environment", "worker-boxlite is not available for macOS amd64. Use --worker-runtime=docker.")

    # Worker start mode
    if args.worker_start == "auto":
        worker_start = "systemd" if systemd_ok else "foreground"
    else:
        worker_start = args.worker_start
        if worker_start == "systemd" and not systemd_ok:
            fatal("environment", "--worker-start=systemd requires systemd with write permission.")

    # Constraint: if any component is foreground, all must be foreground.
    # Mixed lifecycle (e.g. docker compose + foreground worker) adds complexity
    # without benefit — go all-foreground instead.
    if worker_start == "foreground" and console_start != "foreground":
        console_start = "foreground"
        info("Note: console forced to foreground (worker is foreground)")
    if console_start == "foreground" and worker_start != "foreground":
        worker_start = "foreground"
        info("Note: worker forced to foreground (console is foreground)")

    plan = DeploymentPlan(console_start, worker_runtime, worker_start)
    info(f"Console:                  {plan.console_start}")
    info(f"Worker runtime:           {plan.worker_runtime}")
    info(f"Worker:                   {plan.worker_start}")
    return plan


# ---------------------------------------------------------------------------
# Installer steps
# ---------------------------------------------------------------------------


def prepare_workdir(workdir: Path, plan: DeploymentPlan) -> None:
    existing = []
    for name in ["docker-compose.yml", "db"]:
        path = workdir / name
        if path.exists():
            existing.append(str(path))

    if platform.system() == "Linux":
        for svc in [CONSOLE_SERVICE_NAME, "onlyboxes-worker-docker", "onlyboxes-worker-boxlite"]:
            path = Path(f"/etc/systemd/system/{svc}.service")
            if path.exists():
                existing.append(str(path))

    if existing:
        fatal(
            "workdir",
            "Existing installation detected. This installer only supports fresh installs.\n"
            "  Found: " + ", ".join(existing),
        )

    subdirs = ["db", "bin", "install-artifacts"]
    if plan.worker_runtime == "boxlite":
        subdirs.append("data/boxlite")
    for sub in subdirs:
        (workdir / sub).mkdir(parents=True, exist_ok=True)

    info(f"Working directory ready: {workdir}")


def download_and_extract_release(workdir: Path, tag: str, asset_template: str, binary_name: str) -> Path:
    machine = platform.machine()
    arch = ARCH_MAP.get(machine)
    if arch is None:
        fatal("arch", f"Unsupported architecture: {machine}")

    version_safe = sanitize_version(tag)
    filename = asset_template.format(version_safe=version_safe, arch=arch)
    url = RELEASE_ASSET_URL.format(repo=GITHUB_REPO, tag=tag, filename=filename)

    zip_path = workdir / "install-artifacts" / f"{binary_name}-release.zip"
    info(f"Downloading {url}")

    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req) as resp:
            total = int(resp.headers.get("Content-Length", 0))
            downloaded = 0
            chunks = []
            while True:
                chunk = resp.read(64 * 1024)
                if not chunk:
                    break
                chunks.append(chunk)
                downloaded += len(chunk)
                if total and sys.stdout.isatty():
                    pct = downloaded * 100 // total
                    mb = downloaded / 1024 / 1024
                    total_mb = total / 1024 / 1024
                    print(f"\r  Downloading... {mb:.1f}/{total_mb:.1f} MB ({pct}%)", end="", flush=True)
            if total and sys.stdout.isatty():
                print()
            zip_path.write_bytes(b"".join(chunks))
    except urllib.error.HTTPError as exc:
        fatal("download", f"Failed to download release asset (HTTP {exc.code}): {filename}")
    except urllib.error.URLError as exc:
        fatal("download", f"Failed to download release asset: {exc.reason}")

    info("Extracting binary...")
    bin_path = workdir / "bin" / binary_name
    with zipfile.ZipFile(zip_path, "r") as zf:
        names = zf.namelist()
        if len(names) != 1:
            fatal("extract", f"Expected 1 file in zip, got {len(names)}: {names}")
        with zf.open(names[0]) as src, open(bin_path, "wb") as dst:
            dst.write(src.read())

    bin_path.chmod(bin_path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    info(f"Binary installed at {bin_path}")
    return bin_path


def download_and_render_compose(
    workdir: Path, tag: str, hash_key: str, admin_password: str,
    initial_api_key: str, http_port: int, grpc_port: int,
) -> None:
    url = COMPOSE_TEMPLATE_URL.format(repo=GITHUB_REPO, tag=tag)
    info(f"Downloading {url}")
    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req) as resp:
            template = resp.read().decode()
    except (urllib.error.HTTPError, urllib.error.URLError) as exc:
        fatal("download", f"Failed to download compose template: {exc}")

    for ph in PLACEHOLDERS:
        if ph not in template:
            fatal("template", f"Placeholder {ph} not found in compose template.")

    rendered = template
    rendered = rendered.replace("{{IMAGE_TAG}}", tag)
    rendered = rendered.replace("{{HASH_KEY}}", hash_key)
    rendered = rendered.replace("{{ADMIN_PASSWORD}}", admin_password)
    rendered = rendered.replace("{{INITIAL_API_KEY}}", initial_api_key)
    rendered = rendered.replace("{{HTTP_PORT}}", str(http_port))
    rendered = rendered.replace("{{GRPC_PORT}}", str(grpc_port))

    leftover = re.findall(r"\{\{.*?\}\}", rendered)
    if leftover:
        fatal("template", f"Unreplaced placeholders remain: {leftover}")

    compose_path = workdir / "docker-compose.yml"
    compose_path.write_text(rendered)
    info(f"Compose file written to {compose_path}")


def wait_console_ready(http_port: int, admin_password: str) -> None:
    info("Waiting for console to become ready...")
    url = f"http://127.0.0.1:{http_port}/api/v1/console/session"
    deadline = time.monotonic() + CONSOLE_READY_TIMEOUT
    while time.monotonic() < deadline:
        try:
            req = urllib.request.Request(url)
            with urllib.request.urlopen(req, timeout=5):
                pass
            _print_console_ready(http_port, admin_password)
            return
        except urllib.error.HTTPError:
            _print_console_ready(http_port, admin_password)
            return
        except (urllib.error.URLError, OSError):
            time.sleep(POLL_INTERVAL)

    fatal("console", f"Console did not become ready within {CONSOLE_READY_TIMEOUT}s.")


def _print_console_ready(http_port: int, admin_password: str) -> None:
    info("Console is ready.")
    info(f"Console:  http://127.0.0.1:{http_port}")
    info(f"Username: admin")
    info(f"Password: {admin_password}")



def create_worker(http_port: int, api_key: str) -> Tuple[str, str]:
    url = f"http://127.0.0.1:{http_port}/api/v1/workers"
    result = api_request(url, method="POST", data={"type": "normal"}, api_key=api_key)

    node_id = result.get("node_id")
    worker_secret = result.get("worker_secret")
    if not node_id or not worker_secret:
        fatal("worker", f"Unexpected response: {json.dumps(result)}")

    info(f"Worker created: {node_id}")
    return node_id, worker_secret


# ---------------------------------------------------------------------------
# Systemd service helpers
# ---------------------------------------------------------------------------


def install_systemd_service(service_name: str, unit_content: str) -> None:
    service_path = Path(f"/etc/systemd/system/{service_name}.service")
    try:
        service_path.write_text(unit_content)
    except PermissionError:
        fatal("systemd", f"Permission denied writing {service_path}. Run as root or with sudo.")

    info(f"Service file written to {service_path}")

    run_cmd(["systemctl", "daemon-reload"])
    info("daemon-reload done.")

    result = run_cmd(["systemctl", "enable", "--now", service_name], check=False)
    if result.returncode != 0:
        fatal(
            "systemd",
            f"Failed to enable/start {service_name}:\n{result.stderr.strip()}\n"
            f"  Check: journalctl -u {service_name} -n 200 --no-pager",
        )

    info(f"Service {service_name} enabled and started.")


def generate_console_binary_unit(workdir: Path, hash_key: str, admin_password: str,
                                  initial_api_key: str, http_port: int, grpc_port: int) -> str:
    bin_path = workdir / "bin" / "onlyboxes-console"
    return f"""\
[Unit]
Description=Onlyboxes Console
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory={workdir}
ExecStart={bin_path}
Restart=always
RestartSec=3
Environment=CONSOLE_HASH_KEY={hash_key}
Environment=CONSOLE_DASHBOARD_USERNAME=admin
Environment=CONSOLE_DASHBOARD_PASSWORD={admin_password}
Environment=CONSOLE_INITIAL_ADMIN_API_KEY={initial_api_key}
Environment=CONSOLE_ENABLE_REGISTRATION=true
Environment=CONSOLE_HTTP_ADDR=:{http_port}
Environment=CONSOLE_GRPC_ADDR=:{grpc_port}
Environment=CONSOLE_DB_PATH={workdir}/db/onlyboxes-console.db

[Install]
WantedBy=multi-user.target
"""


def generate_worker_unit(plan: DeploymentPlan, workdir: Path, grpc_port: int,
                          worker_id: str, worker_secret: str) -> str:
    bin_path = workdir / "bin" / plan.worker_binary_name

    if plan.worker_runtime == "docker":
        after = "network-online.target docker.service"
        requires = "Requires=docker.service\n"
        extra_env = ""
        description = "Onlyboxes Worker Docker"
    else:
        after = "network-online.target"
        requires = ""
        extra_env = f"Environment=WORKER_BOXLITE_HOME={workdir}/data/boxlite\n"
        description = "Onlyboxes Worker Boxlite"

    return f"""\
[Unit]
Description={description}
After={after}
Wants=network-online.target
{requires}
[Service]
Type=simple
WorkingDirectory={workdir}
ExecStart={bin_path}
Restart=always
RestartSec=3
Environment=WORKER_CONSOLE_INSECURE=true
Environment=WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:{grpc_port}
Environment=WORKER_ID={worker_id}
Environment=WORKER_SECRET={worker_secret}
{extra_env}
[Install]
WantedBy=multi-user.target
"""


# ---------------------------------------------------------------------------
# Foreground loop
# ---------------------------------------------------------------------------


def build_console_env(workdir: Path, hash_key: str, admin_password: str,
                      initial_api_key: str, http_port: int, grpc_port: int) -> Dict[str, str]:
    env = os.environ.copy()
    env["CONSOLE_HASH_KEY"] = hash_key
    env["CONSOLE_DASHBOARD_USERNAME"] = "admin"
    env["CONSOLE_DASHBOARD_PASSWORD"] = admin_password
    env["CONSOLE_INITIAL_ADMIN_API_KEY"] = initial_api_key
    env["CONSOLE_ENABLE_REGISTRATION"] = "true"
    env["CONSOLE_HTTP_ADDR"] = f":{http_port}"
    env["CONSOLE_GRPC_ADDR"] = f":{grpc_port}"
    env["CONSOLE_DB_PATH"] = str(workdir / "db" / "onlyboxes-console.db")
    return env


def build_worker_env(plan: DeploymentPlan, workdir: Path, grpc_port: int,
                     worker_id: str, worker_secret: str) -> Dict[str, str]:
    env = os.environ.copy()
    env["WORKER_CONSOLE_INSECURE"] = "true"
    env["WORKER_CONSOLE_GRPC_TARGET"] = f"127.0.0.1:{grpc_port}"
    env["WORKER_ID"] = worker_id
    env["WORKER_SECRET"] = worker_secret
    if plan.worker_runtime == "boxlite":
        env["WORKER_BOXLITE_HOME"] = str(workdir / "data" / "boxlite")
    return env


def enter_foreground_loop() -> None:
    """Block forever, monitoring managed processes.  Ctrl+C / SIGTERM triggers
    sys.exit() which fires the atexit cleanup handler."""

    def _shutdown(signum, frame):
        print(f"\n{_c('33', 'Shutting down...')}")
        sys.exit(0)

    signal.signal(signal.SIGINT, _shutdown)
    signal.signal(signal.SIGTERM, _shutdown)

    print(f"\n{_c('33', 'Services running in foreground. Press Ctrl+C to stop.')}")

    while True:
        for proc in _fg_processes:
            ret = proc.poll()
            if ret is not None:
                print(f"\n{_c('31', 'A managed process exited unexpectedly')} (pid={proc.pid}, code={ret})")
                sys.exit(1)
        time.sleep(1)


# ---------------------------------------------------------------------------
# Wait for worker online
# ---------------------------------------------------------------------------


def wait_worker_online(
    http_port: int, worker_id: str, api_key: str, plan: DeploymentPlan,
) -> None:
    url = f"http://127.0.0.1:{http_port}/api/v1/workers?status=online&page=1&page_size=100"
    deadline = time.monotonic() + WORKER_ONLINE_TIMEOUT
    while time.monotonic() < deadline:
        try:
            result = api_request(url, api_key=api_key)
            workers = result.get("items") or []
            for w in workers:
                if w.get("node_id") == worker_id:
                    info("Worker is online!")
                    return
        except SystemExit:
            pass
        time.sleep(POLL_INTERVAL)

    if plan.worker_start == "systemd":
        hint = (
            f"  Check status:  systemctl status {plan.worker_service_name}\n"
            f"  Check logs:    journalctl -u {plan.worker_service_name} -n 200 --no-pager"
        )
    else:
        hint = "  Check the terminal output above for errors."

    fatal(
        "worker-online",
        f"Worker {worker_id} did not come online within {WORKER_ONLINE_TIMEOUT}s.\n{hint}",
    )


# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------


def print_summary(
    plan: DeploymentPlan, workdir: Path, http_port: int,
    admin_password: str, worker_id: str, worker_secret: str,
) -> None:
    lines = [
        "",
        "=" * 56,
        "  Installation complete",
        "=" * 56,
        "",
        f"  Console:          http://127.0.0.1:{http_port}",
        f"  Username:         admin",
        f"  Password:         {admin_password}",
        f"  Worker ID:        {worker_id}",
        f"  Worker Secret:    {worker_secret}",
        f"  Console:          {plan.console_start}",
        f"  Worker runtime:   {plan.worker_runtime}",
        f"  Worker:           {plan.worker_start}",
        "",
        "  ⚠ Save the password and worker secret now.",
        "    They cannot be retrieved later.",
    ]

    # Collect systemd services
    systemd_services: List[str] = []
    if plan.console_start == "systemd":
        systemd_services.append(CONSOLE_SERVICE_NAME)
    if plan.worker_start == "systemd":
        systemd_services.append(plan.worker_service_name)

    if systemd_services:
        lines.append("")
        for svc in systemd_services:
            lines.append(f"  Start {svc}:")
            lines.append(f"    systemctl start {svc}")
            lines.append(f"  Stop {svc}:")
            lines.append(f"    systemctl stop {svc}")
            lines.append(f"  Status {svc}:")
            lines.append(f"    systemctl status {svc}")
            lines.append(f"  Logs {svc}:")
            lines.append(f"    journalctl -u {svc} -n 200 --no-pager")
            lines.append("")

    if plan.has_foreground:
        lines.append("")
        lines.append("  Foreground services are running.")
        lines.append("  Press Ctrl+C to stop them.")

    # Uninstall
    lines.append("")
    lines.append("  Uninstall:")
    for svc in systemd_services:
        lines.append(f"    systemctl stop {svc}")
        lines.append(f"    systemctl disable {svc}")
        lines.append(f"    rm /etc/systemd/system/{svc}.service")
    if systemd_services:
        lines.append("    systemctl daemon-reload")
    if plan.console_start == "docker":
        lines.append(f"    docker compose -f {workdir}/docker-compose.yml down")
    lines.append(f"    rm -rf {workdir}")

    lines.append("")
    lines.append("  Next:")
    lines.append(f"    - Open the console in your browser: http://127.0.0.1:{http_port}")
    lines.append('    - Log in with username "admin" and the password shown above')
    lines.append("    - Create a mcp token and configure it in your MCP client")
    lines.append("    - Also see: https://github.com/Coooolfan/onlyboxes?tab=readme-ov-file#6-verify-readiness")
    lines.append("")

    print("\n".join(lines))


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Onlyboxes installer – deploy console + worker on a single Linux or macOS host."
    )
    parser.add_argument(
        "--tag",
        default=DEFAULT_TAG,
        help=f"Override release version tag for advanced use cases (default: {DEFAULT_TAG})",
    )
    parser.add_argument("--workdir", default=None, help="Working directory (default: $PWD/onlyboxes)")
    parser.add_argument("--yes", "-y", action="store_true", help="Non-interactive mode, skip confirmations")
    parser.add_argument("--console-http-port", type=int, default=8089, help="Console HTTP port (default: 8089)")
    parser.add_argument("--console-grpc-port", type=int, default=50051, help="Console gRPC port (default: 50051)")
    parser.add_argument(
        "--console-start", choices=["auto", "docker", "systemd", "foreground"], default="auto",
        help="Console start mode (default: auto-detect)",
    )
    parser.add_argument(
        "--worker-runtime", choices=["auto", "docker", "boxlite"], default="auto",
        help="Worker runtime (default: auto-detect)",
    )
    parser.add_argument(
        "--worker-start", choices=["auto", "systemd", "foreground"], default="auto",
        help="Worker start mode (default: auto-detect)",
    )
    return parser.parse_args()


def main() -> None:
    global _fg_compose_workdir

    args = parse_args()

    workdir = Path(args.workdir) if args.workdir else Path.cwd() / "onlyboxes"
    workdir = workdir.resolve()

    tag = args.tag
    http_port = args.console_http_port
    grpc_port = args.console_grpc_port

    admin_password = generate_password()
    hash_key = generate_hash_key()
    api_key = "obxk_" + secrets.token_hex(32)

    sc = StepCounter()

    print_banner()

    # --- Step: Environment detection ---
    sc.next("Environment detection")
    plan = detect_environment(args)

    # --- Execution plan summary ---
    print(f"""
{'=' * 56}
  Execution Plan
{'=' * 56}

  Tag:              {tag}
  Workdir:          {workdir}
  HTTP port:        {http_port}
  gRPC port:        {grpc_port}

  Console:          {plan.console_start}
  Worker runtime:   {plan.worker_runtime}
  Worker:           {plan.worker_start}
""")

    if not args.yes and sys.stdin.isatty():
        try:
            answer = input("Proceed? [Y/n] ").strip().lower()
        except (EOFError, KeyboardInterrupt):
            print()
            sys.exit(1)
        if answer and answer not in ("y", "yes"):
            print("Aborted.")
            sys.exit(0)

    # --- Step: Prepare working directory ---
    sc.next("Prepare working directory")
    prepare_workdir(workdir, plan)

    # --- Step: Download releases ---
    if plan.console_start == "docker":
        sc.next("Download releases")
        download_and_render_compose(workdir, tag, hash_key, admin_password, api_key, http_port, grpc_port)
        download_and_extract_release(workdir, tag, plan.worker_asset_template, plan.worker_binary_name)
    else:
        sc.next("Download releases")
        download_and_extract_release(workdir, tag, CONSOLE_ASSET_TEMPLATE, "onlyboxes-console")
        download_and_extract_release(workdir, tag, plan.worker_asset_template, plan.worker_binary_name)

    # --- Step: Deploy console ---
    if plan.console_start == "docker":
        sc.next("Start console (Docker Compose)")
        run_cmd(["docker", "compose", "up", "-d"], cwd=str(workdir))
        # Track for cleanup only when the script will stay alive (foreground worker)
        if plan.has_foreground:
            _fg_compose_workdir = str(workdir)
    elif plan.console_start == "systemd":
        sc.next("Setup console systemd service")
        unit = generate_console_binary_unit(workdir, hash_key, admin_password, api_key, http_port, grpc_port)
        install_systemd_service(CONSOLE_SERVICE_NAME, unit)
    else:  # foreground
        sc.next("Start console (foreground)")
        console_env = build_console_env(workdir, hash_key, admin_password, api_key, http_port, grpc_port)
        console_bin = workdir / "bin" / "onlyboxes-console"
        proc = subprocess.Popen(
            [str(console_bin)],
            cwd=str(workdir),
            env=console_env,
        )
        _fg_processes.append(proc)
        info(f"Console started (pid={proc.pid})")

    # --- Step: Wait for console ready ---
    sc.next("Wait for console to become ready")
    wait_console_ready(http_port, admin_password)

    # --- Step: Create worker ---
    sc.next("Create worker")
    worker_id, worker_secret = create_worker(http_port, api_key)

    # --- Step: Start worker ---
    if plan.worker_start == "systemd":
        sc.next(f"Setup {plan.worker_service_name} systemd service")
        unit = generate_worker_unit(plan, workdir, grpc_port, worker_id, worker_secret)
        install_systemd_service(plan.worker_service_name, unit)
    else:  # foreground
        sc.next(f"Start worker-{plan.worker_runtime} (foreground)")
        worker_env = build_worker_env(plan, workdir, grpc_port, worker_id, worker_secret)
        worker_bin = workdir / "bin" / plan.worker_binary_name
        proc = subprocess.Popen(
            [str(worker_bin)],
            cwd=str(workdir),
            env=worker_env,
        )
        _fg_processes.append(proc)
        info(f"Worker started (pid={proc.pid})")

    # --- Step: Wait worker online ---
    sc.next("Waiting for worker to come online")
    wait_worker_online(http_port, worker_id, api_key, plan)

    # Cleanup
    shutil.rmtree(workdir / "install-artifacts", ignore_errors=True)

    # --- Summary ---
    print_summary(plan, workdir, http_port, admin_password, worker_id, worker_secret)

    # --- Foreground loop ---
    if plan.has_foreground:
        enter_foreground_loop()


if __name__ == "__main__":
    main()
