import hashlib
import json
import os
import shutil
import stat
import struct
import sys
from pathlib import Path

P = Path("/persist")
paths = [
    "/persist",
    "/etc/onlyboxes-probe",
    "/root/onlyboxes-probe",
    "/usr/local/share/onlyboxes-probe",
]
if sys.argv[1] == "seed":
    P.mkdir()
    for n in ["modify", "delete", "rename", "file-to-dir"]:
        (P / n).write_text("base\n")
    for n in ["opaque", "dir-to-file"]:
        (P / n).mkdir()
        (P / n / "old").write_text("base\n")
    (P / "untouched").write_text("untouched\n")
elif sys.argv[1] == "mutate":
    (P / "modify").write_text("changed\n")
    (P / "delete").unlink()
    (P / "rename").rename(P / "renamed")
    shutil.rmtree(P / "opaque")
    (P / "opaque").mkdir()
    (P / "opaque" / "new").write_text("new\n")
    shutil.rmtree(P / "dir-to-file")
    (P / "dir-to-file").write_text("now-file\n")
    (P / "file-to-dir").unlink()
    (P / "file-to-dir").mkdir()
    (P / "file-to-dir" / "child").write_text("now-dir\n")
    (P / "added").write_bytes(b"hello\x00portable\n")
    os.link(P / "added", P / "hardlink")
    (P / "symlink").symlink_to("added")
    os.mkfifo(P / "fifo", 0o640)
    with (P / "sparse").open("wb") as f:
        f.seek(8 * 1024 * 1024)
        f.write(b"end")
    (P / "permissions").write_text("metadata\n")
    os.chown(P / "permissions", 1234, 2345)
    os.chmod(P / "permissions", 0o6751)
    os.setxattr(P / "permissions", "user.onlyboxes", b"xattr-value")
    acl = struct.pack("<I", 2) + b"".join(
        struct.pack("<HHI", tag, perm, uid)
        for tag, perm, uid in [
            (1, 7, 0xFFFFFFFF),
            (2, 4, 5678),
            (4, 5, 0xFFFFFFFF),
            (16, 5, 0xFFFFFFFF),
            (32, 1, 0xFFFFFFFF),
        ]
    )
    os.setxattr(P / "permissions", "system.posix_acl_access", acl)
    for n in paths[1:]:
        Path(n).write_text("outside-workdir\n")
    Path("/tmp/onlyboxes-probe").write_text("tmp-marker\n")
    for root, dirs, files in os.walk(P):
        for n in dirs + files:
            os.utime(Path(root) / n, (1700000000, 1700000000), follow_symlinks=False)
    os.utime(P, (1700000000, 1700000000))
    for n in paths[1:]:
        os.utime(n, (1700000000, 1700000000))
    os.sync()
elif sys.argv[1] == "verify":
    out = {}
    inodes = {}
    for top in paths:
        candidates = [top]
        if os.path.isdir(top):
            candidates += [
                str(Path(r) / n) for r, ds, fs in os.walk(top) for n in ds + fs
            ]
        for n in sorted(candidates):
            if not os.path.lexists(n):
                out[n] = {"missing": True}
                continue
            s = os.lstat(n)
            d = {
                "mode": oct(s.st_mode),
                "uid": s.st_uid,
                "gid": s.st_gid,
                "mtime_ns": s.st_mtime_ns,
                "xattrs": {
                    k: os.getxattr(n, k, follow_symlinks=False).hex()
                    for k in sorted(os.listxattr(n, follow_symlinks=False))
                },
            }
            if stat.S_ISREG(s.st_mode):
                d.update(
                    size=s.st_size,
                    sha256=hashlib.sha256(Path(n).read_bytes()).hexdigest(),
                )
                inodes.setdefault((s.st_dev, s.st_ino), []).append(n)
            if stat.S_ISLNK(s.st_mode):
                d["target"] = os.readlink(n)
            out[n] = d
    print(
        json.dumps(
            {
                "files": out,
                "hardlinks": sorted(sorted(v) for v in inodes.values() if len(v) > 1),
                "deleted_absent": not (P / "delete").exists(),
                "opaque_old_absent": not (P / "opaque" / "old").exists(),
                "tmp_present": Path("/tmp/onlyboxes-probe").exists(),
                "sparse_blocks": os.stat(P / "sparse").st_blocks,
            },
            sort_keys=True,
        )
    )
