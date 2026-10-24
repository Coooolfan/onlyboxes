#!/usr/bin/env python3
"""Internal dev.sh adapter for local snapshot storage and BoxLite workers."""
import argparse
import datetime
import fcntl
import hashlib
import hmac
import http.cookiejar
import http.client
import json
import os
from pathlib import Path
import secrets
import shlex
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent
STATE = Path(os.environ.get("ONLYBOXES_DEV_STATE_DIR", ROOT / "scripts/.dev")).resolve()
SERVICES = ("worker-boxlite", "worker-boxlite-peer")
MINIO_SOURCE = "github.com/minio/minio@RELEASE.2025-10-15T17-29-55Z"
NAMESPACE = hashlib.sha256(str(STATE).encode()).hexdigest()[:16]
STORE_NAME = "onlyboxes-snapshot-" + NAMESPACE


def fail(message):
    raise RuntimeError(message)


def private_dir(path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        fail("本机沙箱状态目录必须属于当前用户且权限为 700")


def read_private(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        fail("拒绝读取权限、链接或归属异常的本机凭据文件")
    if info.st_size > 65536:
        fail("本机凭据文件超出限制")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "r") as stream:
        return stream.read()


def atomic_private(path, text):
    private_dir(path.parent)
    read_private(path)
    fd, name = tempfile.mkstemp(prefix=".dev-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def env_file(path):
    raw = read_private(path)
    values = {}
    for line in (raw or "").splitlines():
        pieces = shlex.split(line, comments=True)
        if len(pieces) == 1 and "=" in pieces[0]:
            key, value = pieces[0].split("=", 1)
            values[key] = value
    return values


def write_env(path, values):
    atomic_private(path, "".join(f"{key}={shlex.quote(str(value))}\n" for key, value in sorted(values.items())))


def run(args, **kwargs):
    result = subprocess.run(args, check=False, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, **kwargs)
    if result.returncode:
        detail = result.stderr.strip()[-4000:]
        fail("本地命令失败：" + args[0] + (": " + detail if detail else ""))
    return result.stdout.strip()


def console_base():
    address = os.environ.get("CONSOLE_HTTP_ADDR", ":8089")
    return "http://127.0.0.1:" + address.rsplit(":", 1)[-1]


def request_json(opener, method, path, value=None, headers=None):
    data = None if value is None else json.dumps(value).encode()
    merged = {"Content-Type": "application/json", **(headers or {})}
    request = urllib.request.Request(console_base() + path, data=data, headers=merged, method=method)
    with opener.open(request, timeout=10) as response:
        return json.load(response)


def credential_fields():
    candidates = []
    cache = read_private(STATE / "console-creds.json")
    if cache:
        candidates.append(cache)
    log = STATE / "console.log"
    if log.exists():
        with log.open() as stream:
            for line in stream:
                if "console admin account initialized" in line:
                    candidates.insert(0, line)
    for line in candidates:
        try:
            values = json.loads(line)
        except ValueError:
            try:
                values = dict(piece.split("=", 1) for piece in shlex.split(line) if "=" in piece)
            except ValueError:
                continue
        if values.get("username") and values.get("password"):
            atomic_private(STATE / "console-creds.json", json.dumps(values, separators=(",", ":")) + "\n")
            return values
    return {}


def console_auth():
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        fields = credential_fields()
        key = os.environ.get("ONLYBOXES_DEV_CONSOLE_API_KEY") or os.environ.get("CONSOLE_INITIAL_ADMIN_API_KEY") or fields.get("api_key")
        if key:
            headers = {"Authorization": "Bearer " + key}
            try:
                request_json(opener, "GET", "/api/v1/auth/session", headers=headers)
                return opener, headers
            except (OSError, http.client.HTTPException, ValueError):
                pass
        if fields.get("username"):
            try:
                request_json(opener, "POST", "/api/v1/auth/login", {"username": fields["username"], "password": fields["password"]})
                return opener, {}
            except (OSError, http.client.HTTPException, ValueError):
                pass
        time.sleep(0.5)
    fail("Console 未就绪或缺少有效管理凭据，请检查 Console 服务及私有凭据缓存")


def provision(service):
    private_dir(STATE)
    path = STATE / (service + ".env")
    # The lock inode is retained across replacements of the credentials file.
    with (STATE / (service + ".provision.lock")).open("a+") as lock:
        os.chmod(lock.name, 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX)
        values = env_file(path)
        opener, headers = console_auth()
        found = False
        if values.get("WORKER_ID"):
            for page in range(1, 1001):
                response = request_json(opener, "GET", f"/api/v1/workers?status=all&page={page}&page_size=100", headers=headers)
                items = response.get("items", [])
                if any(item.get("node_id") == values["WORKER_ID"] for item in items):
                    found = True
                    break
                if len(items) < 100:
                    break
        if not found:
            created = request_json(opener, "POST", "/api/v1/workers", {"type": "normal"}, headers)
            if not created.get("node_id") or not created.get("worker_secret"):
                fail("Console 返回了无效 Worker 凭据")
            values = {"WORKER_ID": created["node_id"], "WORKER_SECRET": created["worker_secret"]}
            print(f"• 已创建 {service} 凭据（私有文件 {path.name}）", flush=True)
        if not values.get("WORKER_SECRET"):
            fail("当前 Worker 缺少 secret，保留既有身份与数据")
        write_env(path, values)
        return values


def node_paths(service, values):
    key = hashlib.sha256(values["WORKER_ID"].encode()).hexdigest()[:32]
    root = STATE / service / key
    private_dir(root)
    control = Path("/tmp") / ("obx-dev-" + hashlib.sha256((str(STATE) + service + key).encode()).hexdigest()[:20])
    return root, control


def build_boxlite():
    private_dir(STATE)
    with (STATE / "boxlite-build.lock").open("a+") as lock:
        os.chmod(lock.name, 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX)
        subprocess.run(["cargo", "build", "--locked", "--bins"], cwd=ROOT / "worker/worker-boxlite", check=True)
    return ROOT / "worker/worker-boxlite/target/debug"


def worker_environment():
    return {key: value for key, value in os.environ.items()
            if not key.startswith(("CONSOLE_", "MINIO_")) and key != "ONLYBOXES_DEV_CONSOLE_API_KEY"}


def guard_identity(control, worker, store=None, pid=None, timeout=1):
    """A private Ping confirms a live ready service; a socket inode does not."""
    try:
        parent = control.lstat()
        endpoint = control / "control.sock"
        info = endpoint.lstat()
        if (not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.geteuid() or parent.st_mode & 0o077
                or not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid()
                or info.st_mode & 0o077 or info.st_nlink != 1):
            return None
        request_id = str(uuid.uuid4())
        payload = json.dumps({"version": 2, "request_id": request_id, "worker_id": worker,
                              "store_id": store, "expected": None, "command": {"action": "ping"}}).encode()
        deadline = time.monotonic() + timeout
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            def remaining():
                left = deadline - time.monotonic()
                if left <= 0:
                    raise TimeoutError()
                client.settimeout(left)
            def receive(size):
                data = bytearray()
                while len(data) < size:
                    remaining()
                    part = client.recv(size - len(data))
                    if not part:
                        raise ValueError()
                    data.extend(part)
                return bytes(data)
            remaining()
            client.connect(str(endpoint))
            remaining()
            client.sendall(len(payload).to_bytes(4, "big") + payload)
            size = int.from_bytes(receive(4), "big")
            if not 0 < size <= 16384:
                return None
            def unique_object(pairs):
                result = {}
                for key, value in pairs:
                    if key in result:
                        raise ValueError()
                    result[key] = value
                return result
            reply = json.loads(receive(size), object_pairs_hook=unique_object)
        if (not isinstance(reply, dict) or set(reply) != {"version", "request_id", "identity", "reply"}
                or type(reply.get("version")) is not int or reply["version"] != 2
                or reply.get("request_id") != request_id):
            return None
        identity = reply.get("identity")
        if (not isinstance(identity, dict) or set(identity) != {"worker_id", "store_id", "epoch_id", "process"}
                or identity.get("worker_id") != worker):
            return None
        for key in ("store_id", "epoch_id"):
            parsed = uuid.UUID(identity.get(key, ""))
            if parsed.int == 0 or str(parsed) != identity[key]:
                return None
        process = identity.get("process")
        required = {"host_id", "boot_id", "process_namespace", "pid", "start_time"}
        if (not isinstance(process, dict) or not required <= set(process) <= required | {"writer_cgroup", "signal_version"}
                or type(process.get("pid")) is not int or not 1 < process["pid"] <= 2147483647
                or type(process.get("start_time")) is not int or not 0 < process["start_time"] < 1 << 64
                or process.get("writer_cgroup") is not None or process.get("signal_version") is not None
                or (pid is not None and process["pid"] != pid)
                or (store is not None and identity["store_id"] != store)
                or reply.get("reply") != {"type": "ready"}):
            return None
        for key in ("host_id", "boot_id"):
            parsed = uuid.UUID(process[key])
            if parsed.int == 0 or str(parsed) != process[key]:
                return None
        namespace = process["process_namespace"]
        if namespace != "host":
            pieces = namespace.split(":")
            if (len(pieces) != 3 or pieces[0] != "linux-pid" or not 0 <= int(pieces[1]) < 1 << 64
                    or not 0 < int(pieces[2]) < 1 << 64
                    or namespace != f"linux-pid:{int(pieces[1])}:{int(pieces[2])}"):
                return None
        return identity
    except (OSError, ValueError, TypeError, AttributeError, RecursionError):
        return None


class ManagedProcess:
    """Install signal forwarding before spawn and retain the direct child until exit."""
    def __init__(self, command, environment):
        self.command, self.environment = command, environment
        self.child, self.stopping, self.previous = None, False, {}

    def stop(self, _signal, _frame):
        self.stopping = True
        if self.child is not None and self.child.poll() is None:
            self.child.terminate()

    def __enter__(self):
        try:
            for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
                self.previous[sig] = signal.signal(sig, self.stop)
            if not self.stopping:
                self.child = subprocess.Popen(self.command, env=self.environment, start_new_session=True)
                if self.stopping and self.child.poll() is None:
                    self.child.terminate()
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def __exit__(self, *_exception):
        try:
            if self.child is not None and self.child.poll() is None:
                self.child.terminate()
                # No forced kill: an accepted native start or independent stop
                # can retain an unresolved outcome beyond a local stop request.
                self.child.wait()
        finally:
            for sig, handler in self.previous.items():
                signal.signal(sig, handler)


def lease(service):
    binaries = build_boxlite()
    values = provision(service)
    root, control = node_paths(service, values)
    state = root / "leases"
    expected = values.get("WORKER_PERSISTENCE_LEASE_STORE_ID")
    if state.joinpath("owner.json").exists() and not expected:
        fail("已有守护器账本缺少固定 store ID，保留数据并拒绝重新发现身份")
    args = [str(binaries / "boxlite-lease-guard"), "--state-dir", str(state), "--socket-dir", str(control), "--worker-id", values["WORKER_ID"]]
    if expected:
        args += ["--store-id", expected]
    with ManagedProcess(args, worker_environment()) as process:
        child = process.child
        if child is None:
            return 0
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if process.stopping:
                return child.wait()
            if child.poll() is not None:
                fail(f"独立租约守护器在就绪前退出（returncode={child.returncode}），未开放 Worker 启动")
            identity = guard_identity(control, values["WORKER_ID"], expected, child.pid,
                                      min(1, max(0, deadline - time.monotonic())))
            if identity and child.poll() is None and not process.stopping:
                values["WORKER_PERSISTENCE_LEASE_STORE_ID"] = identity["store_id"]
                write_env(STATE / (service + ".env"), values)
                print(f"• {service} 独立租约守护器已就绪", flush=True)
                return child.wait()
            time.sleep(0.1)
        fail("独立租约守护器未在期限内就绪")


def worker(service):
    binaries = build_boxlite()
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        values = env_file(STATE / (service + ".env"))
        if values.get("WORKER_PERSISTENCE_LEASE_STORE_ID"):
            root, control = node_paths(service, values)
            if guard_identity(control, values["WORKER_ID"], values["WORKER_PERSISTENCE_LEASE_STORE_ID"]):
                break
        time.sleep(0.2)
    else:
        fail("Worker 等待独立守护器就绪超时")
    environment = worker_environment()
    environment.update(values)
    selected = os.environ.get("WORKER_PERSISTENCE_ENABLED", os.environ.get("CONSOLE_PERSISTENCE_ENABLED", "false")).strip().lower()
    if selected not in ("true", "1", "yes", "on", "false", "0", "no", "off", ""):
        fail("WORKER_PERSISTENCE_ENABLED 不是有效布尔值")
    enabled = "true" if selected in ("true", "1", "yes", "on") else "false"
    environment.update({
        "WORKER_CONSOLE_GRPC_TARGET": "127.0.0.1:" + os.environ.get("CONSOLE_GRPC_ADDR", ":50051").rsplit(":", 1)[-1],
        "WORKER_CONSOLE_INSECURE": "true", "WORKER_NODE_NAME": service,
        "WORKER_BOXLITE_HOME": str(root / ("sdk-home" if enabled == "true" else "ephemeral-home")),
        "WORKER_PERSISTENCE_ENABLED": enabled, "WORKER_PERSISTENCE_JOURNAL_DIR": str(root / "journal"),
        "WORKER_PERSISTENCE_LEASE_STATE_DIR": str(root / "leases"), "WORKER_PERSISTENCE_LEASE_SOCKET_DIR": str(control),
        "WORKER_PERSISTENCE_PYTHON_HOME": str(root / "python-home"),
    })
    print(f"• {service} 启动（独立 home / journal / lease store）", flush=True)
    return supervise_worker([str(binaries / "worker-boxlite")], environment, root, control)


def supervise_worker(command, environment, root, control):
    """Retain the direct-child handle so fault injection never signals a PID lookup."""
    private_dir(control)
    lock_path = root / "worker-supervisor.lock"
    read_private(lock_path)
    with lock_path.open("a+") as lock:
        os.chmod(lock.name, 0o600)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            fail("当前 Worker 已有本机管理进程")
        endpoint = control / "worker.sock"
        if endpoint.exists() or endpoint.is_symlink():
            info = endpoint.lstat()
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid():
                fail("拒绝替换非本机 Worker 的控制端点")
            endpoint.unlink()
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            server.bind(str(endpoint))
            os.chmod(endpoint, 0o600)
            server.listen(4)
            server.settimeout(0.2)
            with ManagedProcess(command, environment) as process:
                return supervise_requests(server, process.child)
        finally:
            server.close()
            endpoint.unlink(missing_ok=True)


def supervise_requests(server, child):
    if child is None:
        return 0
    while child.poll() is None:
        try:
            connection, _ = server.accept()
        except socket.timeout:
            continue
        with connection:
            connection.settimeout(1)
            try:
                with connection.makefile("rb") as stream:
                    request = stream.readline(17)
                if request not in (b"kill\n", b"wait\n"):
                    connection.sendall(b"rejected\n")
                    continue
                # An unreaped direct child cannot have its PID recycled.
                # The command never targets a group, shim or lease guard.
                if request == b"kill\n":
                    child.kill()
                else:
                    connection.sendall(b"waiting\n")
                result = child.wait()
                connection.sendall(json.dumps({"pid": child.pid, "returncode": result}).encode() + b"\n")
            except (OSError, subprocess.SubprocessError):
                continue
    return child.wait()


def crash_worker(service):
    values = env_file(STATE / (service + ".env"))
    if not values.get("WORKER_ID"):
        fail("当前状态目录没有此 Worker 的本机身份")
    _root, control = node_paths(service, values)
    private_dir(control)
    endpoint = control / "worker.sock"
    info = endpoint.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        fail("Worker 本机控制端点权限或归属异常")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(5)
        client.connect(str(endpoint))
        client.sendall(b"kill\n")
        with client.makefile("rb") as stream:
            response = stream.readline(257)
    if len(response) > 256:
        fail("Worker 本机控制回执超出限制")
    receipt = json.loads(response)
    if receipt.get("returncode") != -signal.SIGKILL:
        fail("Worker 未确认以 SIGKILL 退出")
    print(f"• {service} 已确认 SIGKILL 退出", flush=True)


def docker_owned():
    result = subprocess.run(["docker", "inspect", STORE_NAME, "--format", "{{json .Config.Labels}}"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    if result.returncode:
        return False
    if json.loads(result.stdout).get("onlyboxes.dev.state") != NAMESPACE:
        fail("同名对象存储容器不属于当前编排状态目录")
    return True


def store_values():
    path = STATE / "snapshot-store.env"
    values = env_file(path)
    for key in ("CONSOLE_SNAPSHOT_STORE_AK", "CONSOLE_SNAPSHOT_STORE_SK"):
        if values.get(key) and os.environ.get(key) and values[key] != os.environ[key]:
            fail("本地 S3 配置与既有凭据不匹配，保留原容器和数据卷")
    ak = os.environ.get("CONSOLE_SNAPSHOT_STORE_AK") or values.get("CONSOLE_SNAPSHOT_STORE_AK")
    sk = os.environ.get("CONSOLE_SNAPSHOT_STORE_SK") or values.get("CONSOLE_SNAPSHOT_STORE_SK")
    if not ak and not sk:
        retained_volume = subprocess.run(["docker", "volume", "inspect", STORE_NAME + "-data"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
        if docker_owned() or retained_volume:
            fail("已有对象存储或数据卷缺少本机凭据文件，保留容器和数据")
        ak, sk = "obx" + secrets.token_hex(8), secrets.token_hex(32)
    if not ak or not sk:
        fail("本地对象存储 AK/SK 必须同时配置")
    port = os.environ.get("ONLYBOXES_DEV_SNAPSHOT_PORT", "19000")
    if not port.isdigit() or not 1 <= int(port) <= 65535:
        fail("本地对象存储端口无效")
    endpoint = f"http://127.0.0.1:{port}"
    if os.environ.get("CONSOLE_SNAPSHOT_STORE_ENDPOINT", endpoint) != endpoint:
        fail("本地 snapshot-store 仅使用指定的 loopback 端口")
    if values.get("CONSOLE_SNAPSHOT_STORE_ENDPOINT", endpoint) != endpoint:
        fail("已有本地 S3 端口配置不同，保留原服务")
    values.update({"CONSOLE_SNAPSHOT_STORE_AK": ak, "CONSOLE_SNAPSHOT_STORE_SK": sk,
                   "CONSOLE_SNAPSHOT_STORE_ENDPOINT": endpoint, "CONSOLE_SNAPSHOT_STORE_REGION": "us-east-1",
                   "CONSOLE_SNAPSHOT_STORE_BUCKET_NAME": os.environ.get("CONSOLE_SNAPSHOT_STORE_BUCKET_NAME", "onlyboxes-snapshots"),
                   "CONSOLE_SNAPSHOT_STORE_FORCE_PATH_STYLE": "true"})
    write_env(path, values)
    return values, port


def create_bucket(values):
    endpoint = urllib.parse.urlsplit(values["CONSOLE_SNAPSHOT_STORE_ENDPOINT"])
    bucket = values["CONSOLE_SNAPSHOT_STORE_BUCKET_NAME"]
    if not bucket or any(character not in "abcdefghijklmnopqrstuvwxyz0123456789-." for character in bucket):
        fail("本地快照 bucket 名称无效")
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    day = stamp[:8]
    payload_hash = hashlib.sha256(b"").hexdigest()
    uri = "/" + bucket
    names = "host;x-amz-content-sha256;x-amz-date"
    canonical = "\n".join(["PUT", uri, "", f"host:{endpoint.netloc}\nx-amz-content-sha256:{payload_hash}\nx-amz-date:{stamp}\n", names, payload_hash])
    scope = day + "/us-east-1/s3/aws4_request"
    content = "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hashlib.sha256(canonical.encode()).hexdigest()
    key = ("AWS4" + values["CONSOLE_SNAPSHOT_STORE_SK"]).encode()
    for piece in (day, "us-east-1", "s3", "aws4_request"):
        key = hmac.new(key, piece.encode(), hashlib.sha256).digest()
    signature = hmac.new(key, content.encode(), hashlib.sha256).hexdigest()
    headers = {"x-amz-date": stamp, "x-amz-content-sha256": payload_hash,
               "Authorization": f"AWS4-HMAC-SHA256 Credential={values['CONSOLE_SNAPSHOT_STORE_AK']}/{scope}, SignedHeaders={names}, Signature={signature}"}
    try:
        with urllib.request.urlopen(urllib.request.Request(endpoint.geturl() + uri, data=b"", method="PUT", headers=headers), timeout=10):
            pass
    except urllib.error.HTTPError as error:
        if error.code != 409:
            fail("创建本地快照 bucket 失败（HTTP " + str(error.code) + "）")


def wait_store(values):
    deadline = time.monotonic() + 60
    url = values["CONSOLE_SNAPSHOT_STORE_ENDPOINT"] + "/minio/health/ready"
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    return
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.2)
    fail("本地 S3 对象存储未就绪")


def store_image():
    configured = os.environ.get("ONLYBOXES_DEV_MINIO_IMAGE")
    if configured:
        return configured
    architecture = run(["docker", "info", "--format", "{{.Architecture}}"])
    architecture = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(architecture)
    if not architecture:
        fail("本地 S3 构建不支持当前 Docker 架构")
    image = "onlyboxes-dev-minio:20251015-" + architecture
    existing = subprocess.run(["docker", "image", "inspect", image, "--format", '{{index .Config.Labels "onlyboxes.dev.source"}}'], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    if existing.returncode == 0:
        if existing.stdout.strip() != MINIO_SOURCE:
            fail("本地 MinIO 构建镜像的源码标识不匹配")
        return image
    print("• 正在从固定官方 MinIO 源码构建本地验收镜像", flush=True)
    source = json.loads(run(["go", "mod", "download", "-json", MINIO_SOURCE]))
    context = STATE / "minio-build"
    private_dir(context)
    environment = dict(os.environ, GOOS="linux", GOARCH=architecture, CGO_ENABLED="0")
    run(["go", "build", "-mod=readonly", "-trimpath", "-o", str(context / "minio"), "."], cwd=source["Dir"], env=environment)
    (context / "Dockerfile").write_text('FROM scratch\nLABEL onlyboxes.dev.source="' + MINIO_SOURCE + '"\nCOPY minio /minio\nENTRYPOINT ["/minio"]\n')
    run(["docker", "build", "--tag", image, str(context)])
    return image


def store():
    private_dir(STATE)
    values, port = store_values()
    environment = os.environ.copy()
    environment.update({"MINIO_ROOT_USER": values["CONSOLE_SNAPSHOT_STORE_AK"], "MINIO_ROOT_PASSWORD": values["CONSOLE_SNAPSHOT_STORE_SK"]})
    if docker_owned():
        run(["docker", "start", STORE_NAME])
    else:
        run(["docker", "run", "--detach", "--name", STORE_NAME, "--label", "onlyboxes.dev.state=" + NAMESPACE,
             "--publish", f"127.0.0.1:{port}:9000", "--mount", "type=volume,src=" + STORE_NAME + "-data,dst=/data",
             "--env", "MINIO_ROOT_USER", "--env", "MINIO_ROOT_PASSWORD", "--env", "MINIO_BROWSER=off",
             store_image(), "server", "/data", "--quiet"], env=environment)
    wait_store(values)
    create_bucket(values)
    atomic_private(STATE / "snapshot-store.ready", STORE_NAME + "\n")
    print("• 本地 S3 对象存储与快照 bucket 已就绪（数据卷在停止后保留）", flush=True)
    stopping = []
    def stop(_signal, _frame):
        stopping.append(True)
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, stop)
    while not stopping:
        if run(["docker", "inspect", STORE_NAME, "--format", "{{.State.Running}} "]).strip() != "true":
            fail("本地 S3 容器已退出")
        time.sleep(0.5)
    stop_store()


def stop_store():
    if docker_owned():
        run(["docker", "stop", "--time", "10", STORE_NAME])
    try:
        (STATE / "snapshot-store.ready").unlink()
    except FileNotFoundError:
        pass


def console():
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        values = env_file(STATE / "snapshot-store.env")
        if values:
            wait_store(values)
            environment = {**values, **os.environ}
            # Store initialization creates the bucket before the readiness file.
            if (STATE / "snapshot-store.ready").exists():
                os.execvpe("go", ["go", "run", "./cmd/console"], environment)
        time.sleep(0.2)
    fail("Console 等待本地 S3 初始化超时")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("lease", "worker", "crash", "store", "console", "stop-store"))
    parser.add_argument("service", nargs="?", choices=SERVICES)
    args = parser.parse_args()
    if args.command in ("lease", "worker", "crash") and not args.service:
        parser.error("BoxLite service is required")
    private_dir(STATE)
    if args.command == "lease":
        return lease(args.service)
    if args.command == "worker":
        return worker(args.service)
    if args.command == "crash":
        return crash_worker(args.service)
    if args.command == "console":
        return console()
    if args.command == "stop-store":
        return stop_store()
    with (STATE / "snapshot-store.lock").open("a+") as lock:
        os.chmod(lock.name, 0o600)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            fail("当前状态目录已有对象存储管理进程")
        return store()


if __name__ == "__main__":
    try:
        sys.exit(main() or 0)
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
        print("错误：" + str(error), file=sys.stderr)
        sys.exit(1)
