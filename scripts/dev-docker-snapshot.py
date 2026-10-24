#!/usr/bin/env python3
"""Internal dev.sh adapter for two isolated Docker persistence workers."""
import argparse
import fcntl
import hashlib
import importlib.util
import json
import os
import platform
from pathlib import Path
import shutil
import socket
import stat
import subprocess
import sys

SPEC = importlib.util.spec_from_file_location("dev_sandbox", Path(__file__).with_name("dev-sandbox.py"))
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)
SERVICES = ("worker-docker-snapshot", "worker-docker-snapshot-peer")

# Never return specifications, environment values or the private guard token.
EVIDENCE_CODE = """import hashlib,json,sys
import os
from pathlib import Path
j=Path(sys.argv[1]); assert j.is_dir() and not j.is_symlink() and j.stat().st_mode&0o077==0
p=j/'journal.json'
assert p.is_file() and not p.is_symlink() and p.stat().st_size<=64*1024*1024 and p.stat().st_mode&0o077==0 and p.stat().st_uid==os.geteuid()
s=json.loads(p.read_text()); assert s['worker_id']==sys.argv[3]
op=s['operations'].get(sys.argv[2]); guard=s.get('lease_guard')
view=None if op is None else {k:op.get(k) for k in ('operation_id','scoped_session_id','generation','kind','phase','container_id','image_id','verified')}
if view is not None:
    view['box_id']=view['container_id']; name=op.get('artifact') or ''; assert not name or Path(name).name==name
    prefix='snapshot-'+hashlib.sha256(sys.argv[2].encode()).hexdigest()+'-'
    artifacts=j/'artifacts'; assert not artifacts.is_symlink()
    view['known_artifact_count']=sum(1 for p in artifacts.iterdir() if (name and p.name in (name,name+'.partial')) or p.name.startswith(prefix)) if artifacts.is_dir() else 0
g=None if not guard else {k:guard.get(k) for k in ('helper_id','state_id','engine_id','image_id','volume_name','volume_created')}
if g is not None: g['binding_sha256']=hashlib.sha256(json.dumps(guard,sort_keys=True,separators=(',',':')).encode()).hexdigest()
print(json.dumps({'operation':view,'guard':g}))
"""


def inspect(kind, name):
    result = subprocess.run(["docker", kind, "inspect", name], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if result.returncode:
        return None
    return json.loads(result.stdout)[0]


def build():
    if sys.platform == "darwin" and not shutil.which("orb"):
        dev.fail("需要已安装且已启动的 OrbStack，不会自动安装或启动桌面应用")
    endpoint = os.environ.get("DOCKER_HOST")
    if not endpoint:
        context = dev.run(["docker", "context", "show"])
        endpoint = dev.run(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}", context])
    if not endpoint.startswith("unix://"):
        dev.fail("本机 Docker 持久化验收不接受远程 daemon")
    if sys.platform == "darwin" and "OrbStack" not in dev.run(["docker", "info", "--format", "{{.OperatingSystem}}"]):
        dev.fail("macOS 本机 Docker 验收需要运行中的 OrbStack daemon")
    arch = dev.run(["docker", "info", "--format", "{{.Architecture}}"])
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(arch)
    if not arch:
        dev.fail("当前 Docker daemon 架构不受支持")
    if sys.platform.startswith("linux") and {"aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine(), platform.machine()) != arch:
        dev.fail("Linux 本机 Worker 与 Docker daemon 架构必须一致")
    dev.private_dir(dev.STATE)
    context = dev.STATE / "docker-persistence-build"
    dev.private_dir(context)
    with (dev.STATE / "docker-persistence-build.lock").open("a+") as lock:
        os.chmod(lock.name, 0o600)
        fcntl.flock(lock, fcntl.LOCK_EX)
        environment = os.environ.copy()
        environment.update({"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch})
        for binary, package in (("worker-docker", "./cmd/worker-docker"), ("runtime-lease-guard", "./cmd/runtime-lease-guard")):
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-o", str(context / binary), package], cwd=dev.ROOT / "worker/worker-docker", env=environment, check=True)
        dev.atomic_private(context / "Dockerfile", 'FROM scratch\nLABEL onlyboxes.component="runtime-lease-guard" onlyboxes.runtime-lease-guard.schema="2"\nCOPY runtime-lease-guard /runtime-lease-guard\nUSER 0:0\nENTRYPOINT ["/runtime-lease-guard"]\n')
        dev.atomic_private(context / ".dockerignore", "worker-docker\n")
        # Keep immutable tags for identical binaries. BuildKit provenance can
        # otherwise replace an index digest on an identical repeated build.
        digest = hashlib.sha256((context / "runtime-lease-guard").read_bytes()).hexdigest()
        tag = "onlyboxes-dev-lease:" + digest
        if not inspect("image", tag):
            subprocess.run(["docker", "build", "--network", "none", "--provenance=false", "-t", tag, str(context)], check=True)
        image = dev.run(["docker", "image", "inspect", "--format", "{{.Id}}", tag])
    return context / "worker-docker", image


def node_volume(service, values, root):
    name = "onlyboxes-snapshot-node-" + dev.NAMESPACE + "-" + values["WORKER_ID"]
    labels = {"onlyboxes.dev.namespace": dev.NAMESPACE, "onlyboxes.dev.service": service, "onlyboxes.worker_id": values["WORKER_ID"]}
    binding = root / "docker-volume.json"
    raw = dev.read_private(binding)
    saved = json.loads(raw) if raw else None
    volume = inspect("volume", name)
    if not volume:
        if saved:
            dev.fail("已绑定 Docker 持久卷丢失，保留身份并拒绝空卷替代")
        args = ["docker", "volume", "create"]
        for key, value in labels.items():
            args += ["--label", key + "=" + value]
        dev.run(args + [name])
        volume = inspect("volume", name)
    if not volume or any((volume.get("Labels") or {}).get(key) != value for key, value in labels.items()) or volume.get("Driver") != "local" or volume.get("Options"):
        dev.fail("Docker 持久卷身份或配置不匹配")
    identity = {key: volume[key] for key in ("Name", "CreatedAt", "Mountpoint", "Driver")}
    if saved and saved != identity:
        dev.fail("Docker 持久卷被替换，拒绝重新绑定")
    if not Path(identity["Mountpoint"]).is_absolute():
        dev.fail("Docker 持久卷路径不是绝对路径")
    dev.atomic_private(binding, json.dumps(identity, separators=(",", ":")) + "\n")
    return name, identity["Mountpoint"]


def retained_volume(service, values, root):
    """Read-only lookup: evidence and fault injection cannot create/rebind a volume."""
    saved = json.loads(dev.read_private(root / "docker-volume.json") or "null")
    name = "onlyboxes-snapshot-node-" + dev.NAMESPACE + "-" + values["WORKER_ID"]
    current = inspect("volume", name)
    labels = {"onlyboxes.dev.namespace":dev.NAMESPACE, "onlyboxes.dev.service":service,
              "onlyboxes.worker_id":values["WORKER_ID"]}
    if (not saved or not current or saved != {key:current.get(key) for key in ("Name","CreatedAt","Mountpoint","Driver")}
            or saved["Name"] != name or current.get("Driver") != "local" or current.get("Options")
            or any((current.get("Labels") or {}).get(key) != value for key,value in labels.items())):
        dev.fail("Docker 持久卷缺失或身份不匹配，拒绝读取或故障注入")
    return saved


def snapshot_evidence(service, values, operation):
    root = dev.STATE / service / hashlib.sha256(values["WORKER_ID"].encode()).hexdigest()[:32]
    if sys.platform.startswith("linux"):
        command = [sys.executable, "-c", EVIDENCE_CODE, str(root / "journal"), operation, values["WORKER_ID"]]
    else:
        binding = retained_volume(service, values, root)
        image = os.environ.get("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", "")
        if "@sha256:" not in image:
            dev.fail("Docker 证据读取需要固定镜像 digest")
        command = ["docker", "run", "--rm", "--read-only", "--network", "none", "--user", "0:0", "--mount",
                   "type=volume,source=" + binding["Name"] + ",target=/snapshot-state,readonly",
                   "--entrypoint", "python3", image, "-c", EVIDENCE_CODE, "/snapshot-state/journal", operation, values["WORKER_ID"]]
    result = subprocess.run(command, capture_output=True, text=True, timeout=15)
    if result.returncode:
        dev.fail("Docker journal 证据读取失败，保留现场")
    return json.loads(result.stdout)


def crash(service):
    values = dev.env_file(dev.STATE / (service + ".env"))
    if not values.get("WORKER_ID"):
        dev.fail("当前状态目录没有此 Docker Worker 身份")
    if sys.platform.startswith("linux"):
        return dev.crash_worker(service)
    root = dev.STATE / service / hashlib.sha256(values["WORKER_ID"].encode()).hexdigest()[:32]
    binding = retained_volume(service, values, root)
    name = "onlyboxes-snapshot-worker-" + dev.NAMESPACE + "-" + values["WORKER_ID"]
    worker = inspect("container", name)
    labels = {"onlyboxes.dev.namespace":dev.NAMESPACE, "onlyboxes.dev.service":service,
              "onlyboxes.worker_id":values["WORKER_ID"]}
    if (not worker or worker.get("Name") != "/" + name or worker.get("Path") != "/worker-docker"
            or not worker.get("State", {}).get("Running") or not worker.get("HostConfig", {}).get("AutoRemove")
            or any((worker.get("Config", {}).get("Labels") or {}).get(key) != value for key,value in labels.items())
            or [entry for entry in worker.get("Config", {}).get("Env", []) if entry.startswith("WORKER_ID=")] != ["WORKER_ID=" + values["WORKER_ID"]]
            or not any(mount.get("Type") == "volume" and mount.get("Name") == binding["Name"]
                       and mount.get("Destination") == binding["Mountpoint"] for mount in worker.get("Mounts", []))):
        dev.fail("Docker Worker 容器归属不匹配，拒绝故障注入")
    container = worker.get("Id", "")
    if len(container) != 64 or any(c not in "0123456789abcdef" for c in container):
        dev.fail("Docker Worker 不可变 ID 无效")
    _root, control = dev.node_paths(service, values)
    endpoint = control / "worker.sock"
    info = endpoint.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        dev.fail("Worker 本机控制端点异常")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(20)
        client.connect(str(endpoint))
        # Keep the docker run CLI alive. Its owned supervisor confirms the
        # container's SIGKILL exit instead of merely killing a client process.
        client.sendall(b"wait\n")
        with client.makefile("rb") as stream:
            if stream.readline(17) != b"waiting\n":
                dev.fail("Worker 管理进程未接管退出等待，拒绝发送 SIGKILL")
            dev.run(["docker", "kill", "--signal", "KILL", container])
            raw = stream.readline(4097)
        if len(raw) > 4096:
            dev.fail("Worker 退出回执超限")
        receipt = json.loads(raw)
    if receipt.get("returncode") != 137 or type(receipt.get("pid")) is not int or receipt["pid"] <= 1:
        dev.fail("未确认 Docker Worker 的 SIGKILL 退出，保留现场")
    print(json.dumps({"container_id":container, "pid":receipt["pid"], "returncode":137}), flush=True)


def worker(service):
    binary, guard = build()
    values = dev.provision(service)
    root, control = dev.node_paths(service, values)
    native = sys.platform.startswith("linux")
    volume, mountpoint = (None, str(root)) if native else node_volume(service, values, root)
    selected = os.environ.get("WORKER_TERMINAL_EXEC_DOCKER_IMAGE")
    if not selected or "@sha256:" not in selected:
        dev.fail("Docker 持久化验收需要 WORKER_TERMINAL_EXEC_DOCKER_IMAGE 固定 digest")
    environment = {key: value for key, value in dev.worker_environment().items() if key.startswith("WORKER_")}
    environment.update(values)
    environment.update({
        "WORKER_CONSOLE_GRPC_TARGET": os.environ.get("ONLYBOXES_DEV_DOCKER_CONSOLE_HOST", "host.docker.internal" if sys.platform == "darwin" else "127.0.0.1") + ":" + os.environ.get("CONSOLE_GRPC_ADDR", ":50051").rsplit(":", 1)[-1],
        "WORKER_CONSOLE_INSECURE": "true", "WORKER_NODE_NAME": service,
        "WORKER_PROXY_ENABLED": "false", "WORKER_PERSISTENCE_ENABLED": "true",
        "WORKER_SNAPSHOT_TMP_DIR": mountpoint + "/journal",
        "WORKER_SNAPSHOT_DOCKER_HELPER_IMAGE": selected,
        "WORKER_SNAPSHOT_LEASE_GUARD_IMAGE": guard,
        "WORKER_SNAPSHOT_LEASE_GUARD_DOCKER_SOCKET": "/var/run/docker.sock",
    })
    config = root / "worker-container.env"
    dev.write_env(config, environment)
    if native:
        process_environment = dev.worker_environment()
        process_environment.update(environment)
        print("• " + service + " 启动（Linux 原生进程 / 独立 journal / lease store）", flush=True)
        return dev.supervise_worker([str(binary)], process_environment, root, control)
    image = os.environ.get("ONLYBOXES_DEV_DOCKER_WORKER_IMAGE", "docker:29-cli")
    if not inspect("image", image):
        subprocess.run(["docker", "pull", image], check=True)
    image = dev.run(["docker", "image", "inspect", "--format", "{{.Id}}", image])
    name = "onlyboxes-snapshot-worker-" + dev.NAMESPACE + "-" + values["WORKER_ID"]
    existing = inspect("container", name)
    if existing:
        dev.fail("当前 Worker 容器仍存在，保留现场并拒绝重复启动")
    command = ["docker", "run", "--rm", "--name", name, "--network", "host",
        "--label", "onlyboxes.dev.namespace=" + dev.NAMESPACE,
        "--label", "onlyboxes.dev.service=" + service,
        "--label", "onlyboxes.worker_id=" + values["WORKER_ID"],
        "--env-file", str(config),
        "--mount", "type=bind,source=" + str(binary) + ",target=/worker-docker,readonly",
        "--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
        "--mount", "type=volume,source=" + volume + ",target=" + mountpoint,
        "--entrypoint", "/worker-docker", image]
    print("• " + service + " 启动（独立凭据 / journal 卷 / lease store）", flush=True)
    return dev.supervise_worker(command, dev.worker_environment(), root, control)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("worker", "crash"))
    parser.add_argument("service", choices=SERVICES)
    args = parser.parse_args()
    return worker(args.service) if args.command == "worker" else crash(args.service)


if __name__ == "__main__":
    try:
        sys.exit(main() or 0)
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
        print("错误：" + str(error), file=sys.stderr)
        sys.exit(1)
