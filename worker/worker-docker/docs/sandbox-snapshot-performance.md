# Docker snapshot performance fixture

`TestIntegrationDockerSnapshotPerformance` measures the native filesystem adapter on a real Docker daemon. It seeds random, nonsparse data, freezes and exports the source, verifies the archive, removes the source, imports a fresh image, reconstructs and starts the same session name, and reads every restored file to compare its size, mode and full content digest. Source, target, export helper, archive and imported image belong only to the fixture; successful completion includes their cleanup.

Run from `worker/worker-docker` with an existing, immutable helper image that supplies GNU tar, Python 3, `sh` and GNU `stat`:

```bash
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_PERFORMANCE=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<pinned-digest>' \
DOCKER_SNAPSHOT_PERF_MIB=1024 \
DOCKER_SNAPSHOT_PERF_WORKERS=1 \
DOCKER_SNAPSHOT_PERF_FILES=1 \
go test ./internal/runner -run '^TestIntegrationDockerSnapshotPerformance$' \
  -count=1 -timeout=50m -v
```

Both enable flags are required, so ordinary integration and race runs skip these large allocations. The fixture does not start platform services; use `scripts/dev.sh` for Console/Worker/S3 acceptance. Avoid the race detector for comparable performance measurements; correctness and race tests remain separate checks.

| Input | Default | Accepted values |
| --- | --- | --- |
| `DOCKER_SNAPSHOT_PERF_MIB` | 1024 | 1–10240 MiB of new data per fixture |
| `DOCKER_SNAPSHOT_PERF_WORKERS` | 1 | 1 or 4 concurrent native fixtures on one daemon |
| `DOCKER_SNAPSHOT_PERF_FILES` | 1 | 1–65536 files sharing the requested data size |

Use 1024 / 5120 / 10240 MiB to exercise increasing sizes, and 65536 files for the small-file case. The data generator writes independent random bytes and syncs each file. Files live outside the excluded temporary directories. Actual archive bytes include the full base filesystem and tar metadata, so a 1024 MiB payload produces an archive larger than 1 GiB. This parameter selects payload size, not an exact archive size.

Each source has one CPU, 512 MiB memory and a 128-process limit. Four fixtures run in independent goroutines with their own scope, resource names, files and import tags; they share the helper image and daemon. This measures same-daemon contention and does not represent four physical Workers.

## Capacity and cache conditions

Before creating a source, the fixture checks available space on both the CLI host's temporary filesystem and the daemon filesystem. Its conservative headroom requirement is:

```text
concurrent fixtures × 6 × (payload bytes + base image bytes + file count × 8192)
  + 2 GiB
```

This guard allows room for source writes, the exported archive, import workspace and restored data. It is a preflight check, not an atomic reservation or a measured disk peak; other activity can still consume capacity. `TMPDIR` selects the Go temporary filesystem. A helper probe reads daemon free blocks, which may be on another host or disk. A failed preflight does not prune shared images, containers, caches or volumes. Cleanup errors fail the test.

The source/helper image is already local, and runtime probes use it before timing begins. Each imported image is newly created after source removal. Host page cache, daemon caches and shared base-image layers are uncontrolled; results must not be labeled as a fully cold-cache measurement. The fixture never evicts unrelated data to force cold conditions.

## Result interpretation

The successful verbose log contains one `snapshot_performance=<JSON>` line with runtime version, guest platform, helper digest, concurrency, headroom requirement, total fixture wall time and per-fixture records. Per-fixture records include payload size/file count, actual archive bytes/SHA-256, restored payload digest, conservative expanded budget, seconds per stage and archive-based export/import MiB/s.

| Stage | Included work |
| --- | --- |
| `prepare`, `seed` | Initial container creation/start and synced random-data generation; preparation costs, separate from lifecycle latency |
| `freeze` | Backend inspection and writer freeze |
| `export` | GNU tar, archive streaming/hash, fsync and tar/materialization validation |
| `local_verify` | Additional complete local archive read and SHA-256 verification |
| `source_remove` | Removing the old native container after the local archive check |
| `import` | Full SHA-256 and tar validation followed by Docker import |
| `create_start_ready` | Reconstructed container creation, start and successful Python execution probe |
| `content_verify` | Reading all restored files and checking sizes, modes and aggregate content digest |

Wall time includes preparation, content checks and resource cleanup. Per-stage export/import rates divide actual archive bytes by the entire corresponding adapter stage; they are not network rates. The expanded budget is a parser estimate, not daemon disk usage. The retained archive's size is known, but this fixture does not measure combined daemon/host temporary disk peak.

## Local baseline

On 2026-10-24, the fixture passed on Docker Engine 29.4.0, Linux arm64 under OrbStack on an Apple M4 / 32 GiB macOS host. The daemon reported 10 CPUs, 16,819,613,696 bytes of memory and the `overlayfs` storage driver. The preloaded source/helper image was `python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258`. These are individual local samples, with uncontrolled host/daemon caches, rather than percentiles or deployment SLOs.

| Payload per fixture | Concurrent fixtures | Files per fixture | Archive per fixture | Export | Local verify | Import | Create/start/ready |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 GiB | 1 | 1 | 1,225,584,640 bytes | 23.74 s | 0.45 s | 18.46 s | 1.52 s |
| 1 GiB | 4 | 1 | 1,225,584,640 bytes | 29.94–30.41 s | 0.53–0.59 s | 28.09–28.58 s | 0.35–0.42 s |
| 1 GiB | 1 | 65,536 | 1,326,254,080 bytes | 22.44 s | 0.49 s | 25.50 s | 1.16 s |

The single large-file sample exported at 49.24 MiB/s and imported at 63.31 MiB/s, including the validation and filesystem work described above. Complete fixture wall time was 47.79 s, including data generation, restored-content verification and cleanup.

The four-concurrent sample ran separately from correctness/race checks and completed in 71.01 s of fixture wall time. Each fixture exported at 38.44–39.03 MiB/s and imported at 40.90–41.61 MiB/s. Table ranges describe the four fixtures within that run, not percentile estimates or aggregate cluster throughput. A separate four-concurrent 64 MiB fixture also passed with the Go race detector; its timings are excluded from this baseline.

The small-file sample divided 1 GiB into 65,536 files of 16 KiB each. Export/import rates were 56.37 / 49.61 MiB/s; archive metadata increased its size by about 96 MiB compared with the one-file sample. Seeding and syncing every file took 42.01 s and full fixture wall time was 94.98 s. Source preparation is excluded from the suspend/resume adapter stages. Each sample verified every restored file before cleanup.

The 4 × 10 GiB headroom-negative case required 260,978,097,584 free bytes on each checked filesystem with this base image and file count. This machine had insufficient space, and the fixture refused before source creation. The failed preflight is a capacity-guard check, not a large-archive performance result.

S3 upload/download/readback, Console scheduling and durable lifecycle commits, execution-lease handoff, cache-hit resumes, image pulls, cross-physical-host behavior and end-to-end percentiles require separate acceptance. The [full performance matrix](../../../docs/plan/sandbox-session-persistence-storage-api.zh-CN.md#基本性能与容量建议) remains the release gate for deployment performance claims.
