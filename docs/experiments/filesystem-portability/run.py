"""Exercise Docker/BoxLite filesystem migration; Python 3.12+, pinned BoxLite version.

Usage: python run.py /absolute/path/to/new-artifact-directory
Creates isolated backend resources and writes manifests, archives and results.
"""

import argparse
import asyncio
import copy
import hashlib
import json
import platform
import shutil
import subprocess
import tarfile
import uuid
from importlib.metadata import version
from pathlib import Path

import boxlite

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument(
    "artifacts", type=Path, help="New directory for experiment artifacts"
)
parser.add_argument(
    "--boxlite-version", default="0.10.0", help="Expected installed BoxLite version"
)
args = parser.parse_args()
if version("boxlite") != args.boxlite_version:
    parser.error("Installed BoxLite does not match --boxlite-version")
D = args.artifacts.resolve()
D.mkdir(parents=True, exist_ok=False)
TAG = "onlyboxes-fs-probe-" + uuid.uuid4().hex[:10]
ARCH = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}[platform.machine()]
results = {}


def cmd(*args, output=None):
    print("+", args[0], args[1] if len(args) > 1 else "", flush=True)
    if output:
        with open(output, "wb") as f:
            subprocess.run(args, stdout=f, check=True)
        return ""
    return subprocess.check_output(args, text=True).strip()


def save_oci(image, name):
    archive = D / (name + ".tar")
    p = D / name
    p.mkdir(exist_ok=True)
    cmd("docker", "save", image, "-o", str(archive))
    with tarfile.open(archive) as t:
        t.extractall(p, filter="data")
    index = json.loads((p / "index.json").read_text())

    def resolve(desc):
        v = json.loads((p / "blobs/sha256" / desc["digest"].split(":")[1]).read_text())
        if "layers" in v:
            return desc
        for x in v["manifests"]:
            if x.get("platform", {}).get("architecture", ARCH) == ARCH:
                return resolve(x)
        raise ValueError(v)

    selected = resolve(index["manifests"][0])
    index["manifests"] = [selected]
    (p / "index.json").write_text(json.dumps(index))
    return p


async def execute(b, *args):
    e = await b.exec(args[0], list(args[1:]))
    out = []
    err = []

    async def collect(s, l):
        if s:
            async for chunk in s:
                l.append(chunk)

    a, c = (
        asyncio.create_task(collect(e.stdout(), out)),
        asyncio.create_task(collect(e.stderr(), err)),
    )
    r = await e.wait()
    await asyncio.gather(a, c)
    stdout = "".join(out)
    stderr = "".join(err)
    if r.exit_code:
        raise RuntimeError(f"{args[0]} failed ({r.exit_code}): {stderr}\n{stdout}")
    return stdout.strip()


def compare(label, source, target):
    differences = {
        k: {"source": source.get(k), "target": target.get(k)}
        for k in source.keys() | target.keys()
        if source.get(k) != target.get(k)
    }
    results[label] = {
        "equal": not differences,
        "differences": differences,
        "source": source,
        "target": target,
    }
    (D / "results.json").write_text(json.dumps(results, indent=2))
    print(
        label,
        json.dumps({"equal": not differences, "changed_fields": list(differences)}),
        flush=True,
    )


def normalized(name):
    while name.startswith("./"):
        name = name[2:]
    return name.rstrip("/")


def excluded(name):
    return (
        name in {"", "."}
        or name.split("/")[0] in {"proc", "sys", "dev", "tmp", "run", "mnt"}
        or name in {"etc/hostname", "etc/hosts", "etc/resolv.conf"}
    )


def entries(t):
    return {normalized(v.name): v for v in t if not excluded(normalized(v.name))}


def signature(t, v):
    pax = {k: x for k, x in v.pax_headers.items() if k.startswith("SCHILY.")}
    return (
        v.type,
        v.mode,
        v.uid,
        v.gid,
        v.mtime,
        v.linkname,
        v.size,
        pax,
        hashlib.sha256(t.extractfile(v).read()).hexdigest() if v.isfile() else None,
    )


def build_delta(artifact, strip_acl_text):
    with (
        tarfile.open(D / "box-base-rootfs.tar") as base,
        tarfile.open(D / "box-rootfs.tar") as state,
        tarfile.open(D / (artifact + ".tar"), "w", format=tarfile.PAX_FORMAT) as delta,
    ):
        a, b = entries(base), entries(state)
        removed = sorted(a.keys() - b.keys())
        # A parent whiteout already hides all of its descendants.
        removed = [
            n for n in removed if not any(n.startswith(p + "/") for p in removed)
        ]
        for name in removed:
            p = Path(name)
            v = tarfile.TarInfo(str(p.parent / (".wh." + p.name)))
            v.mode = 0
            delta.addfile(v)
        changes = [
            name
            for name, v in b.items()
            if name not in a or signature(base, a[name]) != signature(state, v)
        ]
        # OCI hardlinks require the target in the same changeset on containerd.
        for name in changes:
            v = b[name]
            if v.islnk() and normalized(v.linkname) not in changes:
                changes.append(normalized(v.linkname))
        ordered = []
        pending = set(changes)
        while pending:
            ready = [
                name
                for name in changes
                if name in pending
                and (not b[name].islnk() or normalized(b[name].linkname) not in pending)
            ]
            if not ready:
                raise RuntimeError("hardlink cycle")
            ordered.extend(ready)
            pending.difference_update(ready)
        for name in ordered:
            v = copy.copy(b[name])
            v.name = name
            v.linkname = normalized(v.linkname) if v.islnk() else v.linkname
            v.pax_headers = {
                k: x
                for k, x in v.pax_headers.items()
                if not k.startswith("GNU.sparse.")
                and k
                not in {
                    "atime",
                    "ctime",
                    "path",
                    "linkpath",
                    *(["SCHILY.acl.access"] if strip_acl_text else []),
                }
            }
            v.sparse = None
            delta.addfile(v, state.extractfile(b[name]) if v.isfile() else None)
        print(
            "delta",
            len(changes),
            "changed paths",
            len(removed),
            "whiteouts",
            flush=True,
        )
        (D / (artifact + "-entries.json")).write_text(
            json.dumps({"changed": changes, "whiteouts": removed}, indent=2)
        )
    p = D / (artifact + "-oci")
    shutil.copytree(D / "base-oci", p, dirs_exist_ok=True)

    def blob(data):
        digest = hashlib.sha256(data).hexdigest()
        (p / "blobs/sha256" / digest).write_bytes(data)
        return "sha256:" + digest, len(data)

    def jblob(obj):
        return blob(json.dumps(obj, separators=(",", ":")).encode())

    index = json.loads((p / "index.json").read_text())
    desc = index["manifests"][0]
    manifest = json.loads(
        (p / "blobs/sha256" / desc["digest"].split(":")[1]).read_text()
    )
    config = json.loads(
        (p / "blobs/sha256" / manifest["config"]["digest"].split(":")[1]).read_text()
    )
    digest, size = blob((D / (artifact + ".tar")).read_bytes())
    manifest["layers"].append(
        {
            "mediaType": "application/vnd.oci.image.layer.v1.tar",
            "digest": digest,
            "size": size,
        }
    )
    config["rootfs"]["diff_ids"].append(digest)
    config.setdefault("history", []).append(
        {"created_by": "onlyboxes filesystem portability probe"}
    )
    digest, size = jblob(config)
    manifest["config"]["digest"] = digest
    manifest["config"]["size"] = size
    digest, size = jblob(manifest)
    desc["digest"] = digest
    desc["size"] = size
    desc["annotations"] = {
        "org.opencontainers.image.ref.name": "docker.io/library/" + TAG + ":" + artifact
    }
    (p / "index.json").write_text(json.dumps(index))
    (p / "manifest.json").unlink(missing_ok=True)
    with tarfile.open(D / (artifact + "-oci.tar"), "w") as t:
        for name in ["oci-layout", "index.json", "blobs"]:
            t.add(p / name, arcname=name)
    return p, desc["digest"]


RESTORE = r"""
import os,stat,json
m=json.loads(DATA)['files']
for p,d in m.items():
 if not os.path.lexists(p) and stat.S_ISFIFO(int(d['mode'],8)):os.mkfifo(p,stat.S_IMODE(int(d['mode'],8)))
 s=os.lstat(p)
 if stat.S_ISLNK(s.st_mode) and s.st_mode!=int(d['mode'],8):
  os.unlink(p);os.symlink(d['target'],p)
 os.chown(p,d['uid'],d['gid'],follow_symlinks=False)
 if not stat.S_ISLNK(s.st_mode):os.chmod(p,stat.S_IMODE(int(d['mode'],8)))
 for k,v in d['xattrs'].items():os.setxattr(p,k,bytes.fromhex(v),follow_symlinks=False)
for group in json.loads(DATA)['hardlinks']:
 for p in group[1:]:
  if not os.path.samefile(group[0],p):os.unlink(p);os.link(group[0],p)
for p,d in sorted(m.items(),key=lambda x:x[0].count('/'),reverse=True):os.utime(p,ns=(d['mtime_ns'],d['mtime_ns']),follow_symlinks=False)
os.sync()
"""


async def export_rootfs(b, name):
    exclusions = [
        "proc",
        "sys",
        "dev",
        "tmp",
        "run",
        "mnt",
        "root/ob-export.tar",
        "etc/hosts",
        "etc/hostname",
        "etc/resolv.conf",
    ]
    command = (
        "tar --xattrs --acls --sparse --one-file-system "
        + " ".join("--exclude=./" + p for p in exclusions)
        + " -cpf /root/ob-export.tar -C / ."
    )
    await execute(b, "sh", "-c", command)
    await b.copy_out("/root/ob-export.tar", str(D / name))


async def probe_operation(label, operation):
    try:
        await operation()
    except Exception as error:
        results[label] = {"error": str(error)}
        (D / "results.json").write_text(json.dumps(results, indent=2))
        print(label, str(error), flush=True)
    else:
        results[label] = {"succeeded": True}
        (D / "results.json").write_text(json.dumps(results, indent=2))
        print(label, "succeeded", flush=True)


async def main():
    containers = []
    images = []

    def image(name):
        ref = TAG + ":" + name
        if ref not in images:
            images.append(ref)
        return ref

    def start(ref):
        cid = cmd("docker", "run", "-d", ref, "sleep", "infinity")
        containers.append(cid)
        return cid

    def check_docker(label, cid, source):
        compare(
            label,
            source,
            json.loads(cmd("docker", "exec", cid, "python", "/probe.py", "verify")),
        )

    rt = boxlite.Boxlite(boxlite.Options(home_dir=str(D / "box-home")))
    target = boxlite.Boxlite(boxlite.Options(home_dir=str(D / "box-import-home")))
    try:
        context = D / "context"
        context.mkdir()
        shutil.copyfile(Path(__file__).with_name("probe.py"), context / "probe.py")
        parent = "python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258"
        (context / "Dockerfile").write_text(
            "FROM "
            + parent
            + '\nCOPY probe.py /probe.py\nRUN python /probe.py seed\nCMD ["sleep", "infinity"]\n'
        )
        cmd("docker", "build", "-t", image("base"), str(context))
        base = save_oci(image("base"), "base-oci")
        (D / "environment.json").write_text(
            json.dumps(
                {
                    "docker": json.loads(
                        cmd("docker", "version", "--format", "{{json .}}")
                    ),
                    "platform": platform.platform(),
                    "architecture": ARCH,
                    "base_image_id": cmd(
                        "docker",
                        "image",
                        "inspect",
                        image("base"),
                        "--format",
                        "{{.Id}}",
                    ),
                    "base_manifest": json.loads((base / "index.json").read_text())[
                        "manifests"
                    ][0],
                    "boxlite": version("boxlite"),
                },
                indent=2,
            )
        )
        cid = start(image("base"))
        cmd("docker", "exec", cid, "python", "/probe.py", "mutate")
        dsource = json.loads(
            cmd("docker", "exec", cid, "python", "/probe.py", "verify")
        )
        cmd("docker", "commit", cid, image("docker-state"))
        committed = save_oci(image("docker-state"), "docker-state-oci")
        c2 = start(image("docker-state"))
        check_docker("docker-to-docker-commit", c2, dsource)
        b = await rt.create(
            boxlite.BoxOptions(
                rootfs_path=str(base), cpus=1, memory_mib=512, auto_delete=0
            ),
            name="source",
        )
        await b.start()
        (D / "box-mounts.txt").write_text(await execute(b, "cat", "/proc/mounts"))
        await export_rootfs(b, "box-base-rootfs.tar")
        await execute(b, "python", "/probe.py", "mutate")
        bsource = json.loads(await execute(b, "python", "/probe.py", "verify"))
        compare("source-runtime-parity", dsource, bsource)
        await export_rootfs(b, "box-rootfs.tar")
        await b.stop()
        archive = await b.export(dest=str(D / "native.boxlite"))
        restored = await target.import_box(archive, name="native-restored")
        await restored.start()
        compare(
            "boxlite-to-boxlite-native",
            bsource,
            json.loads(await execute(restored, "python", "/probe.py", "verify")),
        )
        await restored.stop()
        db = await rt.create(
            boxlite.BoxOptions(
                rootfs_path=str(committed), cpus=1, memory_mib=512, auto_delete=0
            ),
            name="docker-import",
        )
        await db.start()
        compare(
            "docker-to-boxlite-oci",
            dsource,
            json.loads(await execute(db, "python", "/probe.py", "verify")),
        )
        await db.stop()
        cmd("docker", "import", str(D / "box-rootfs.tar"), image("box-state"))
        check_docker("boxlite-to-docker-rootfs", start(image("box-state")), bsource)
        rich, _ = build_delta("rich", False)
        rb = await rt.create(
            boxlite.BoxOptions(
                rootfs_path=str(rich), cpus=1, memory_mib=512, auto_delete=0
            ),
            name="rich-oci",
        )
        await probe_operation("boxlite-rich-pax", rb.start)
        delta, digest = build_delta("delta", True)
        cmd("docker", "load", "-i", str(D / "delta-oci.tar"))
        cmd("docker", "tag", digest, image("delta"))
        check_docker("boxlite-to-docker-oci-delta", start(image("delta")), bsource)
        bb = await rt.create(
            boxlite.BoxOptions(
                rootfs_path=str(delta), cpus=1, memory_mib=512, auto_delete=0
            ),
            name="delta-import",
        )
        await bb.start()
        compare(
            "boxlite-to-boxlite-oci-delta",
            bsource,
            json.loads(await execute(bb, "python", "/probe.py", "verify")),
        )
        code = "DATA=" + repr(json.dumps(bsource)) + "\n" + RESTORE
        await execute(bb, "python", "-c", code)
        compare(
            "boxlite-oci-metadata-replay",
            bsource,
            json.loads(await execute(bb, "python", "/probe.py", "verify")),
        )
        await bb.stop()
        cmd("docker", "exec", c2, "python", "-c", code)
        check_docker("docker-commit-metadata-replay", c2, dsource)
        await probe_operation(
            "docker-to-boxlite-native",
            lambda: target.import_box(str(D / "docker-state-oci.tar")),
        )
        error = subprocess.run(
            ["docker", "load", "-i", archive], capture_output=True, text=True
        )
        if error.returncode == 0:
            raise AssertionError("Docker unexpectedly loaded native BoxLite archive")
        results["boxlite-to-docker-native"] = {"error": error.stderr.strip()}
        # All source boxes are stopped before hiding their image layout.
        base.rename(D / "base-hidden")
        try:
            standalone = await target.import_box(archive, name="missing-rootfs")
            await probe_operation("boxlite-native-missing-rootfs", standalone.start)
        finally:
            (D / "base-hidden").rename(base)
        (D / "results.json").write_text(json.dumps(results, indent=2))
    finally:
        await rt.shutdown()
        await target.shutdown()
        for cid in containers:
            cmd("docker", "rm", "-f", cid)
        for ref in reversed(images):
            subprocess.run(
                ["docker", "image", "rm", ref], check=False, stdout=subprocess.DEVNULL
            )
    print("Artifacts:", D, flush=True)


if __name__ == "__main__":
    asyncio.run(main())
