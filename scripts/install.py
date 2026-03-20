#!/usr/bin/env python3
"""Onlyboxes one-click installer for Linux."""

import argparse
import http.cookiejar
import json
import os
import platform
import re
import secrets
import shutil
import stat
import string
import subprocess
import sys
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

GITHUB_REPO = "Coooolfan/onlyboxes"
COMPOSE_TEMPLATE_URL = (
    "https://raw.githubusercontent.com/{repo}/{tag}/scripts/docker-compose.install.yml"
)
RELEASE_ASSET_URL = (
    "https://github.com/{repo}/releases/download/{tag}/{filename}"
)
PLACEHOLDERS = ["{{IMAGE_TAG}}", "{{HASH_KEY}}", "{{ADMIN_PASSWORD}}", "{{HTTP_PORT}}", "{{GRPC_PORT}}"]
ARCH_MAP = {
    "x86_64": "amd64",
    "amd64": "amd64",
    "aarch64": "arm64",
    "arm64": "arm64",
}
POLL_INTERVAL = 3
CONSOLE_READY_TIMEOUT = 60
WORKER_ONLINE_TIMEOUT = 60

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def fatal(stage: str, message: str) -> None:
    print(f"\n✗ [{stage}] {message}", file=sys.stderr)
    sys.exit(1)


def info(message: str) -> None:
    print(f"  {message}")


def step(number: int, title: str) -> None:
    print(f"\n● Step {number}: {title}")


def generate_password(length: int = 24) -> str:
    alphabet = string.ascii_letters + string.digits
    return "".join(secrets.choice(alphabet) for _ in range(length))


def generate_hash_key(length: int = 64) -> str:
    return secrets.token_hex(length)


def sanitize_version(tag: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]+", "-", tag)


def run_cmd(args: list[str], cwd: str | None = None, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if check and result.returncode != 0:
        stderr = result.stderr.strip() or result.stdout.strip()
        fatal("command", f"`{' '.join(args)}` failed:\n{stderr}")
    return result


def build_opener(cookie_jar: http.cookiejar.CookieJar) -> urllib.request.OpenerDirector:
    return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookie_jar))


def api_request(
    opener: urllib.request.OpenerDirector,
    url: str,
    method: str = "GET",
    data: dict | None = None,
) -> dict:
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with opener.open(req) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace") if exc.fp else ""
        fatal("api", f"{method} {url} returned {exc.code}: {detail}")
    except urllib.error.URLError as exc:
        fatal("api", f"{method} {url} failed: {exc.reason}")
    return {}  # unreachable


# ---------------------------------------------------------------------------
# Installer steps
# ---------------------------------------------------------------------------


def check_environment() -> None:
    step(1, "Environment check")

    if platform.system() != "Linux":
        fatal("environment", "This installer only supports Linux.")

    for cmd, hint in [
        ("docker", "Install Docker: https://docs.docker.com/engine/install/"),
        ("systemctl", "This installer requires systemd."),
    ]:
        if shutil.which(cmd) is None:
            fatal("environment", f"`{cmd}` not found. {hint}")

    result = run_cmd(["docker", "compose", "version"], check=False)
    if result.returncode != 0:
        fatal("environment", "`docker compose` is not available. Install Docker Compose v2.")

    result = run_cmd(["docker", "info"], check=False)
    if result.returncode != 0:
        fatal(
            "environment",
            "Cannot connect to Docker daemon. Ensure Docker is running and your user has permission.\n"
            "  Hint: sudo usermod -aG docker $USER && newgrp docker",
        )

    result = run_cmd(["systemctl", "--version"], check=False)
    if result.returncode != 0:
        fatal("environment", "systemctl is not functional.")

    info("All checks passed.")


def prepare_workdir(workdir: Path, service_name: str) -> None:
    step(2, "Prepare working directory")

    service_file = Path(f"/etc/systemd/system/{service_name}.service")
    existing = []
    if (workdir / "docker-compose.yml").exists():
        existing.append(str(workdir / "docker-compose.yml"))
    if (workdir / "db").exists():
        existing.append(str(workdir / "db/"))
    if service_file.exists():
        existing.append(str(service_file))

    if existing:
        fatal(
            "workdir",
            "Existing installation detected. This installer only supports fresh installs.\n"
            "  Found: " + ", ".join(existing),
        )

    for sub in ["db", "bin", "run", "install-artifacts"]:
        (workdir / sub).mkdir(parents=True, exist_ok=True)

    info(f"Working directory ready: {workdir}")


def download_and_render_compose(
    workdir: Path, tag: str, hash_key: str, admin_password: str,
    http_port: int, grpc_port: int,
) -> None:
    step(3, "Download and render compose template")

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
    rendered = rendered.replace("{{HTTP_PORT}}", str(http_port))
    rendered = rendered.replace("{{GRPC_PORT}}", str(grpc_port))

    leftover = re.findall(r"\{\{.*?\}\}", rendered)
    if leftover:
        fatal("template", f"Unreplaced placeholders remain: {leftover}")

    compose_path = workdir / "docker-compose.yml"
    compose_path.write_text(rendered)
    info(f"Compose file written to {compose_path}")


def start_console(workdir: Path, http_port: int, admin_password: str) -> None:
    step(4, "Start console")

    run_cmd(["docker", "compose", "up", "-d"], cwd=str(workdir))
    info("Waiting for console to become ready...")

    url = f"http://127.0.0.1:{http_port}/api/v1/console/session"
    deadline = time.monotonic() + CONSOLE_READY_TIMEOUT
    while time.monotonic() < deadline:
        try:
            req = urllib.request.Request(url)
            with urllib.request.urlopen(req, timeout=5):
                pass
            info("Console is ready.")
            info(f"Console:  http://127.0.0.1:{http_port}")
            info(f"Username: admin")
            info(f"Password: {admin_password}")
            return
        except urllib.error.HTTPError:
            info("Console is ready.")
            info(f"Console:  http://127.0.0.1:{http_port}")
            info(f"Username: admin")
            info(f"Password: {admin_password}")
            return
        except (urllib.error.URLError, OSError):
            time.sleep(POLL_INTERVAL)

    fatal("console", f"Console did not become ready within {CONSOLE_READY_TIMEOUT}s.")


def login_and_save_cookie(
    workdir: Path, http_port: int, admin_password: str,
) -> urllib.request.OpenerDirector:
    step(5, "Login and save session cookie")

    cookie_jar = http.cookiejar.MozillaCookieJar(str(workdir / "run" / "console.cookiejar"))
    opener = build_opener(cookie_jar)

    url = f"http://127.0.0.1:{http_port}/api/v1/console/login"
    body = json.dumps({"username": "admin", "password": admin_password}).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", "application/json")

    try:
        with opener.open(req) as resp:
            result = json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace") if exc.fp else ""
        fatal("login", f"Login failed (HTTP {exc.code}): {detail}")
    except urllib.error.URLError as exc:
        fatal("login", f"Login request failed: {exc.reason}")

    if not result.get("authenticated"):
        fatal("login", "Login returned authenticated=false.")

    cookie_path = workdir / "run" / "console.cookiejar"
    cookie_jar.save(ignore_discard=True, ignore_expires=True)
    os.chmod(cookie_path, stat.S_IRUSR | stat.S_IWUSR)
    info("Login successful. Cookie saved.")
    return opener


def create_worker(
    opener: urllib.request.OpenerDirector, http_port: int,
) -> tuple[str, str]:
    step(6, "Create worker")

    url = f"http://127.0.0.1:{http_port}/api/v1/workers"
    result = api_request(opener, url, method="POST", data={"type": "normal"})

    node_id = result.get("node_id")
    worker_secret = result.get("worker_secret")
    if not node_id or not worker_secret:
        fatal("worker", f"Unexpected response: {json.dumps(result)}")

    info(f"Worker created: {node_id}")
    return node_id, worker_secret


def download_worker_release(workdir: Path, tag: str) -> None:
    step(7, "Download worker-docker release")

    machine = platform.machine()
    arch = ARCH_MAP.get(machine)
    if arch is None:
        fatal("arch", f"Unsupported architecture: {machine}")

    version_safe = sanitize_version(tag)
    filename = f"onlyboxes-worker-docker_{version_safe}_linux_{arch}.zip"
    url = RELEASE_ASSET_URL.format(repo=GITHUB_REPO, tag=tag, filename=filename)

    zip_path = workdir / "install-artifacts" / "worker-release.zip"
    info(f"Downloading {url}")

    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req) as resp:
            zip_path.write_bytes(resp.read())
    except urllib.error.HTTPError as exc:
        fatal("download", f"Failed to download release asset (HTTP {exc.code}): {filename}")
    except urllib.error.URLError as exc:
        fatal("download", f"Failed to download release asset: {exc.reason}")

    info("Extracting binary...")
    bin_path = workdir / "bin" / "onlyboxes-worker-docker"
    with zipfile.ZipFile(zip_path, "r") as zf:
        names = zf.namelist()
        if len(names) != 1:
            fatal("extract", f"Expected 1 file in zip, got {len(names)}: {names}")
        with zf.open(names[0]) as src, open(bin_path, "wb") as dst:
            dst.write(src.read())

    bin_path.chmod(bin_path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    info(f"Binary installed at {bin_path}")


def setup_systemd_service(
    workdir: Path, service_name: str, grpc_port: int,
    worker_id: str, worker_secret: str,
) -> None:
    step(8, "Setup systemd service")

    bin_path = workdir / "bin" / "onlyboxes-worker-docker"
    unit = f"""\
[Unit]
Description=Onlyboxes Worker Docker
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

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

[Install]
WantedBy=multi-user.target
"""

    service_path = Path(f"/etc/systemd/system/{service_name}.service")
    try:
        service_path.write_text(unit)
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


def wait_worker_online(
    opener: urllib.request.OpenerDirector,
    http_port: int, worker_id: str, service_name: str,
) -> None:
    step(9, "Waiting for worker to come online")

    url = f"http://127.0.0.1:{http_port}/api/v1/workers?status=online&page=1&page_size=100"
    deadline = time.monotonic() + WORKER_ONLINE_TIMEOUT
    while time.monotonic() < deadline:
        try:
            result = api_request(opener, url)
            workers = result.get("items") or []
            for w in workers:
                if w.get("node_id") == worker_id:
                    info("Worker is online!")
                    return
        except SystemExit:
            pass
        time.sleep(POLL_INTERVAL)

    fatal(
        "worker-online",
        f"Worker {worker_id} did not come online within {WORKER_ONLINE_TIMEOUT}s.\n"
        f"  Check status:  systemctl status {service_name}\n"
        f"  Check logs:    journalctl -u {service_name} -n 200 --no-pager",
    )


def print_summary(
    http_port: int, admin_password: str,
    worker_id: str, worker_secret: str, service_name: str,
) -> None:
    print(
        f"""
{'=' * 56}
  Installation complete
{'=' * 56}

  Console:        http://127.0.0.1:{http_port}
  Username:       admin
  Password:       {admin_password}
  Worker ID:      {worker_id}
  Worker Secret:  {worker_secret}

  ⚠ Save the password and worker secret now.
    They cannot be retrieved later.

  Start service:
    systemctl start {service_name}

  Stop service:
    systemctl stop {service_name}

  Service status:
    systemctl status {service_name}

  View logs:
    journalctl -u {service_name} -n 200 --no-pager

  Uninstall service:
    systemctl stop {service_name}
    systemctl disable {service_name}
    rm /etc/systemd/system/{service_name}.service
    systemctl daemon-reload
"""
    )


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Onlyboxes installer – deploy console + worker-docker on a single Linux host."
    )
    parser.add_argument("--tag", required=True, help="Release version tag, e.g. v0.1.0")
    parser.add_argument("--workdir", default=None, help="Working directory (default: $PWD/onlyboxes)")
    parser.add_argument("--yes", "-y", action="store_true", help="Non-interactive mode, skip confirmations")
    parser.add_argument("--console-http-port", type=int, default=8089, help="Console HTTP port (default: 8089)")
    parser.add_argument("--console-grpc-port", type=int, default=50051, help="Console gRPC port (default: 50051)")
    parser.add_argument("--service-name", default="onlyboxes-worker-docker", help="systemd service name (default: onlyboxes-worker-docker)")
    return parser.parse_args()


def main() -> None:
    args = parse_args()

    workdir = Path(args.workdir) if args.workdir else Path.cwd() / "onlyboxes"
    workdir = workdir.resolve()

    tag = args.tag
    http_port = args.console_http_port
    grpc_port = args.console_grpc_port
    service_name = args.service_name

    admin_password = generate_password()
    hash_key = generate_hash_key()

    print(f"Onlyboxes Installer")
    print(f"  Tag:       {tag}")
    print(f"  Workdir:   {workdir}")
    print(f"  HTTP port: {http_port}")
    print(f"  gRPC port: {grpc_port}")
    print(f"  Service:   {service_name}")

    if not args.yes and sys.stdin.isatty():
        try:
            answer = input("\nProceed? [Y/n] ").strip().lower()
        except (EOFError, KeyboardInterrupt):
            print()
            sys.exit(1)
        if answer and answer not in ("y", "yes"):
            print("Aborted.")
            sys.exit(0)

    # Step 1
    check_environment()

    # Step 2
    prepare_workdir(workdir, service_name)

    # Step 3
    download_and_render_compose(workdir, tag, hash_key, admin_password, http_port, grpc_port)

    # Step 4
    start_console(workdir, http_port, admin_password)

    # Step 5
    opener = login_and_save_cookie(workdir, http_port, admin_password)

    # Step 6
    worker_id, worker_secret = create_worker(opener, http_port)

    # Step 7
    download_worker_release(workdir, tag)

    # Step 8
    setup_systemd_service(workdir, service_name, grpc_port, worker_id, worker_secret)

    # Step 9
    wait_worker_online(opener, http_port, worker_id, service_name)

    # Cleanup
    shutil.rmtree(workdir / "install-artifacts", ignore_errors=True)

    # Step 10
    print_summary(http_port, admin_password, worker_id, worker_secret, service_name)


if __name__ == "__main__":
    main()
