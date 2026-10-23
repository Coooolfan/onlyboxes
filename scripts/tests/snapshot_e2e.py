#!/usr/bin/env python3
"""Exercise real Console/S3 snapshot APIs on dev.sh's two BoxLite workers."""
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
import time
import urllib.error
import urllib.request
import uuid

SPEC = importlib.util.spec_from_file_location("dev_sandbox", Path(__file__).resolve().parents[1] / "dev-sandbox.py")
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=("cycle", "same-worker", "no-worker", "console-restart", "worker-restart", "worker-start-restart"), default="cycle")
    scenario = parser.parse_args().scenario
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
    evidence = {"session_id": sid, "account_id": account, "sha256": digest, "checks": []}
    def record(label, **values):
        evidence["checks"].append({"check": label, **values})
        dev.atomic_private(dev.STATE / ("snapshot-e2e-" + scenario + "-result.json"), json.dumps(evidence, indent=2) + "\n")
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
    def native_operation(operation_id):
        for node, service in nodes.items():
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
        node, native = native_operation(operation_id)
        if kind in restarted or not native:
            return
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
    def restart_during_native_action(kind, operation_id, parameters, request_key):
        node, native = native_operation(operation_id)
        if not native or kind in restarted:
            return
        eligible = (kind == "suspend" and native.get("phase") == "preparing" and native.get("source_stop_confirmed")) or (kind == "resume" and native.get("phase") == "importing" and native.get("start_attempted") and native.get("box_id"))
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
    def lifecycle(kind, recovery_retry=False):
        parameters = {"retention_ttl_sec": 3600} if kind == "suspend" else {"lease_ttl_sec": 900}
        request_key = str(uuid.uuid4())
        status, result = api("POST", base + "/" + kind + query, parameters, request_key)
        require(status, result, 202)
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
                    if scenario == "worker-start-restart" and kind == "resume" and not recovery_retry and "resume" in restarted:
                        finish_failed_start(operation, result["operation_id"])
                        return
                    raise RuntimeError("lifecycle operation failed; evidence retained")
                if fault_expected:
                    if kind not in restarted:
                        raise RuntimeError("operation completed without exercising the intended restart window")
                    _, native = native_operation(result["operation_id"])
                    if not native or native["box_id"] != restarted[kind][1]:
                        raise RuntimeError("recovery replaced the selected native instance")
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
    lifecycle("suspend")
    suspended = session()
    if suspended["lifecycle_state"] != "suspended" or suspended["worker_id"] is not None or not suspended["persistence"]["latest_snapshot"]:
        raise RuntimeError("snapshot was not durably suspended")
    if scenario in ("same-worker", "console-restart", "worker-restart", "worker-start-restart"):
        lifecycle("resume")
        verify("same-worker", source)
        require(*api("DELETE", base + query), expected=204)
        record("deleted", status=204)
        wait_cleanup()
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
    services("start", *dev.SERVICES)
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        workers = require(*api("GET", "/api/v1/workers?status=all"))["items"]
        if all(any(w["node_id"] == node and w["status"] == "online" for w in workers) for node in nodes):
            break
        time.sleep(1)
    else:
        raise RuntimeError("workers did not reconnect")
    lifecycle("resume")
    verify("resume-after-no-worker")
    require(*api("DELETE", base + query), expected=204)
    record("deleted", status=204)
    wait_cleanup()
    require(*api("DELETE", "/api/v1/tokens/" + execution["id"]), expected=204)


if __name__ == "__main__":
    main()
