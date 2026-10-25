#!/usr/bin/env python3
"""Bounded-memory guest payload for snapshot acceptance."""
import argparse
import hashlib
import os
from pathlib import Path
import stat
import struct

CHUNK = 1024 * 1024
SMALL_BYTES = 1024
SMALL_RECORD = struct.Struct(">QH32s")


def small_payload(root, action, count, digest):
    directory = root / "small-files"
    manifest = root / "small-files.manifest"
    header = struct.pack(">QQ", count, SMALL_BYTES)
    digest.update(b"\0onlyboxes-small-files-v1\0" + header)
    if action == "seed":
        directory.mkdir(mode=0o700)
    else:
        info = directory.lstat()
        assert stat.S_ISDIR(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o700
        assert (info.st_uid, info.st_gid) == (os.getuid(), os.getgid())
        info = manifest.lstat()
        assert stat.S_ISREG(info.st_mode) and info.st_nlink == 1
        assert info.st_size == len(header) + count * SMALL_RECORD.size
        assert stat.S_IMODE(info.st_mode) == 0o600
    with manifest.open("xb" if action == "seed" else "rb") as index:
        if action == "seed":
            os.fchmod(index.fileno(), 0o600)
            index.write(header)
        else:
            assert index.read(len(header)) == header
        for number in range(count):
            shard = directory / f"{number // 256:04x}"
            path = shard / f"{number:08x}"
            mode = (0o600, 0o640, 0o644)[number % 3]
            if action == "seed":
                if number % 256 == 0:
                    shard.mkdir(mode=0o700)
                data = os.urandom(SMALL_BYTES)
                with path.open("xb") as output:
                    os.fchmod(output.fileno(), mode)
                    output.write(data)
                record = SMALL_RECORD.pack(number, mode, hashlib.sha256(data).digest())
                index.write(record)
                if number % 256 == 0:
                    os.link(path, shard / "hardlink")
                    (shard / "shortcut").symlink_to(path.name)
            else:
                record = index.read(SMALL_RECORD.size)
                saved_number, saved_mode, checksum = SMALL_RECORD.unpack(record)
                assert saved_number == number and saved_mode == mode
                info = path.lstat()
                assert stat.S_ISREG(info.st_mode) and info.st_size == SMALL_BYTES
                assert stat.S_IMODE(info.st_mode) == mode
                assert (info.st_uid, info.st_gid) == (os.getuid(), os.getgid())
                assert info.st_nlink == (2 if number % 256 == 0 else 1)
                with path.open("rb") as input_file:
                    data = input_file.read(SMALL_BYTES + 1)
                assert len(data) == SMALL_BYTES and hashlib.sha256(data).digest() == checksum
                if number % 256 == 0:
                    alias = (shard / "hardlink").lstat()
                    assert stat.S_ISREG(alias.st_mode) and (alias.st_dev, alias.st_ino) == (info.st_dev, info.st_ino)
                    assert (shard / "shortcut").readlink() == Path(path.name)
                    names = {f"{value:08x}" for value in range(number, min(number + 256, count))}
                    assert {value.name for value in shard.iterdir()} == names | {"hardlink", "shortcut"}
                    assert stat.S_IMODE(shard.lstat().st_mode) == 0o700
            digest.update(record)
        if action == "seed":
            index.flush()
            os.fsync(index.fileno())
        else:
            assert not index.read(1)
    if action == "seed":
        # Flush all generated file data and directory metadata in the guest,
        # rather than timing one fsync per tiny file.
        os.sync()
    else:
        assert {value.name for value in directory.iterdir()} == {f"{number:04x}" for number in range((count + 255) // 256)}


def payload(root, action, size, mutated=False, small_files=0):
    root = Path(root)
    assert action in ("seed", "verify") and size > 0 and 0 <= small_files <= 65536
    digest = hashlib.sha256()
    if action == "seed":
        if small_files:
            space = os.statvfs(root.parent)
            # Each shard owns a directory and a symlink; its hard link shares
            # the first data inode. Include the root, manifest and main files.
            needed_inodes = small_files + 2*((small_files + 255)//256) + 8
            assert space.f_favail >= needed_inodes, "insufficient guest inodes for small-file sample"
            needed_bytes = size + small_files * max(SMALL_BYTES, space.f_frsize) + small_files * SMALL_RECORD.size + (32 << 20)
            assert space.f_bavail * space.f_frsize >= needed_bytes, "insufficient guest space for small-file sample"
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
        if small_files:
            small_payload(root, action, small_files, digest)
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
        if small_files:
            small_payload(root, action, small_files, digest)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("seed", "verify"))
    parser.add_argument("size", type=int)
    parser.add_argument("--root", default="/root/snapshot-e2e")
    parser.add_argument("--mutated", action="store_true")
    parser.add_argument("--small-files", type=int, default=0, help="0..65536 additional independent random 1 KiB files")
    args = parser.parse_args()
    print(payload(args.root, args.action, args.size, args.mutated, args.small_files))


if __name__ == "__main__":
    main()
