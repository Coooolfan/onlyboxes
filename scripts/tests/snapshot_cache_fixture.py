#!/usr/bin/env python3
"""Internal, operation-scoped cache fault fixture for snapshot acceptance."""
import argparse
from contextlib import ExitStack
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import time


def probe(root, operation, worker, action, snapshot, expected, runtime="docker"):
    with ExitStack() as stack:
        return probe_locked(root, operation, worker, action, snapshot, expected, runtime, stack)


def probe_locked(root, operation, worker, action, snapshot, expected, runtime, stack):
    assert runtime in ("docker", "boxlite")
    assert action in ("corrupt", "evict", "source-corrupt", "source-missing", "target-valid")
    root = Path(root)
    info = root.lstat()
    assert stat.S_ISDIR(info.st_mode) and info.st_mode & 0o077 == 0
    journal = root / "journal.json"
    info = journal.lstat()
    assert stat.S_ISREG(info.st_mode) and info.st_mode & 0o077 == 0 and info.st_uid == os.geteuid() and info.st_size <= 64*1024*1024
    with os.fdopen(os.open(journal, os.O_RDONLY | os.O_NOFOLLOW), "rb") as stream:
        saved = stream.read(64*1024*1024+1)
    state = json.loads(saved)
    assert state["worker_id"] == worker
    op = state["operations"][operation]
    assert op["operation_id"] == operation and op["verified"]
    if action == "target-valid":
        assert op["kind"] == "target" and op["phase"] == "active"
    else:
        assert op["kind"] == "source" and op["phase"] == "finalized" and op["snapshot_committed"] and op["remote_verified"]
    assert op["cache_until_unix_ms"] > time.time()*1000
    archive = op["archive"]
    assert archive["snapshot_id"] == snapshot and archive["sha256"] == expected and archive["size_bytes"] > 0
    name = op["artifact"]
    assert name and Path(name).name == name and name not in (".", "..")
    artifacts = root / "artifacts"
    info = artifacts.lstat()
    assert stat.S_ISDIR(info.st_mode) and info.st_mode & 0o077 == 0
    path = artifacts / name
    if runtime == "boxlite":
        identity = json.dumps([op["scoped_session_id"], operation, op["generation"]], ensure_ascii=False, separators=(",", ":")).encode()
        assert name == hashlib.sha256(identity).hexdigest() + ".boxlite"
        lock = stack.enter_context(os.fdopen(os.open(str(path)+".lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600), "r+b"))
        info = os.fstat(lock.fileno())
        assert stat.S_ISREG(info.st_mode) and info.st_mode & 0o077 == 0 and info.st_uid == os.geteuid() and info.st_nlink == 1 and info.st_size == 0
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    if action == "source-missing":
        assert not path.exists() and not path.is_symlink()
        return {"present": False, "snapshot_id": snapshot}
    # A write is allowed only on this finalized, committed optional cache.
    flags = os.O_RDWR if action == "corrupt" else os.O_RDONLY
    with os.fdopen(os.open(path, flags | os.O_NOFOLLOW), "r+b" if action == "corrupt" else "rb") as stream:
        info = os.fstat(stream.fileno())
        assert stat.S_ISREG(info.st_mode) and info.st_mode & 0o077 == 0 and info.st_uid == os.geteuid()
        assert info.st_size == archive["size_bytes"]
        def digest():
            stream.seek(0)
            h = hashlib.sha256()
            for block in iter(lambda: stream.read(1048576), b""):
                h.update(block)
            return h.hexdigest()
        actual = digest()
        if action == "source-corrupt":
            assert actual != expected
        else:
            assert actual == expected
        if action in ("corrupt", "evict"):
            assert info.st_nlink == 1
            current = path.lstat()
            assert (current.st_dev, current.st_ino) == (info.st_dev, info.st_ino)
            if action == "corrupt":
                stream.seek(0)
                first = stream.read(1)
                stream.seek(0)
                assert stream.write(bytes([first[0] ^ 1])) == 1
                stream.flush()
                os.fsync(stream.fileno())
                actual = digest()
                assert actual != expected and os.fstat(stream.fileno()).st_size == info.st_size
            else:
                path.unlink()
                directory = os.open(artifacts, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
                try:
                    os.fsync(directory)
                finally:
                    os.close(directory)
    # Never change ownership records, specs, manifests or guard receipts.
    if action in ("corrupt", "evict"):
        assert journal.read_bytes() == saved
    return {"present": action != "evict", "snapshot_id": snapshot, "sha256": actual, "valid": actual == expected}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root")
    parser.add_argument("operation")
    parser.add_argument("worker")
    parser.add_argument("action", choices=("corrupt", "evict", "source-corrupt", "source-missing", "target-valid"))
    parser.add_argument("snapshot")
    parser.add_argument("sha256")
    parser.add_argument("--runtime", choices=("docker", "boxlite"), default="docker")
    args = parser.parse_args()
    print(json.dumps(probe(args.root, args.operation, args.worker, args.action, args.snapshot, args.sha256, args.runtime)))


if __name__ == "__main__":
    main()
