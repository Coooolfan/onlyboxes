#!/usr/bin/env python3
"""Exercise real Console/S3 snapshot APIs on dev.sh's two BoxLite workers."""
import importlib.util
import atexit
import argparse
import shlex
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
    parser.add_argument("--scenario", choices=("cycle", "same-worker", "no-worker"), default="cycle")
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
    nodes = {dev.env_file(dev.STATE / (service + ".env"))["WORKER_ID"]: service for service in dev.SERVICES}
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
    def lifecycle(kind):
        status, result = api("POST", base + "/" + kind + query,
            {"retention_ttl_sec": 3600} if kind == "suspend" else {"lease_ttl_sec": 900}, str(uuid.uuid4()))
        require(status, result, 202)
        path = base + "/operations/" + result["operation_id"] + query
        deadline = time.monotonic() + 600
        phase = None
        while time.monotonic() < deadline:
            operation = require(*api("GET", path))
            if operation.get("phase") != phase:
                phase = operation.get("phase")
                print(kind + " phase=" + str(phase), flush=True)
            if operation["status"] in ("succeeded", "failed", "canceled"):
                record(kind, status=operation["status"], error=operation.get("error"))
                if operation["status"] != "succeeded":
                    raise RuntimeError("lifecycle operation failed; evidence retained")
                return
            time.sleep(1)
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
    if scenario == "same-worker":
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
