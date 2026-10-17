"""Restore an image-backed Box into a cold home with the source home hidden.

Usage: python native-cold-home.py /new/artifacts/directory
Uses a registry image, matching worker-boxlite's RootfsSpec::Image behavior.
"""

import argparse
import asyncio
import base64
import json
from importlib.metadata import version
from pathlib import Path

import boxlite

IMAGE = "python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258"


async def execute(box, *args):
    execution = await box.exec(args[0], list(args[1:]))

    async def collect(stream):
        return "".join([part async for part in stream]) if stream else ""

    stdout = asyncio.create_task(collect(execution.stdout()))
    stderr = asyncio.create_task(collect(execution.stderr()))
    result = await execution.wait()
    out, err = await asyncio.gather(stdout, stderr)
    if result.exit_code:
        raise RuntimeError(f"Command failed ({result.exit_code}): {err}")
    return out.strip()


async def main(directory):
    directory.mkdir(parents=True, exist_ok=False)
    source_home = directory / "source-home"
    source_rt = boxlite.Boxlite(boxlite.Options(home_dir=str(source_home)))
    target_rt = boxlite.Boxlite(
        boxlite.Options(home_dir=str(directory / "target-home"))
    )
    hidden = directory / "source-home-hidden"
    try:
        source = await source_rt.create(
            boxlite.BoxOptions(image=IMAGE, cpus=1, memory_mib=512, auto_delete=0),
            name="source",
        )
        await source.start()
        code = base64.b64encode(
            Path(__file__).with_name("probe.py").read_bytes()
        ).decode()
        await execute(
            source,
            "python",
            "-c",
            f"import base64;open('/probe.py','wb').write(base64.b64decode({code!r}))",
        )
        await execute(source, "python", "/probe.py", "seed")
        await execute(source, "python", "/probe.py", "mutate")
        before = json.loads(await execute(source, "python", "/probe.py", "verify"))
        await source.stop()
        archive = await source.export(dest=str(directory / "native.boxlite"))
        await source_rt.shutdown()
        source_home.rename(hidden)
        restored = await target_rt.import_box(archive, name="restored")
        await restored.start()
        after = json.loads(await execute(restored, "python", "/probe.py", "verify"))
        differences = {
            key: {"source": before[key], "target": after[key]}
            for key in before
            if before[key] != after[key]
        }
        result = {
            "boxlite": version("boxlite"),
            "image": IMAGE,
            "source_home_hidden": True,
            "target_started_with_empty_home": True,
            "differences": differences,
            "source": before,
            "target": after,
        }
        (directory / "results.json").write_text(json.dumps(result, indent=2))
        print(
            json.dumps({"boxlite": version("boxlite"), "differences": differences}),
            flush=True,
        )
        await restored.stop()
    finally:
        await source_rt.shutdown()
        await target_rt.shutdown()
        if hidden.exists():
            hidden.rename(source_home)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("artifacts", type=Path)
    args = parser.parse_args()
    asyncio.run(main(args.artifacts.resolve()))
