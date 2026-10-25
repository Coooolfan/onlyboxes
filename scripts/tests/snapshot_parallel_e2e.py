#!/usr/bin/env python3
"""Four-session BoxLite Console/S3 acceptance; retains all evidence on failure."""
import argparse
import datetime
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import shlex
import sqlite3
import stat
import threading
import time
import urllib.error
import urllib.request
import uuid

SPEC = importlib.util.spec_from_file_location("dev_sandbox", Path(__file__).resolve().parents[1] / "dev-sandbox.py")
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)


def required_headroom(payload, expanded, concurrency):
    return concurrency * (8 * payload + 2 * expanded) + (4 << 30)


def peak_overlap(intervals):
    events = [(entry["started"], 1) for entry in intervals] + [(entry["finished"], -1) for entry in intervals]
    current = peak = 0
    for _, delta in sorted(events):
        current += delta
        peak = max(peak, current)
    return peak


def fresh_heartbeats(workers, nodes, after):
    indexed = {w["node_id"]:w for w in workers}
    try:
        for node in nodes:
            worker = indexed[node]
            seen = datetime.datetime.fromisoformat(worker["last_seen_at"].replace("Z", "+00:00"))
            if worker["status"] != "online" or seen.tzinfo is None or seen.timestamp() <= after:
                return False
        return True
    except (KeyError, ValueError, TypeError, AttributeError):
        return False


def read_journal(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 64 << 20:
            raise RuntimeError("parallel native journal type, ownership or size is invalid")
        data = stream.read((64 << 20) + 1)
        if len(data) > 64 << 20:
            raise RuntimeError("parallel native journal exceeds its byte limit")
    return json.loads(data)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--payload-mib", type=int, default=1024)
    parser.add_argument("--operation-wait-sec", type=int, default=1800)
    args = parser.parse_args()
    if not 1 <= args.payload_mib <= 10240 or not 1 <= args.operation_wait_sec <= 86400:
        parser.error("payload requires 1..10240 MiB and operation wait requires 1..86400 seconds")
    os.environ.update(dev.env_file(Path(os.environ["ONLYBOXES_DEV_ENV"])))
    payload = args.payload_mib << 20
    archive = int(os.environ.get("CONSOLE_SNAPSHOT_MAX_BYTES", "0"))
    expanded = int(os.environ.get("CONSOLE_SNAPSHOT_MAX_EXPANDED_BYTES", "0"))
    if archive <= payload or expanded <= payload:
        raise RuntimeError("parallel acceptance requires explicit archive/expanded limits above each payload")
    if os.environ.get("ONLYBOXES_DEV_BOXLITE_PROFILE") != "release":
        raise RuntimeError("parallel baseline requires the configured release profile")
    if os.environ.get("WORKER_PERSISTENCE_CACHE_MAX_BYTES") != "0":
        raise RuntimeError("this parallel baseline requires archive cache disabled on both Workers")
    if (os.environ.get("WORKER_TERMINAL_MAX_ACTIVE_SESSIONS") != "4"
            or os.environ.get("WORKER_PERSISTENCE_MAX_INFLIGHT") != "4"
            or os.environ.get("WORKER_TERMINAL_EXEC_MAX_INFLIGHT") != "2"):
        raise RuntimeError("parallel fixture requires four session/control slots and two execution slots per Worker")
    result_path = dev.STATE / "snapshot-parallel-result.json"
    if result_path.exists():
        raise RuntimeError("parallel result already exists; preserve the existing run")
    required = required_headroom(payload, expanded, 4)
    disk = os.statvfs(dev.STATE)
    if disk.f_bavail * disk.f_frsize < required:
        raise RuntimeError(f"parallel fixture host filesystem requires {required} free bytes before session creation")
    image = os.environ.get("WORKER_TERMINAL_EXEC_DOCKER_IMAGE", "")
    if "@sha256:" not in image:
        raise RuntimeError("parallel fixture requires a pinned headroom inspection image")
    free = dev.run(["docker", "run", "--rm", "--read-only", "--network", "none", "--entrypoint", "stat", image, "-f", "-c", "%a %S", "/"]).split()
    if len(free) != 2 or int(free[0]) * int(free[1]) < required:
        raise RuntimeError(f"parallel fixture daemon filesystem requires {required} free bytes before session creation")
    print("parallel-headroom: " + json.dumps({"required_free_bytes":required}), flush=True)
    opener, headers = dev.console_auth()
    fields = dev.credential_fields()
    if not fields.get("password"):
        raise RuntimeError("parallel acceptance requires the cached isolated Console login")
    dev.request_json(opener, "POST", "/api/v1/auth/login", {"username":fields["username"], "password":fields["password"]})
    execution = dev.request_json(opener, "POST", "/api/v1/tokens", {"name":"snapshot-parallel-" + uuid.uuid4().hex})
    lock = threading.RLock()
    origin = time.monotonic()
    evidence = {"runtime":"boxlite", "build_profile":"release", "concurrency":4, "payload_bytes_per_session":payload,
                "archive_limit_bytes":archive, "expanded_limit_bytes":expanded, "required_headroom_bytes":required,
                "archive_cache":"disabled", "worker_session_slots":4, "worker_control_slots":4, "worker_execution_slots":2,
                "root_disk_gib":int(os.environ.get("WORKER_TERMINAL_EXEC_BOXLITE_DISK_SIZE_GB", "0")), "operation_wait_seconds":args.operation_wait_sec, "sessions":[{} for _ in range(4)], "checks":[]}

    def save(label, **values):
        with lock:
            evidence["checks"].append({"check":label, "elapsed_seconds":round(time.monotonic()-origin, 6), **values})
            dev.atomic_private(result_path, json.dumps(evidence, indent=2) + "\n")
            print(label + ": " + json.dumps(values), flush=True)

    def api(method, path, value=None, key=None):
        authorization = {"Authorization":"Bearer " + execution["token"]} if path.startswith("/api/v1/commands/") else headers
        request = urllib.request.Request(dev.console_base() + path, data=None if value is None else json.dumps(value).encode(), method=method,
                                         headers={**authorization, "Content-Type":"application/json", **({"Idempotency-Key":key} if key else {})})
        try:
            # Every thread owns its request/connection; the mutable login cookie
            # jar is used only by the parent before concurrent work begins.
            with urllib.request.urlopen(request, timeout=630) as response:
                return response.status, None if response.status == 204 else json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.load(error)

    def require(response, expected=200):
        status, body = response
        if status != expected:
            code = body.get("code") if isinstance(body, dict) else None
            raise RuntimeError(f"parallel acceptance HTTP {status}, expected {expected}; code={code}")
        return body

    try:
        account = dev.request_json(opener, "GET", "/api/v1/auth/session")["account"]["account_id"]
        query = "?account_id=" + account
        nodes = {dev.env_file(dev.STATE/(service + ".env"))["WORKER_ID"]:service for service in dev.SERVICES}
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            workers = require(api("GET", "/api/v1/workers?status=all"))["items"]
            online = {w["node_id"] for w in workers if w["status"] == "online"}
            if online == set(nodes):
                break
            time.sleep(1)
        else:
            raise RuntimeError("parallel fixture requires exactly its two BoxLite Workers online")
        save("admitted", worker_count=len(nodes))
        source = Path(__file__).with_name("snapshot_payload.py").read_text()
        def session(entry):
            return require(api("GET", "/api/v1/sessions/" + entry["session_id"] + query))

        def command(entry, action):
            request = {"command":"python3 -c " + shlex.quote(source) + " " + action + " " + str(payload), "lease_ttl_sec":1800, "timeout_ms":600000}
            if entry:
                request.update(session_id=entry["session_id"], create_if_missing=False)
            return require(api("POST", "/api/v1/commands/terminal", request))

        def seed(index):
            result = command(None, "seed")
            with lock:
                evidence["sessions"][index].update(session_id=result["session_id"])
            save("seed-response", index=index, exit_code=result.get("exit_code"))
            digest = result.get("stdout", "").strip()
            if result.get("exit_code") != 0 or not result.get("created") or len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
                raise RuntimeError("parallel seed did not create its complete payload")
            entry = evidence["sessions"][index]
            current = session(entry)
            if current["worker_id"] not in nodes or current["lifecycle_state"] != "running":
                raise RuntimeError("parallel seed route is invalid")
            with lock:
                entry.update(payload_sha256=digest, source_worker=current["worker_id"])
            save("seeded", index=index)

        def batch(function):
            # Join every accepted client task even if a sibling fails. Failure
            # preserves all sessions and operations for explicit reconciliation.
            with ThreadPoolExecutor(max_workers=4) as executor:
                futures = [executor.submit(function, index) for index in range(4)]
                errors = []
                for future in futures:
                    try:
                        future.result()
                    except Exception as error:
                        errors.append(error)
                if errors:
                    raise errors[0]

        batch(seed)
        distribution = [sum(e["source_worker"] == node for e in evidence["sessions"]) for node in nodes]
        if distribution != [2, 2] or len({e["session_id"] for e in evidence["sessions"]}) != 4:
            raise RuntimeError("parallel sessions did not occupy two distinct slots on each Worker")
        save("seed-distribution", sessions_per_worker=distribution)

        def lifecycle(index, kind):
            entry = evidence["sessions"][index]
            base = "/api/v1/sessions/" + entry["session_id"]
            accepted = require(api("POST", base + "/" + kind + query, {"retention_ttl_sec":3600} if kind == "suspend" else {"lease_ttl_sec":1800}, str(uuid.uuid4())), 202)
            with lock:
                entry[kind] = {"operation_id":accepted["operation_id"], "started":time.monotonic()-origin}
            save(kind + "-accepted", index=index)
            deadline = time.monotonic() + args.operation_wait_sec
            phase = None
            while time.monotonic() < deadline:
                result = require(api("GET", base + "/operations/" + accepted["operation_id"] + query))
                if result.get("phase") != phase:
                    phase = result.get("phase")
                    save(kind + "-phase", index=index, phase=phase)
                if result["status"] in ("succeeded", "failed", "canceled"):
                    with lock:
                        entry[kind].update(finished=time.monotonic()-origin, status=result["status"], error_code=(result.get("error") or {}).get("code"))
                    save(kind + "-terminal", index=index, status=result["status"])
                    if result["status"] != "succeeded":
                        raise RuntimeError("parallel lifecycle did not succeed; preserve all evidence")
                    return
                time.sleep(.2)
            raise RuntimeError("parallel lifecycle observation timed out; preserve all evidence")

        def native(worker, operation):
            root = dev.STATE/nodes[worker]/hashlib.sha256(worker.encode()).hexdigest()[:32]
            journal = read_journal(root/"journal/journal.json")
            return root, journal["operations"][operation]

        batch(lambda index:lifecycle(index, "suspend"))
        suspend_intervals = [entry["suspend"] for entry in evidence["sessions"]]
        if peak_overlap(suspend_intervals) != 4:
            raise RuntimeError("four suspend operation intervals did not overlap")
        database = Path(os.environ["CONSOLE_DB_PATH"])
        if not database.is_absolute():
            database = dev.ROOT/"console"/database
        for index, entry in enumerate(evidence["sessions"]):
            current = session(entry)
            if current["lifecycle_state"] != "suspended" or current["worker_id"] is not None:
                raise RuntimeError("parallel source was not durably suspended")
            archive = current["persistence"]["latest_snapshot"]
            root, op = native(entry["source_worker"], entry["suspend"]["operation_id"])
            if op["phase"] != "finalized" or not op["snapshot_committed"] or not op["remote_verified"] or (root/"sdk-home/boxes"/op["box_id"]/"disks/disk.qcow2").exists():
                raise RuntimeError("parallel source native cleanup was not confirmed")
            with sqlite3.connect(database.resolve().as_uri()+"?mode=ro", uri=True) as db:
                parts, size = db.execute("SELECT COUNT(*),COALESCE(SUM(p.size_bytes),0) FROM sandbox_snapshot_parts p JOIN sandbox_snapshots s USING(snapshot_id) WHERE s.snapshot_id=? AND s.owner_id=? AND s.session_id=?", (archive["snapshot_id"],account,entry["session_id"])).fetchone()
            if parts < 1 or size != archive["size_bytes"]:
                raise RuntimeError("parallel multipart receipts do not cover their committed archive")
            with lock:
                entry.update(archive=op["archive"], multipart_parts=parts, source_instance=op["box_id"])
            save("source-finalized", index=index)

        # A terminal operation result can precede the capacity heartbeat.
        # Keep this separate from measured restore latency; do not relax the
        # scheduler's conservative overlap of reports and reservations.
        after = time.time()
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            workers = require(api("GET", "/api/v1/workers?status=all"))["items"]
            if fresh_heartbeats(workers, nodes, after):
                break
            time.sleep(.2)
        else:
            raise RuntimeError("Workers did not report fresh post-suspend heartbeats")
        save("post-suspend-heartbeats")
        batch(lambda index:lifecycle(index, "resume"))
        if peak_overlap([entry["resume"] for entry in evidence["sessions"]]) != 4:
            raise RuntimeError("four resume operation intervals did not overlap")
        def verify(index):
            entry = evidence["sessions"][index]
            current = session(entry)
            if current["lifecycle_state"] != "running" or current["worker_id"] != entry["source_worker"]:
                raise RuntimeError("parallel restore did not use its available preferred original Worker")
            result = command(entry, "verify")
            if result.get("exit_code") != 0 or result.get("created") or result.get("session_id") != entry["session_id"] or result.get("stdout", "").strip() != entry["payload_sha256"]:
                raise RuntimeError("parallel restored payload/mode/link/session check failed")
            _, op = native(current["worker_id"], entry["resume"]["operation_id"])
            if op["phase"] != "active" or not op["verified"] or not op["box_id"] or op["box_id"] == entry["source_instance"]:
                raise RuntimeError("parallel restore did not activate a distinct verified target")
            with lock:
                entry.update(target_instance=op["box_id"], restored_worker=current["worker_id"])
            save("restored-verified", index=index)
        batch(verify)
        for entry in evidence["sessions"]:
            require(api("DELETE", "/api/v1/sessions/" + entry["session_id"] + query), 204)
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            stats = require(api("GET", "/api/v1/persistence/stats" + query))
            if all(stats.get(k) == 0 for k in ("pending_sessions", "pending_workers", "pending_snapshots")):
                break
            time.sleep(1)
        else:
            raise RuntimeError("parallel permanent deletion did not drain")
        for node, service in nodes.items():
            root = dev.STATE/service/hashlib.sha256(node.encode()).hexdigest()[:32]/"sdk-home"
            if list(root.glob("boxes/*/disks/disk.qcow2")):
                raise RuntimeError("parallel fixture still contains guest qcow disks after deletion")
        save("cleanup-drained", pending_sessions=0, pending_workers=0, pending_snapshots=0,
             suspend_peak_overlapping_operations=peak_overlap(suspend_intervals), resume_peak_overlapping_operations=peak_overlap([e["resume"] for e in evidence["sessions"]]))
    finally:
        # Token deletion is authorized fixture cleanup; no guest is destroyed on
        # a failed run and accepted operations remain owned by their servers.
        request = urllib.request.Request(dev.console_base()+"/api/v1/tokens/"+execution["id"], method="DELETE")
        try:
            with opener.open(request, timeout=10):
                pass
        except OSError:
            pass


if __name__ == "__main__":
    main()
