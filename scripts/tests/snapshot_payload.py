#!/usr/bin/env python3
"""Bounded-memory guest payload for snapshot acceptance."""
import argparse
import hashlib
import os
from pathlib import Path
import stat

CHUNK = 1024 * 1024


def payload(root, action, size, mutated=False):
    root = Path(root)
    assert action in ("seed", "verify") and size > 0
    digest = hashlib.sha256()
    if action == "seed":
        root.mkdir(mode=0o700)
        with (root / "payload").open("xb") as stream:
            remaining = size
            while remaining:
                block = os.urandom(min(CHUNK, remaining))
                stream.write(block)
                digest.update(block)
                remaining -= len(block)
            os.fchmod(stream.fileno(), 0o640)
            stream.flush()
            os.fsync(stream.fileno())
        (root / "link").symlink_to("payload")
        for name, text in (("remove-me", "delete after restore"), ("modified-file", "first version")):
            with (root / name).open("x") as stream:
                stream.write(text)
                stream.flush()
                os.fsync(stream.fileno())
        directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    else:
        assert (root / "link").readlink() == Path("payload")
        info = (root / "payload").stat()
        assert info.st_size == size and stat.S_IMODE(info.st_mode) == 0o640
        if mutated:
            assert not (root / "remove-me").exists()
            assert (root / "new-file").read_bytes() == b"created after restore"
            assert (root / "modified-file").read_text() == "second version"
        else:
            assert (root / "remove-me").exists()
            assert (root / "modified-file").read_text() == "first version"
        with (root / "link").open("rb") as stream:
            for block in iter(lambda: stream.read(CHUNK), b""):
                digest.update(block)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("seed", "verify"))
    parser.add_argument("size", type=int)
    parser.add_argument("--root", default="/root/snapshot-e2e")
    parser.add_argument("--mutated", action="store_true")
    args = parser.parse_args()
    print(payload(args.root, args.action, args.size, args.mutated))


if __name__ == "__main__":
    main()
