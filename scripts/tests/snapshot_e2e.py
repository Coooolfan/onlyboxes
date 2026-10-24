#!/usr/bin/env python3
"""Exercise real Console/S3 snapshot APIs on dev.sh's two same-runtime workers."""
import importlib.util
import atexit
import argparse
import shlex
import hashlib
import sqlite3
import socket
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

SPEC = importlib.util.spec_from_file_location("dev_sandbox", Path(__file__).resolve().parents[1] / "dev-sandbox.py")
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", choices=("boxlite", "docker"), default="boxlite")
    parser.add_argument("--return-to-source", action="store_true", help="after cross-worker mutation and no-worker failure, restore onto the original worker with its older cache")
    parser.add_argument("--scenario", choices=("cycle", "same-worker", "no-worker", "console-restart", "worker-restart", "worker-start-restart", "cache-corrupt", "cache-miss"), default="cycle")
    args = parser.parse_args()
    scenario = args.scenario
    if args.return_to_source and scenario != "cycle":
        parser.error("--return-to-source requires --scenario cycle")
    if scenario.startswith("cache-") and args.runtime != "docker":
        parser.error("cache-corrupt/cache-miss currently require --runtime docker")
    if args.runtime == "docker":
        dev.SERVICES = ("worker-docker-snapshot", "worker-docker-snapshot-peer")
        spec = importlib.util.spec_from_file_location("docker_acceptance", Path(dev.__file__).with_name("dev-docker-snapshot.py"))
        docker = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(docker)
    config = Path(os.environ.get("ONLYBOXES_DEV_ENV", dev.ROOT / "scripts/dev.env"))
    os.environ.update(dev.env_file(config))
    opener, headers = dev.console_auth()
    fields = dev.credential_fields()
    if not fields.get("password"):
        raise RuntimeError("acceptance requires cached local Console login to create an execution token")
    dev.request_json(opener, "POST", "/api/v1/auth/login", {"username": fields["username"], "password": fields["password"]})
    execution = dev.request_json(opener, "POST", "/api/v1/tokens", {"name": "snapshot-e2e-" + uuid.uuid4().hex})
    def revoke_token():
        try:
            request = urllib.request.Request(dev.console_base() + "/api/v1/tokens/" + execution["id"], method="DELETE")
            with opener.open(request, timeout=10):
                pass
        except (OSError, ValueError):
            pass
    atexit.register(revoke_token)

    def api(method, path, value=None, key=None):
        request = urllib.request.Request(dev.console_base() + path, data=None if value is None else json.dumps(value).encode(),
            headers={**({"Authorization": "Bearer " + execution["token"]} if path.startswith("/api/v1/commands/") else ({} if path.startswith("/api/v1/tokens/") else headers)), "Content-Type": "application/json", **({"Idempotency-Key": key} if key else {})}, method=method)
        try:
            with opener.open(request, timeout=180) as response:
                return response.status, None if response.status == 204 else json.load(response)
        except urllib.error.HTTPError as error:
            body = json.load(error)
            return error.code, body

    def require(status, body, expected=200):
        if status != expected:
            # Server bodies may contain private execution output; report only error codes.
            raise RuntimeError(f"HTTP {status}, expected {expected}; code=" + str(body.get("code", body.get("error", {}).get("code") if isinstance(body.get("error"), dict) else body.get("error"))))
        return body

    account = require(*api("GET", "/api/v1/auth/session"))["account"]["account_id"]
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        credentials = {service: dev.env_file(dev.STATE / (service + ".env")) for service in dev.SERVICES}
        if all(values.get("WORKER_ID") for values in credentials.values()):
            break
        time.sleep(0.2)
    else:
        raise RuntimeError("isolated Worker credentials were not provisioned")
    nodes = {values["WORKER_ID"]: service for service, values in credentials.items()}
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
        if all(any(w["node_id"] == node and w["status"] == "online" for w in workers) for node in nodes):
            if any(w["status"] == "online" and w["node_id"] not in nodes for w in workers):
                raise RuntimeError("acceptance requires an isolated Console with only the two selected runtime workers online")
            break
        time.sleep(1)
    else:
        raise RuntimeError("both isolated workers must be online before acceptance")
    token = uuid.uuid4().hex
    command = "python3 -c " + shlex.quote(
        "from pathlib import Path; import os,hashlib; p=Path('/root/snapshot-e2e'); p.mkdir(); "
        f"b=bytes(range(256))*4096+{token!r}.encode(); (p/'payload').write_bytes(b); "
        "os.chmod(p/'payload',0o640); (p/'link').symlink_to('payload'); (p/'remove-me').write_text('delete after restore'); (p/'modified-file').write_text('first version'); os.sync(); print(hashlib.sha256(b).hexdigest())")
    first = require(*api("POST", "/api/v1/commands/terminal", {"command": command, "lease_ttl_sec": 900, "timeout_ms": 150000}))
    if first.get("exit_code") != 0:
        raise RuntimeError("initial command failed")
    sid, digest = first["session_id"], first["stdout"].strip()
    base = "/api/v1/sessions/" + sid
    query = "?account_id=" + account
    evidence = {"runtime": args.runtime, "return_to_source": args.return_to_source, "session_id": sid, "account_id": account, "sha256": digest, "checks": []}
    result_name = ("docker-" if args.runtime == "docker" else "") + scenario + ("-return-to-source" if args.return_to_source else "")
    def record(label, **values):
        evidence["checks"].append({"check": label, **values})
        dev.atomic_private(dev.STATE / ("snapshot-e2e-" + result_name + "-result.json"), json.dumps(evidence, indent=2) + "\n")
        print(label + ": " + json.dumps(values), flush=True)
    def session():
        return require(*api("GET", base + query))
    def wait_cleanup():
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            stats = require(*api("GET", "/api/v1/persistence/stats" + query))
            if all(stats.get(field) == 0 for field in ("pending_sessions", "pending_workers", "pending_snapshots")):
                record("cleanup-drained", pending_sessions=0, pending_workers=0, pending_snapshots=0)
                return
            time.sleep(1)
        raise RuntimeError("physical cleanup backlog did not drain; retained evidence requires inspection")
    restarted = {}
    target_fault = None
    target_guard = None
    def close_target_fault():
        nonlocal target_fault
        if target_fault is not None:
            if target_fault.poll() is None:
                target_fault.terminate()
            target_fault.wait(timeout=15)
            target_fault.stdout.close()
            target_fault = None
    atexit.register(close_target_fault)
    def native_operation(operation_id):
        for node, service in nodes.items():
            if args.runtime == "docker":
                observed = docker.snapshot_evidence(service, {"WORKER_ID":node}, operation_id)
                if observed["operation"]:
                    return node, observed["operation"]
                continue
            root = dev.STATE / service / hashlib.sha256(node.encode()).hexdigest()[:32]
            path = root / "journal/journal.json"
            if path.exists():
                state = json.loads(path.read_text())
                operation = state.get("operations", {}).get(operation_id)
                if operation:
                    return node, operation
        return None, None
    def guard_exchange(node, command, expected=None):
        values = dev.env_file(dev.STATE / (nodes[node] + ".env"))
        _root, control = dev.node_paths(nodes[node], values)
        request_id = str(uuid.uuid4())
        frame = json.dumps({"version": 2, "request_id": request_id, "worker_id": node,
            "store_id": values["WORKER_PERSISTENCE_LEASE_STORE_ID"], "expected": expected,
            "command": command}).encode()
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(3)
            client.connect(str(control / "control.sock"))
            client.sendall(len(frame).to_bytes(4, "big") + frame)
            with client.makefile("rb") as stream:
                size = int.from_bytes(stream.read(4), "big")
                if not 0 < size <= 16384:
                    raise RuntimeError("invalid independent guard discovery frame")
                reply = json.loads(stream.read(size))
        identity = reply.get("identity", {})
        if reply.get("request_id") != request_id or identity.get("worker_id") != node or identity.get("store_id") != values["WORKER_PERSISTENCE_LEASE_STORE_ID"]:
            raise RuntimeError("independent guard identity query was rejected")
        return reply
    def lease_identity(node):
        if args.runtime == "docker":
            observed = docker.snapshot_evidence(nodes[node], {"WORKER_ID":node}, "guard-evidence")["guard"]
            if not observed or not observed.get("state_id") or not observed.get("helper_id"):
                raise RuntimeError("Docker independent guard lacks durable identity")
            helper = docker.inspect("container", observed["helper_id"])
            if not helper or not helper.get("State", {}).get("Running") or helper.get("Image") != observed["image_id"]:
                raise RuntimeError("Docker independent guard is not running with its pinned image")
            return observed
        reply = guard_exchange(node, {"action": "ping"})
        if reply.get("reply", {}).get("type") != "ready":
            raise RuntimeError("independent guard is unavailable")
        return reply["identity"]
    def finish_failed_start(operation, operation_id):
        if not (operation.get("error") or {}).get("retryable"):
            raise RuntimeError("unknown-start failure was not reported as retryable")
        node, native = native_operation(operation_id)
        expected_id = restarted["resume"][1]
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            current = session()
            if current["lifecycle_state"] == "suspended" and not current.get("current_operation_id"):
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("failed-start cancellation did not release ownership")
        if current["persistence"]["latest_snapshot"] != suspended["persistence"]["latest_snapshot"]:
            raise RuntimeError("failed start changed the committed snapshot")
        _, native = native_operation(operation_id)
        if not native or native["phase"] != "canceled" or native["box_id"] != expected_id:
            raise RuntimeError("failed start lost native cancellation evidence")
        identity = lease_identity(node)
        response = guard_exchange(node, {"action": "retirement_status", "box_id": expected_id}, identity)
        receipt = response.get("reply", {})
        if receipt.get("type") != "retirement_status" or not (receipt.get("value") or {}).get("released"):
            raise RuntimeError("failed start lacks the independent exit/cleanup receipt")
        record("unknown-start-canceled", operation_id=operation_id, instance_id=expected_id,
            released=True, snapshot_retained=True)
        fresh = lifecycle("resume", recovery_retry=True)
        if fresh["operation_id"] == operation_id or fresh["generation"] <= operation["generation"]:
            raise RuntimeError("retry reused the failed restore operation or generation")
        _, selected = native_operation(fresh["operation_id"])
        if not selected or selected["box_id"] == expected_id:
            raise RuntimeError("retry reused the retired native instance")
        if session()["lifecycle_state"] != "running":
            raise RuntimeError("new resume attempt did not become running")
        record("resume-after-start-failure", failed_generation=operation["generation"], generation=fresh["generation"], fresh_operation=True, fresh_instance=True)
    def restart_worker_during_action(kind, operation_id):
        if args.runtime == "docker" and kind == "resume" and kind not in restarted:
            return restart_docker_target(operation_id)
        node, native = native_operation(operation_id)
        if kind in restarted or not native:
            return
        if args.runtime == "docker":
            return restart_docker_source(node, native, operation_id)
        writer_pid = None
        if kind == "suspend":
            if native.get("phase") != "preparing" or not native.get("source_stop_confirmed"):
                return
        else:
            if native.get("phase") != "importing" or not native.get("start_attempted") or native.get("started_process") or not native.get("boot_guard", {}).get("authorization"):
                return
            root = dev.STATE / nodes[node] / hashlib.sha256(node.encode()).hexdigest()[:32]
            box = root / "sdk-home/boxes" / native["box_id"]
            try:
                writer_pid = int((box / "shim.pid").read_text().splitlines()[0])
            except (OSError, ValueError, IndexError):
                return
            if os.uname().sysname == "Darwin":
                observed = subprocess.run(["ps", "-p", str(writer_pid), "-o", "comm="], capture_output=True, text=True)
                if observed.returncode != 0 or observed.stdout.strip() != str(box / "bin/boxlite-shim"):
                    return
            elif Path("/proc", str(writer_pid), "exe").resolve() != box / "bin/boxlite-shim":
                return
        guardian = lease_identity(node)
        services("crash", nodes[node])
        if lease_identity(node) != guardian:
            raise RuntimeError("Worker crash also replaced the independent guard")
        _, killed = native_operation(operation_id)
        if kind == "resume" and (not killed or killed.get("started_process")):
            raise RuntimeError("writer binding completed before SIGKILL; unknown-start window was not exercised")
        if kind == "resume":
            status = guard_exchange(node, {"action": "status", "box_id": native["box_id"]}, guardian).get("reply", {})
            boot = guard_exchange(node, {"action": "boot_status", "box_id": native["box_id"]}, guardian).get("reply", {})
            if status.get("type") != "status" or status.get("value") is not None or boot.get("type") != "boot_status" or not boot.get("value"):
                raise RuntimeError("guard binding completed before SIGKILL; unbound-boot window was not exercised")
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            pane = subprocess.run(["tmux", "display-message", "-p", "-t", os.environ.get("ONLYBOXES_DEV_SESSION", "onlyboxes-dev") + ":" + nodes[node], "#{pane_dead}"], capture_output=True, text=True)
            if pane.returncode != 0 or pane.stdout.strip() == "1":
                break
            time.sleep(0.05)
        else:
            raise RuntimeError("Worker supervisor did not finish after confirmed child death")
        offline(node)
        record("worker-crashed-during-" + kind, operation_id=operation_id, worker_id=node,
            instance_id=native["box_id"], guard_epoch_id=guardian["epoch_id"], writer_pid=writer_pid,
            writer_recorded=native.get("started_process") is not None)
        services("start", nodes[node])
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
            if any(w["node_id"] == node and w["status"] == "online" for w in workers):
                # A stale pre-crash online heartbeat is insufficient: the new
                # process must have re-established its supervisor endpoint too.
                values = dev.env_file(dev.STATE / (nodes[node] + ".env"))
                _root, control = dev.node_paths(nodes[node], values)
                if (control / "worker.sock").is_socket():
                    break
            time.sleep(0.2)
        else:
            raise RuntimeError("crashed Worker did not reconnect; preserve its native roots")
        if lease_identity(node) != guardian:
            raise RuntimeError("Worker restart replaced the independent guard")
        restarted[kind] = (operation_id, native["box_id"])
    def restart_docker_source(node, native, operation_id):
        if native.get("kind") != "source" or native.get("phase") != "preparing" or not native.get("box_id"):
            return
        source = docker.inspect("container", native["box_id"])
        if not source or not source.get("State", {}).get("Paused"):
            return
        helpers = dev.run(["docker", "ps", "--no-trunc", "--filter", "label=onlyboxes.snapshot.export_source=" + native["box_id"],
                           "--filter", "label=onlyboxes.snapshot.export_operation=" + operation_id, "--format", "{{.ID}}"])
        if len(helpers.splitlines()) != 1:
            return
        helper = docker.inspect("container", helpers.strip())
        if not helper or not helper.get("State", {}).get("Running"):
            return
        guardian = lease_identity(node)
        services("crash", nodes[node])
        if lease_identity(node) != guardian:
            raise RuntimeError("Docker Worker SIGKILL replaced its independent guard")
        _, killed = native_operation(operation_id)
        retained = docker.inspect("container", native["box_id"])
        if not killed or killed["phase"] != "preparing" or killed["box_id"] != native["box_id"] or not retained or not retained.get("State", {}).get("Paused"):
            raise RuntimeError("Docker export completed before SIGKILL or lost the frozen source; fault window was not exercised")
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            pane = subprocess.run(["tmux", "display-message", "-p", "-t", os.environ.get("ONLYBOXES_DEV_SESSION", "onlyboxes-dev") + ":" + nodes[node], "#{pane_dead}"], capture_output=True, text=True)
            if pane.returncode != 0 or pane.stdout.strip() == "1":
                break
            time.sleep(.05)
        else:
            raise RuntimeError("Docker Worker supervisor did not confirm exit")
        offline(node)
        record("worker-crashed-during-suspend", operation_id=operation_id, worker_id=node,
               instance_id=native["box_id"], export_helper_id=helpers.strip(), guard_binding_sha256=guardian["binding_sha256"], frozen=True)
        services("start", nodes[node])
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
            if any(w["node_id"] == node and w["status"] == "online" for w in workers):
                break
            time.sleep(.2)
        else:
            raise RuntimeError("Docker Worker did not reconnect after SIGKILL; retain source and journal")
        if lease_identity(node) != guardian:
            raise RuntimeError("Docker Worker restart replaced the independent guard or store")
        restarted["suspend"] = (operation_id, native["box_id"])
    def restart_docker_target(operation_id):
        if target_fault.poll() is None:
            return
        output = target_fault.stdout.read(16385)
        if target_fault.returncode != 0 or len(output) > 16384:
            raise RuntimeError("Docker start-triggered Worker crash did not return a confirmed exit receipt")
        receipt = json.loads(output.splitlines()[-1])
        cid = receipt.get("target_container_id")
        close_target_fault()
        node, native = native_operation(operation_id)
        instance = docker.inspect("container", cid) if cid else None
        state = (instance or {}).get("State", {})
        if node != source or not native or native.get("kind") != "target" or native.get("phase") != "importing" or native.get("box_id") != cid or not state.get("Running") or state.get("Paused"):
            raise RuntimeError("Docker target completed before SIGKILL or lost its started instance; fault window was not exercised")
        if lease_identity(node) != target_guard:
            raise RuntimeError("Docker target Worker crash replaced its independent guard")
        reply = subprocess.run(["docker", "exec", "-i", target_guard["helper_id"], "/runtime-lease-guard", "-mode", "control"],
            input=json.dumps({"action":"status", "entry":{"container_id":cid}}), capture_output=True, text=True, timeout=10)
        if reply.returncode:
            raise RuntimeError("Docker started target lacks an independent lease receipt")
        lease = json.loads(reply.stdout)
        if (lease.get("container_id") != cid or lease.get("scope") != native.get("scoped_session_id") or
                lease.get("generation") != native.get("generation") or lease.get("revoked") is not False or
                lease.get("expires_unix_ms", 0) <= int(time.time()*1000) or lease.get("revision", 0) <= 0):
            raise RuntimeError("Docker started target lease identity is invalid")
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            pane = subprocess.run(["tmux", "display-message", "-p", "-t", os.environ.get("ONLYBOXES_DEV_SESSION", "onlyboxes-dev") + ":" + nodes[node], "#{pane_dead}"], capture_output=True, text=True)
            if pane.returncode != 0 or pane.stdout.strip() == "1":
                break
            time.sleep(.05)
        else:
            raise RuntimeError("Docker target Worker supervisor did not confirm exit")
        offline(node)
        record("worker-crashed-during-resume", operation_id=operation_id, worker_id=node, instance_id=cid,
            native_phase=native["phase"], guard_binding_sha256=target_guard["binding_sha256"], lease_revision=lease["revision"], started=True)
        services("start", nodes[node])
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
            if any(w["node_id"] == node and w["status"] == "online" for w in workers):
                break
            time.sleep(.2)
        else:
            raise RuntimeError("Docker target Worker did not reconnect after SIGKILL")
        if lease_identity(node) != target_guard:
            raise RuntimeError("Docker target Worker restart replaced the independent guard")
        restarted["resume"] = (operation_id, cid)
    def restart_during_native_action(kind, operation_id, parameters, request_key):
        node, native = native_operation(operation_id)
        if not native or kind in restarted:
            return
        eligible = (kind == "suspend" and native.get("phase") == "preparing" and native.get("source_stop_confirmed")) or (kind == "resume" and native.get("phase") == "importing" and native.get("start_attempted") and native.get("box_id"))
        guardian = None
        if args.runtime == "docker":
            instance = docker.inspect("container", native["box_id"]) if native.get("box_id") else None
            state = (instance or {}).get("State", {})
            eligible = (
                (kind == "suspend" and native.get("kind") == "source" and native.get("phase") == "preparing" and state.get("Paused")) or
                (kind == "resume" and native.get("kind") == "target" and native.get("phase") == "importing" and native.get("verified") and not native.get("box_id") and not native.get("image_id")))
            if eligible:
                guardian = lease_identity(node)
        if not eligible:
            return
        services("stop", "console")
        try:
            database = Path(os.environ.get("CONSOLE_DB_PATH", "./db/onlyboxes-console.db"))
            if not database.is_absolute():
                database = dev.ROOT / "console" / database
            with sqlite3.connect(database.resolve().as_uri() + "?mode=ro", uri=True) as connection:
                status, phase = connection.execute("SELECT status,phase FROM sandbox_persistence_operations WHERE operation_id=?", (operation_id,)).fetchone()
            _, observed = native_operation(operation_id)
            record("console-stopped-during-" + kind, operation_id=operation_id, status=status, phase=phase,
                worker_id=node, instance_id=native["box_id"], native_phase=observed.get("phase") if observed else None)
        finally:
            services("start", "console")
        dev.console_auth()
        dev.request_json(opener, "POST", "/api/v1/auth/login", {"username": fields["username"], "password": fields["password"]})
        if status not in ("pending", "running"):
            raise RuntimeError("operation completed before Console interruption; fault window was not exercised")
        if guardian is not None:
            if lease_identity(node) != guardian:
                raise RuntimeError("Console restart replaced the Docker independent guard")
            record("console-restart-guard-" + kind, guard_binding_sha256=guardian["binding_sha256"], unchanged=True)
        replay_status, replay = api("POST", base + "/" + kind + query, parameters, request_key)
        if replay_status == 202:
            if replay.get("operation_id") != operation_id:
                raise RuntimeError("idempotent replay after Console restart created another operation")
        elif replay_status != 200 or replay.get("lifecycle_state") != ("suspended" if kind == "suspend" else "running"):
            raise RuntimeError("idempotent replay after Console restart failed")
        conflicting = {key: value + 1 for key, value in parameters.items()}
        conflict_status, _ = api("POST", base + "/" + kind + query, conflicting, request_key)
        if conflict_status != 409:
            raise RuntimeError("same idempotency key with different parameters was accepted after restart")
        record("restart-idempotency-" + kind, operation_id=operation_id, replay_status=replay_status, conflicting_status=conflict_status)
        restarted[kind] = (operation_id, native["box_id"])
    def lifecycle(kind, recovery_retry=False, capacity_retry=0):
        nonlocal target_fault, target_guard
        parameters = {"retention_ttl_sec": 3600} if kind == "suspend" else {"lease_ttl_sec": 900}
        retained = session()["persistence"]["latest_snapshot"] if kind == "resume" else None
        request_key = str(uuid.uuid4())
        watch_target = args.runtime == "docker" and scenario == "worker-start-restart" and kind == "resume" and not recovery_retry
        if watch_target:
            target_guard = lease_identity(source)
        status, result = api("POST", base + "/" + kind + query, parameters, request_key)
        require(status, result, 202)
        if watch_target:
            target_fault = subprocess.Popen([sys.executable, str(Path(docker.__file__)), "crash", nodes[source], "--on-start", result["operation_id"]],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        path = base + "/operations/" + result["operation_id"] + query
        deadline = time.monotonic() + 600
        phase = None
        while time.monotonic() < deadline:
            operation = require(*api("GET", path))
            if operation.get("phase") != phase:
                phase = operation.get("phase")
                print(kind + " phase=" + str(phase), flush=True)
            fault_expected = not recovery_retry and (scenario == "console-restart" or (scenario == "worker-restart" and kind == "suspend") or (scenario == "worker-start-restart" and kind == "resume"))
            if fault_expected and operation["status"] not in ("succeeded", "failed", "canceled"):
                if scenario in ("worker-restart", "worker-start-restart"):
                    restart_worker_during_action(kind, result["operation_id"])
                else:
                    restart_during_native_action(kind, result["operation_id"], parameters, request_key)
            if operation["status"] in ("succeeded", "failed", "canceled"):
                record(kind, status=operation["status"], error=operation.get("error"))
                if operation["status"] != "succeeded":
                    error = operation.get("error") or {}
                    if kind == "resume" and scenario in ("cycle", "same-worker", "no-worker") and error.get("code") == "no_capacity" and error.get("retryable") and capacity_retry < 3:
                        current = session()
                        if operation.get("phase") != "resolved" or current["lifecycle_state"] != "suspended" or current.get("current_operation_id") or current["persistence"]["latest_snapshot"] != retained:
                            raise RuntimeError("capacity refusal did not preserve the snapshot and release operation ownership")
                        record("capacity-retry", failed_operation_id=operation["operation_id"], generation=operation["generation"], code="no_capacity", snapshot_retained=True)
                        time.sleep(1)
                        fresh = lifecycle(kind, recovery_retry=recovery_retry, capacity_retry=capacity_retry+1)
                        if fresh["operation_id"] == operation["operation_id"] or fresh["generation"] <= operation["generation"]:
                            raise RuntimeError("capacity retry reused the failed operation or generation")
                        return fresh
                    if args.runtime == "boxlite" and scenario == "worker-start-restart" and kind == "resume" and not recovery_retry and "resume" in restarted:
                        finish_failed_start(operation, result["operation_id"])
                        return
                    raise RuntimeError("lifecycle operation failed; evidence retained")
                if fault_expected:
                    if kind not in restarted:
                        raise RuntimeError("operation completed without exercising the intended restart window")
                    _, native = native_operation(result["operation_id"])
                    expected_instance = restarted[kind][1]
                    if not native or not native.get("box_id") or (expected_instance and native["box_id"] != expected_instance):
                        raise RuntimeError("recovery replaced the selected native instance")
                    if args.runtime == "docker" and kind == "resume" and native.get("phase") != "active":
                        raise RuntimeError("Docker restore did not activate the original target operation")
                    record("restart-reconciled-" + kind, operation_id=result["operation_id"], instance_id=native["box_id"])
                return operation
            time.sleep(0.05 if fault_expected and kind not in restarted else 1)
        raise RuntimeError("operation still unresolved; preserve session and inspect operation")
    def services(action, *names):
        subprocess.run([str(dev.ROOT / "scripts/dev.sh"), action, *names], check=True)
    def offline(node):
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
            if not any(w["node_id"] == node and w["status"] == "online" for w in workers):
                return
            time.sleep(1)
        raise RuntimeError("source worker did not become offline")
    def verify_old_source_cache(node, operation_id):
        root = dev.STATE / nodes[node] / hashlib.sha256(node.encode()).hexdigest()[:32]
        code = """import hashlib,json,sys,time
from pathlib import Path
root=Path(sys.argv[1]); op=json.loads((root/'journal.json').read_text())['operations'][sys.argv[2]]
assert op['phase']=='finalized' and op['snapshot_committed'] and op['verified']
assert op['cache_until_unix_ms']>time.time()*1000
name=op['artifact']; assert name and Path(name).name==name
file=root/'artifacts'/name; assert file.is_file() and not file.is_symlink()
digest=hashlib.sha256(); size=0
with file.open('rb') as stream:
    for block in iter(lambda:stream.read(1048576),b''): digest.update(block); size+=len(block)
archive=op['archive']; assert size==archive['size_bytes'] and digest.hexdigest()==archive['sha256']
print(json.dumps({'snapshot_id':archive['snapshot_id'],'sha256':digest.hexdigest()}))
"""
        if args.runtime == "docker" and sys.platform == "darwin":
            binding = json.loads(dev.read_private(root / "docker-volume.json"))
            if binding["Name"] != "onlyboxes-snapshot-node-" + dev.NAMESPACE + "-" + node:
                raise RuntimeError("cache inspection volume does not belong to the source worker")
            command = ["docker", "run", "--rm", "--read-only", "--network", "none", "--mount", "type=volume,source=" + binding["Name"] + ",target=/snapshot-state,readonly", "--entrypoint", "python3", os.environ["WORKER_TERMINAL_EXEC_DOCKER_IMAGE"], "-c", code, "/snapshot-state/journal", operation_id]
        else:
            command = [sys.executable, "-c", code, str(root / "journal"), operation_id]
        result = subprocess.run(command, capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise RuntimeError("return-to-source requires the original verified cache to remain present")
        cached = json.loads(result.stdout)
        latest = session()["persistence"]["latest_snapshot"]
        if cached["snapshot_id"] != suspended["persistence"]["latest_snapshot"]["snapshot_id"] or cached["sha256"] != suspended["persistence"]["latest_snapshot"]["sha256"] or cached["snapshot_id"] == latest["snapshot_id"]:
            raise RuntimeError("source cache does not represent the older committed snapshot")
        record("old-cache-present", **cached, latest_snapshot_id=latest["snapshot_id"])
    mutated = False
    def verify(label, expected_worker=None):
        current = session()
        if current["lifecycle_state"] != "running" or (expected_worker and current["worker_id"] != expected_worker):
            raise RuntimeError("unexpected restored session route")
        check = "python3 -c " + shlex.quote(
            "from pathlib import Path; import hashlib,stat; p=Path('/root/snapshot-e2e'); "
            "assert stat.S_IMODE((p/'payload').stat().st_mode)==0o640; assert (p/'link').readlink()==Path('payload'); "
            + ("assert not (p/'remove-me').exists(); assert (p/'new-file').read_bytes()==b'created after restore'; assert (p/'modified-file').read_text()=='second version'; " if mutated else "assert (p/'remove-me').exists(); assert (p/'modified-file').read_text()=='first version'; ")
            + "print(hashlib.sha256((p/'link').read_bytes()).hexdigest())")
        result = require(*api("POST", "/api/v1/commands/terminal", {"command": check, "session_id": sid, "lease_ttl_sec": 900, "timeout_ms": 60000}))
        if result.get("exit_code") != 0 or result.get("stdout", "").strip() != digest or result.get("created"):
            raise RuntimeError("restored file/hash/mode/symlink/session identity check failed")
        record(label, worker_id=current["worker_id"], session_id=sid, sha256=digest)
        return current["worker_id"]

    source = session()["worker_id"]
    record("created", worker_id=source)
    original_suspend = lifecycle("suspend")
    suspended = session()
    if suspended["lifecycle_state"] != "suspended" or suspended["worker_id"] is not None or not suspended["persistence"]["latest_snapshot"]:
        raise RuntimeError("snapshot was not durably suspended")
    if scenario in ("same-worker", "console-restart", "worker-restart", "worker-start-restart", "cache-corrupt", "cache-miss"):
        stopped_peer = None
        cache_fault = scenario in ("cache-corrupt", "cache-miss")
        if args.runtime == "docker" and (scenario == "worker-start-restart" or cache_fault):
            stopped_peer = next(node for node in nodes if node != source)
            services("stop", nodes[stopped_peer])
            offline(stopped_peer)
        if cache_fault:
            _, native = native_operation(original_suspend["operation_id"])
            if not native or native.get("phase") != "finalized" or not native.get("box_id"):
                raise RuntimeError("cache fault requires the exact finalized source operation")
            roots = dev.run(["docker", "ps", "-a", "--no-trunc", "--filter", "id=" + native["box_id"], "--format", "{{.ID}}"])
            if roots:
                raise RuntimeError("source container still exists; refusing cache fault injection")
            latest = suspended["persistence"]["latest_snapshot"]
            def cache_probe(operation_id, action):
                return docker.cache_probe(nodes[source], {"WORKER_ID":source}, operation_id, action, latest["snapshot_id"], latest["sha256"])
            injected = cache_probe(original_suspend["operation_id"], "corrupt" if scenario == "cache-corrupt" else "evict")
            if scenario == "cache-corrupt" and injected.get("valid") is not False:
                raise RuntimeError("source cache corruption was not confirmed")
            if scenario == "cache-miss" and injected.get("present") is not False:
                raise RuntimeError("source cache eviction was not confirmed")
            record("cache-fault-injected", scenario=scenario, source_removed=True, **injected)
        restored = lifecycle("resume")
        if cache_fault:
            if session()["persistence"]["latest_snapshot"] != latest:
                raise RuntimeError("cache fallback changed the committed snapshot reference")
            valid = cache_probe(restored["operation_id"], "target-valid")
            if valid.get("valid") is not True:
                raise RuntimeError("restored target lacks the verified committed archive")
            record("cache-fallback-verified", **valid, snapshot_retained=True)
        verify("same-worker", source)
        if stopped_peer:
            services("start", nodes[stopped_peer])
        require(*api("DELETE", base + query), expected=204)
        record("deleted", status=204)
        wait_cleanup()
        if args.runtime == "docker" and scenario == "worker-restart":
            _, cleaned = native_operation(original_suspend["operation_id"])
            roots = dev.run(["docker", "ps", "-a", "--no-trunc", "--filter", "id=" + restarted["suspend"][1], "--format", "{{.ID}}"])
            if not cleaned or cleaned["known_artifact_count"] != 0 or roots:
                raise RuntimeError("Docker crash source or its known archive/temp files remain after deletion acknowledgement")
            remaining = dev.run(["docker", "ps", "-a", "--no-trunc", "--filter", "label=onlyboxes.snapshot.export_source=" + restarted["suspend"][1],
                                 "--filter", "label=onlyboxes.snapshot.export_operation=" + original_suspend["operation_id"], "--format", "{{.ID}}"])
            if remaining:
                raise RuntimeError("Docker crash export helper remains after deletion acknowledgement")
            record("crash-resource-cleanup", source_removed=True, known_artifacts=0, export_helpers=0)
        if args.runtime == "docker" and scenario == "worker-start-restart":
            _, cleaned = native_operation(restarted["resume"][0])
            cid = restarted["resume"][1]
            roots = dev.run(["docker", "ps", "-a", "--no-trunc", "--filter", "id=" + cid, "--format", "{{.ID}}"])
            if not cleaned or cleaned["known_artifact_count"] != 0 or roots:
                raise RuntimeError("Docker interrupted target or its known archive/temp files remain after deletion acknowledgement")
            reply = subprocess.run(["docker", "exec", "-i", target_guard["helper_id"], "/runtime-lease-guard", "-mode", "control"],
                input=json.dumps({"action":"status", "entry":{"container_id":cid}}), capture_output=True, text=True, timeout=10)
            if reply.returncode == 0 or json.loads(reply.stdout) != {"error":"not_found"}:
                raise RuntimeError("Docker interrupted target lease was not confirmed forgotten after deletion")
            record("crash-target-cleanup", target_removed=True, known_artifacts=0, lease_forgotten=True)
        return
    if scenario == "cycle":
        services("stop", nodes[source])
        offline(source)
        lifecycle("resume")
        target = verify("cross-worker", next(node for node in nodes if node != source))
        mutation = "python3 -c " + shlex.quote("from pathlib import Path; import os; p=Path('/root/snapshot-e2e'); (p/'new-file').write_bytes(b'created after restore'); (p/'modified-file').write_text('second version'); (p/'remove-me').unlink(); os.sync()")
        result = require(*api("POST", "/api/v1/commands/terminal", {"command": mutation, "session_id": sid, "lease_ttl_sec": 900, "timeout_ms": 60000}))
        if result.get("exit_code") != 0:
            raise RuntimeError("restored filesystem mutation failed")
        mutated = True
        record("mutated-restored-files", created=True, modified=True, deleted=True)
        lifecycle("suspend")
        lifecycle("resume")
        verify("same-worker", target)
        lifecycle("suspend")
        services("stop", nodes[target])
        offline(target)
    else:
        services("stop", *dev.SERVICES)
        for node in nodes:
            offline(node)
    status, body = api("POST", base + "/resume" + query, {"lease_ttl_sec": 900}, str(uuid.uuid4()))
    submission_status = status
    outcome_code = body.get("code")
    if status == 202:
        path = base + "/operations/" + body["operation_id"] + query
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            operation = require(*api("GET", path))
            if operation["status"] == "failed" and operation.get("error", {}).get("code") == "no_worker":
                outcome_code = "no_worker"
                break
            if operation["status"] in ("succeeded", "canceled", "failed"):
                raise RuntimeError("unexpected no-worker operation result")
            time.sleep(1)
    if submission_status not in (202, 503) or outcome_code != "no_worker":
        raise RuntimeError("no-worker result differs from expected contract: HTTP " + str(submission_status))
    if session()["lifecycle_state"] != "suspended":
        raise RuntimeError("no-worker attempt changed suspended state")
    record("no-worker", submission_status=submission_status, code=outcome_code, asynchronous=submission_status == 202)
    if args.return_to_source:
        verify_old_source_cache(source, original_suspend["operation_id"])
    recovery_nodes = {source: nodes[source]} if args.return_to_source else nodes
    services("start", *recovery_nodes.values())
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
        if all(any(w["node_id"] == node and w["status"] == "online" for w in workers) for node in recovery_nodes):
            break
        time.sleep(1)
    else:
        raise RuntimeError("workers did not reconnect")
    lifecycle("resume")
    verify("return-to-source" if args.return_to_source else "resume-after-no-worker", source if args.return_to_source else None)
    if args.return_to_source:
        services("start", *(service for node, service in nodes.items() if node not in recovery_nodes))
    require(*api("DELETE", base + query), expected=204)
    record("deleted", status=204)
    wait_cleanup()
    require(*api("DELETE", "/api/v1/tokens/" + execution["id"]), expected=204)


if __name__ == "__main__":
    main()
