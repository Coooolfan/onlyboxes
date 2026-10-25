#!/usr/bin/env python3
"""Measure real BoxLite snapshot stages and sampled local disk accounting."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import threading
import tempfile
import time

SPEC = importlib.util.spec_from_file_location("dev_sandbox", Path(__file__).resolve().parents[1] / "dev-sandbox.py")
dev = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(dev)
START = "sandbox snapshot stage started"
FINISH = "sandbox snapshot stage finished"
STAGES = {"export", "upload_source_verify", "upload_parts", "remote_verify", "finalize", "cache_copy", "download", "import", "start_and_readiness"}


def write_metrics(path, evidence):
    if path.exists() or path.is_symlink():
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 64 << 20:
            raise RuntimeError("refusing unsafe metric evidence replacement")
    data = (json.dumps(evidence, indent=2) + "\n").encode()
    if len(data) > 64 << 20:
        raise RuntimeError("metric evidence exceeds its bounded size")
    fd, temporary = tempfile.mkstemp(prefix=".metrics-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data); stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def stage_events(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 32 << 20:
        raise RuntimeError("stage logs must be bounded private owned files")
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "r") as stream:
        for line in stream:
            try:
                item = json.loads(line)
            except ValueError:
                continue
            fields = item.get("fields", {}) if isinstance(item, dict) else {}
            if not isinstance(fields, dict) or fields.get("message") not in (START, FINISH):
                continue
            if (not all(isinstance(fields.get(key), str) and fields[key] for key in ("operation_id", "attempt_id", "stage"))
                    or fields["stage"] not in STAGES):
                raise RuntimeError("invalid snapshot stage identity")
            event = {key:fields[key] for key in ("message", "operation_id", "attempt_id", "stage")}
            if fields["message"] == FINISH:
                if type(fields.get("elapsed_us")) is not int or fields["elapsed_us"] < 0 or type(fields.get("succeeded")) is not bool:
                    raise RuntimeError("invalid snapshot stage completion")
                event.update(elapsed_us=fields["elapsed_us"], succeeded=fields["succeeded"])
            yield event


def summarize(events, worker, operation, required):
    attempts = {}
    for event in events:
        if event["worker"] != worker or event["operation_id"] != operation:
            continue
        pair = attempts.setdefault(event["attempt_id"], {})
        if event["message"] in pair and pair[event["message"]] != event:
            raise RuntimeError("conflicting stage receipt")
        pair[event["message"]] = event
    stages = {}
    for pair in attempts.values():
        if set(pair) != {START, FINISH} or pair[START]["stage"] != pair[FINISH]["stage"]:
            raise RuntimeError("stage attempt lacks its matching start/finish")
        event = pair[FINISH]
        stage = stages.setdefault(event["stage"], {"attempts":0, "succeeded":0, "failed":0, "elapsed_us_sum":0, "attempt_elapsed_us":[]})
        stage["attempts"] += 1
        stage["succeeded" if event["succeeded"] else "failed"] += 1
        stage["elapsed_us_sum"] += event["elapsed_us"]
        stage["attempt_elapsed_us"].append(event["elapsed_us"])
    if any(not stages.get(name, {}).get("succeeded") for name in required):
        raise RuntimeError("successful lifecycle lacks required successful stage receipts")
    return stages


def native_operation(worker, service, operation):
    root = dev.STATE / service / hashlib.sha256(worker.encode()).hexdigest()[:32]
    path = root / "journal/journal.json"
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size > 64 << 20:
        raise RuntimeError("invalid private native journal evidence")
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as stream:
        data = stream.read((64 << 20) + 1)
    if len(data) > 64 << 20:
        raise RuntimeError("native journal evidence exceeds its bound")
    state = json.loads(data)
    if state["worker_id"] != worker:
        raise RuntimeError("native journal belongs to another Worker")
    return root, state["operations"][operation]


def audit_native_lifecycle(result, nodes, checks, source, target):
    source_root, source_op = native_operation(source, nodes[source], checks["suspend"]["operation_id"])
    target_root, target_op = native_operation(target, nodes[target], checks["resume"]["operation_id"])
    scope = "obx:" + result["account_id"] + ":" + result["session_id"]
    if (source_op["kind"] != "source" or source_op["phase"] != "finalized" or not source_op["snapshot_committed"] or not source_op["remote_verified"]
            or target_op["kind"] != "target" or target_op["phase"] != "active" or source_op["archive"] != target_op["archive"]
            or source_op["scoped_session_id"] != scope or target_op["scoped_session_id"] != scope
            or not source_op["box_id"] or not target_op["box_id"] or source_op["box_id"] == target_op["box_id"]):
        raise RuntimeError("metric lifecycle lacks exact committed source and independent target evidence")
    for root, op in ((source_root, source_op), (target_root, target_op)):
        box = root / "sdk-home/boxes" / op["box_id"]
        if Path(op["box_id"]).name != op["box_id"] or op["box_id"] in (".", "..") or box.exists() or box.is_symlink():
            raise RuntimeError("metric lifecycle original/restored root or guest disk remains after cleanup")
    return {"independent_native_instances_and_disks_removed":True}


def lifecycle_checks(result):
    for kind in ("suspend", "resume"):
        attempts = [check for check in result["checks"] if check["check"] == kind]
        if len(attempts) != 1 or attempts[0].get("status") != "succeeded":
            raise RuntimeError("baseline measurement requires one successful client lifecycle attempt per phase")
    return {value["check"]:value for value in result["checks"]}


def disk_accounting(roots):
    """Do not follow links; deduplicate hard-link aliases across all roots."""
    seen = set()
    values = {}
    raced = False
    for label, root in roots.items():
        logical = allocated = count = 0
        todo = [root]
        while todo:
            path = todo.pop()
            try:
                info = path.lstat()
                key = (info.st_dev, info.st_ino)
                if key in seen:
                    continue
                seen.add(key)
                count += 1
                logical += info.st_size
                allocated += info.st_blocks * 512
                if stat.S_ISDIR(info.st_mode):
                    with os.scandir(path) as entries:
                        todo.extend(Path(entry.path) for entry in entries)
            except FileNotFoundError:
                raced = True
        values[label] = {"logical_bytes":logical, "allocated_bytes":allocated, "inodes":count}
    return {"roots":values, "raced_with_filesystem_change":raced,
            "total_logical_bytes":sum(v["logical_bytes"] for v in values.values()),
            "total_allocated_bytes":sum(v["allocated_bytes"] for v in values.values())}


class Monitor:
    def __init__(self, nodes, roots):
        self.nodes, self.roots = nodes, roots
        self.events, self.samples = {}, []
        self.failure = False
        self.done = threading.Event()
        self.origin = time.monotonic()
        self.thread = threading.Thread(target=self.run, daemon=True)

    def capture(self, sample):
        for worker, service in self.nodes.items():
            for event in stage_events(dev.STATE / (service + ".log")):
                event["worker"] = worker
                key = (worker, event["attempt_id"], event["message"])
                if key in self.events and self.events[key] != event:
                    raise RuntimeError("stage receipt changed")
                self.events[key] = event
        if sample:
            value = disk_accounting(self.roots)
            value["elapsed_seconds"] = time.monotonic() - self.origin
            self.samples.append(value)

    def run(self):
        last_sample = time.monotonic()
        try:
            while not self.done.wait(.1):
                sample = time.monotonic() - last_sample >= .5
                self.capture(sample)
                if sample:
                    last_sample = time.monotonic()
        except Exception:
            # Preserve no exception text: IO/decoder errors can carry paths/data.
            self.failure = True

    def start(self):
        self.capture(True)  # Baseline precedes even the child's seed/auth work.
        if self.samples[0]["raced_with_filesystem_change"]:
            raise RuntimeError("cannot capture a complete pre-seed disk baseline")
        self.thread.start()

    def stop(self):
        self.done.set()
        self.thread.join(10)
        if self.thread.is_alive():
            raise RuntimeError("read-only metric monitor remains pending")
        self.capture(True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=("cross-worker", "same-worker"), default="cross-worker")
    parser.add_argument("--payload-mib", type=int, default=1024)
    parser.add_argument("--operation-wait-sec", type=int, default=1800)
    args = parser.parse_args()
    if not 1 <= args.payload_mib <= 10240 or not 1 <= args.operation_wait_sec <= 86400:
        parser.error("payload requires 1..10240 MiB; operation wait requires 1..86400 seconds")
    os.environ.update(dev.env_file(Path(os.environ["ONLYBOXES_DEV_ENV"])))
    if (os.environ.get("ONLYBOXES_DEV_BOXLITE_PROFILE") != "release"
            or os.environ.get("WORKER_LOG_FORMAT", "json") != "json"
            or os.environ.get("WORKER_LOG_LEVEL", "info") not in ("info", "debug", "trace")):
        raise RuntimeError("stage measurement requires release Workers and JSON info logs")
    prefix = "snapshot-stage-metrics-" + args.scenario
    destination = dev.STATE / (prefix + ".json")
    result_path = dev.STATE / ("snapshot-e2e-" + args.scenario + "-result.json")
    log = dev.STATE / (prefix + ".log")
    if any(path.exists() for path in (destination, result_path, log)):
        raise RuntimeError("stage or lifecycle evidence already exists; use another isolated fixture")
    nodes, roots = {}, {}
    for service in dev.SERVICES:
        credentials = dev.env_file(dev.STATE / (service + ".env"))
        worker = credentials["WORKER_ID"]
        if not worker or worker in nodes:
            raise RuntimeError("stage measurement requires distinct provisioned Workers")
        nodes[worker] = service
        root = dev.STATE / service / hashlib.sha256(worker.encode()).hexdigest()[:32]
        roots[service + "/sdk-home"] = root / "sdk-home"
        roots[service + "/journal"] = root / "journal"
    # O_EXCL serializes this fixture before any child can create a session.
    fd = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    monitor = Monitor(nodes, roots)
    child = None
    try:
        with os.fdopen(fd, "w") as stream:
            monitor.start()
            child = subprocess.run([sys.executable, str(Path(__file__).with_name("snapshot_e2e.py")), "--scenario", args.scenario,
                                    "--payload-mib", str(args.payload_mib), "--operation-wait-sec", str(args.operation_wait_sec)],
                                   stdout=stream, stderr=subprocess.STDOUT)
    finally:
        if monitor.thread.ident is not None:
            try:
                monitor.stop()
            except Exception:
                monitor.failure = True
        evidence = {"runtime":"boxlite", "build_profile":"release", "scenario":args.scenario,
                    "child_exit_code":None if child is None else child.returncode, "monitor_failed":monitor.failure,
                    "monitor_stopped":not monitor.thread.is_alive(),
                    "sampling_interval_target_seconds":.5, "samples":monitor.samples, "stage_events":list(monitor.events.values()),
                    "coverage":"before child authentication/seed through child exit after verification/deletion/cleanup; includes existing image caches and journal history",
                    "disk_limit":"metadata sampling; no symlink traversal; hard-link inode aliases deduplicated. COW/reflink shared physical blocks may be counted multiple times; brief peaks and file creation/deletion races can be missed. Excludes S3, build products and other host paths; not a hard physical-peak or quota measurement",
                    "timing_limit":"per awaited Worker call, including its permit waits, internal verification and retries. Replays are separate attempts. No completed receipt if its waiter is dropped; stage sums exclude Console/DB/scheduling/activation and are not end-to-end latency"}
        write_metrics(destination, evidence)
    if child.returncode or monitor.failure:
        raise RuntimeError("lifecycle or metric capture failed; preserve private evidence")
    result = json.loads(dev.read_private(result_path))
    checks = lifecycle_checks(result)
    source = checks["created"]["worker_id"]
    target = checks[args.scenario]["worker_id"]
    evidence["source_stages"] = summarize(evidence["stage_events"], source, checks["suspend"]["operation_id"], {"export", "upload_source_verify", "upload_parts", "remote_verify", "finalize"})
    evidence["target_stages"] = summarize(evidence["stage_events"], target, checks["resume"]["operation_id"], {"import", "start_and_readiness"})
    target_stages = evidence["target_stages"]
    if not any(target_stages.get(stage, {}).get("succeeded") for stage in ("download", "cache_copy")):
        raise RuntimeError("restore lacks a successful measured archive transfer")
    if args.scenario == "cross-worker" and not target_stages.get("download", {}).get("succeeded"):
        raise RuntimeError("cross-worker measurement did not observe archive download")
    evidence.update(audit_native_lifecycle(result, nodes, checks, source, target))
    valid_samples = [sample for sample in evidence["samples"] if not sample["raced_with_filesystem_change"]]
    if len(valid_samples) < 2:
        raise RuntimeError("insufficient complete disk samples")
    evidence.update(verified=True, source_worker=source, target_worker=target,
                    payload_bytes=result["payload_bytes"], archive_bytes=checks["committed-archive"]["size_bytes"],
                    baseline_allocated_bytes=evidence["samples"][0]["total_allocated_bytes"],
                    observed_peak_allocated_bytes=max(value["total_allocated_bytes"] for value in valid_samples))
    write_metrics(destination, evidence)
    print(json.dumps({"verified":True, "samples":len(evidence["samples"]), "source_stages":evidence["source_stages"],
                      "target_stages":target_stages, "observed_peak_allocated_bytes":evidence["observed_peak_allocated_bytes"]}))


if __name__ == "__main__":
    main()
