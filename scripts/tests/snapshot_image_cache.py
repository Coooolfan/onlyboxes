"""Read-only BoxLite 0.10.7 image-cache evidence; never opens the SDK runtime."""
import json
import os
from pathlib import Path
import re
import sqlite3
import stat

DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def canonical_reference(image):
    name, separator, digest = image.rpartition("@")
    if not separator or not DIGEST.fullmatch(digest) or not re.fullmatch(r"[a-z0-9._:/-]+", name):
        raise RuntimeError("image-cache acceptance requires a pinned OCI image")
    first = name.split("/", 1)[0]
    if "." not in first and ":" not in first and first != "localhost":
        name = "docker.io/" + ("library/" if "/" not in name else "") + name
    return name + "@" + digest


def cache_snapshot(home, image):
    home = Path(home)
    info = home.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or home.resolve() != home:
        raise RuntimeError("image-cache evidence requires the canonical private SDK home")
    reference = canonical_reference(image)
    database = home/"db/boxlite.db"
    info = database.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022 or info.st_nlink != 1 or database.resolve() != database:
        raise RuntimeError("unsafe SDK image index")
    with sqlite3.connect(database.as_uri()+"?mode=ro", uri=True, timeout=3) as db:
        db.execute("BEGIN")
        if db.execute("SELECT version FROM schema_version WHERE id=1").fetchone() != (10,):
            raise RuntimeError("unsupported SDK image index schema")
        count = db.execute("SELECT COUNT(*) FROM image_index").fetchone()[0]
        if count > 16384:
            raise RuntimeError("SDK image index exceeds evidence bound")
        row = db.execute("SELECT manifest_digest,config_digest,CASE WHEN length(layers)<=65536 THEN layers END,complete FROM image_index WHERE reference=?", (reference,)).fetchone()
        aliases = [] if row is None else [value[0] for value in db.execute("SELECT reference FROM image_index WHERE manifest_digest=? AND complete=1", (row[0],))]
    images = home/"images"
    if not images.is_dir() or images.resolve() != images:
        raise RuntimeError("unsafe or missing SDK image cache directory")
    def walk_error(error):
        raise RuntimeError("SDK image cache inventory unavailable") from None
    files = 0
    for root, directories, names in os.walk(images, followlinks=False, onerror=walk_error):
        files += len(names) + sum((Path(root)/name).is_symlink() for name in directories)
        if files > 200000:
            raise RuntimeError("SDK image cache exceeds evidence inventory bound")
    state = "cold" if count == 0 and files == 0 else "incomplete"
    complete = False
    layer_count = 0
    manifest = None
    if row is not None:
        manifest, config, layers, indexed_complete = row
        if not DIGEST.fullmatch(manifest) or not DIGEST.fullmatch(config) or layers is None:
            raise RuntimeError("invalid SDK cached-image identity")
        layers = json.loads(layers)
        if not isinstance(layers, list) or len(layers) > 1024 or any(not isinstance(value, str) or not DIGEST.fullmatch(value) for value in layers):
            raise RuntimeError("invalid SDK cached-image layers")
        paths = [home/"images/manifests"/(manifest.replace(":", "-")+".json"),
                 home/"images/configs"/(config.replace(":", "-")+".json")]
        paths += [home/"images/layers"/(digest.replace(":", "-")+".tar.gz") for digest in layers]
        complete = bool(indexed_complete)
        for path in paths:
            try:
                info = path.lstat()
            except FileNotFoundError:
                complete = False
                continue
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022 or path.resolve() != path or info.st_size <= 0:
                raise RuntimeError("unsafe or empty SDK image-cache blob")
        layer_count = len(layers)
        if complete:
            state = "warm"
    return {"state":state, "reference":reference, "manifest_digest":manifest, "indexed_images":count,
            "cache_file_entries":files, "requested_index_present":row is not None,
            "requested_complete_blobs_present":complete, "requested_layer_count":layer_count,
            "equivalent_complete_references":aliases,
            "definition":"SDK image_index plus manifest/config/layer presence per pinned 0.10.7 cache lookup; no blob rehash or OS/registry cache claim"}


def sdk_cache_events(path, offset, references):
    counts = {"registry_pull":0, "cache_reuse":0}
    prefixes = {"Pulling image from registry: ":"registry_pull", "Using cached image: ":"cache_reuse"}
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1 or info.st_size < offset:
        raise RuntimeError("unsafe or truncated SDK image log")
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as stream:
        stream.seek(offset)
        data = stream.read((32<<20)+1)
    if len(data) > 32<<20:
        raise RuntimeError("SDK image log evidence exceeds bound")
    for line in data.splitlines():
        try:
            value = json.loads(line)
        except ValueError:
            continue
        fields = value.get("fields", {}) if isinstance(value, dict) else {}
        message = fields.get("message", "") if isinstance(fields, dict) else ""
        if not isinstance(message, str):
            continue
        for prefix, key in prefixes.items():
            if message.startswith(prefix) and message[len(prefix):] in references:
                counts[key] += 1
    return counts
