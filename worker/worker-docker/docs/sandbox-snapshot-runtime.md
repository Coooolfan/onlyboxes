# Docker filesystem snapshot adapter

`internal/runner/snapshot_runtime.go` implements the Docker backend primitives for a filesystem snapshot: inspect the existing container and restore specification, freeze its writers, export a private archive, verify/import an archive, reconstruct container creation arguments, explicitly roll back a source, and remove an instance by immutable backend ID.

The opt-in [lifecycle controller](sandbox-snapshot-controller.md) connects this adapter to durable operation journals, local session/generation gates, transfer/cache management, independent execution leases and registry lifecycle controls. The adapter's direct integration tests cover backend primitives; public suspend/resume acceptance uses the controller and Console/S3 path.

## Archive and configuration contract

The `docker-rootfs-v1` payload is a full uncompressed GNU tar/PAX archive of the merged container root filesystem. It includes base-image files and current changes, so deletions, renames and file/directory replacements do not depend on replaying an old writable layer. The archive preserves ordinary file content, UID/GID, modes including setuid/setgid, nanosecond mtimes, symlinks, hardlinks, POSIX ACLs, xattrs and FIFOs in the tested configuration.

A trusted, administrator-selected helper image pinned by digest supplies GNU tar. The image must already exist in the daemon's image store. The helper runs as root with a read-only root filesystem and no network, joins only the source container's PID namespace, and reads `/proc/1/root`. It requires privileged access to read the complete source filesystem metadata. It does not execute a tar binary from the sandbox or enter the host PID namespace. Joining the source namespace also avoids resolving a recycled host PID after source termination. Helpers use unique names, automatic removal and explicit cleanup on errors.

The source must already be fenced against commands/previews and drained before the caller freezes it. `docker pause` stops the container's cgroup, including background writers. This produces filesystem/crash consistency; applications that need a logical flush must perform it before suspension. Archive generation does not unpause, stop or delete the source on failure. Only an explicit rollback authorizes `Thaw`, and only a committed snapshot authorizes source removal.

The archive excludes `/proc`, `/sys`, `/dev`, contents under `/tmp` and `/run`, and Docker-managed `/etc/hosts`, `/etc/hostname` and `/etc/resolv.conf`. Docker recreates its runtime mount points and managed files for the new instance. Volumes and explicit mounts are rejected rather than silently discarded. Character/block devices are rejected. GNU tar diagnostics, including its warning when skipping a socket, invalidate an export.

`Describe` requires an existing managed terminal container and explicit memory, CPU and process limits. It resolves the image's platform manifest from the daemon's local image descriptor rather than retaining a multi-platform index digest. Untagged images without a registry identity are rejected on first adoption. A journal-owned specification supports subsequent snapshots of an imported image through `DescribePersistent`, with matching configuration and the import's archive label. Environment values, user, workdir, entrypoint arguments and command are reconstructed explicitly. Fractional CPU limits are retained.

## Integrity and space

Exports use mode-600 temporary files. Failed or incomplete files are removed. An archive is returned only after syncing it, parsing its entries, computing the full SHA-256 and checking both archive and expanded-size limits. Import verifies the size, full SHA-256, format, runtime version/platform and materialization budget before invoking Docker. Verification and import use the same open file descriptor; the lifecycle/cache owner must keep that file immutable and pinned through completion.

Expanded space is a conservative per-archive materialization budget: regular files count their logical sizes rounded to 4 KiB, hardlinks do not count a second file body, and each entry adds 4 KiB for metadata. Sparse logical extents count toward this budget because Docker import can allocate them densely. The archive parser also limits entry count to one million and rejects unsafe member/hardlink paths. The complete worker admission path must additionally reserve Docker daemon import workspace, container writes, other operations and actual filesystem headroom; this estimate alone does not establish available disk capacity.

Daemon errors and tar diagnostics never copy private metadata or sandbox-controlled paths into returned error messages. Errors distinguish unsupported configurations, integrity failures and archive/materialization quota failures.

## Compatibility and verification

The tested configuration on 2026-10-18 was Docker Engine 29.4.0, Linux arm64, with OrbStack's overlayfs image store. Image inspection requires Docker CLI/daemon support for `image inspect --platform` and a platform-specific OCI/Docker manifest descriptor. Rootless and user-namespace-remapped daemons are rejected. Other engine/filesystem/platform combinations require the same metadata roundtrip and failure tests before advertising persistence capability.

The integration test creates and modifies root filesystem data, freezes and exports it, deletes the source container, imports into a fresh image, and starts a new container with the original stable session name. It checks deletion and replacement semantics, files outside the workdir, ACLs/xattrs, modes, UID/GID, nanosecond mtimes, symlinks, hardlinks and FIFO content/types. It also checks custom execution user, environment, workdir and fractional CPU configuration. Repeating cleanup for the old immutable container ID must leave the new same-name instance intact.

Sparse-file content is preserved but physical allocation is not: the 8 MiB fixture increased from 8 to 16,392 blocks of 512 bytes after Docker import. The import budget includes that expansion. In the measured test, a roughly 152 MB archive required a roughly 196 MB expanded budget; export and import each took about 3 seconds on this machine. This is a small local fixture measurement, not a production performance guarantee.

Failure tests verify that archive quota exhaustion and unsupported sockets preserve the frozen source and remove incomplete archives. Corruption, invalid paths, devices, understated expanded budgets and canceled verification are rejected before import. The lifecycle journal must retain ownership until rollback or cleanup is acknowledged; the adapter does not infer that timeout means a backend instance disappeared.

```bash
go test -race ./...
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<pinned-digest>' \
go test -race ./internal/runner -run '^TestIntegrationDockerSnapshot' -count=1 -v
```

The helper image must provide GNU tar with ACL/xattr/sparse support, `python3` and `sh` for the test fixture. No source container, target container, helper or imported test image is retained on a successful test run. Source deletion and cold-image import happen on one daemon; this is not a two-worker, shared-object-store or cross-machine acceptance test.

An additional opt-in [native performance fixture](sandbox-snapshot-performance.md) measures large payloads and concurrent imports, with full restored-content verification and machine-readable stage timings.
