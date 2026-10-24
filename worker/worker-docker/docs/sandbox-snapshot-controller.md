# Docker snapshot lifecycle controller and transfers

`internal/runner/snapshot_controller*.go` joins the runtime adapter, operation journal, local session fences and archive transport behind an internal `Handle` entry point for `SandboxPersistenceControl` messages. It supports source preparation/upload/verification/finalization and target reservation/restore/activation/cancellation/inspection. Worker startup binds the controller to the authenticated registry stream when `WORKER_PERSISTENCE_ENABLED=true`. Hello advertises the probed backend version/platform/formats and available staging capacity; heartbeat reports held operation reservations. Persistence remains disabled by default. Configuration is described in [config-file.md](config-file.md).

Control admission happens synchronously on the receive loop before asynchronous native/HTTP work, preserving cancellation order without blocking heartbeats. Connection epochs reject late frames from a replaced stream. Disconnect closes persistent execution/proxy admission and cancels that connection's transfers; the durable journal retains ownership for reconnect. Before ordinary orphan cleanup, recovery validates Console generation/ownership against the journal and native identity, retains held sources/targets, and restores acknowledged active ownership with Console's exact live lease. Unknown generations cannot create an empty replacement.

A one-second maintenance loop cancels expired unactivated targets, stops expired active backends while retaining their rootfs, and evicts eligible cache copies. It pauses lifecycle admission while connection recovery runs. Shutdown waits for accepted controls, stops journal-owned running backends, including unfinished source preparation, while retaining paused sources, and releases the private journal lock after cleanup. The verified [independent lease guard](runtime-lease-guard.md) enforces expiry when the Worker is lost; process-local maintenance additionally handles cache and lifecycle work. Host failure and unknown start outcomes still require explicit ownership reconciliation.

## Source lifecycle

An initial read-only inspection reports the existing native container specification and lease. It durably records that existing backend ID and absolute lease, gates new session entry and confirms the independent guard receipt before returning success for a narrow generation-1 bootstrap: Console can adopt the session without allowing a blank replacement if operation admission fails after inspection. The probe survives process restart and rejects a later same-name replacement, including delayed legacy creation. This bootstrap permits only the inspected existing instance; later durable generations use the journal fence.

Preparation reserves staging capacity, closes local command/proxy admission, durably claims the source, drains accepted command slots, pauses the complete container cgroup, exports and syncs a private archive, then records its size, expanded budget and SHA-256. Retried frozen preparation verifies the stored artifact and rejects an unexpectedly running/unpaused source. A failed preparation retains source ownership for explicit rollback.

Upload actions read only the journal-owned archive. They acknowledge exact part numbers, byte counts and opaque ETags. Full remote readback uses a separate GET grant and records successful verification durably. Source finalization requires that verification marker and Console's committed-snapshot flag before removing the immutable container ID. The finalized archive can remain as a cache of the committed snapshot.

Failed-source rollback thaws the same backend and reports its confirmed lease/specification/identity. Console supplies the expected private specification when local preparation failed before a journal claim, allowing the controller to verify and recover that source for rollback. A lease extension for an active persistent session is written to the journal before command/renewal acknowledgement, so unrelated journal updates cannot shorten a newer confirmed lease.

## Target lifecycle

Target preparation validates runtime/version/platform/format, reserves disk and session capacity, and persists the operation before import or container creation. Admission checks the private staging filesystem and Docker daemon filesystem, budgets a second archive copy plus materialized data, and adds 20 percent headroom. Existing pending reservations participate in local admission; Console scheduling remains a hint.

Restore first considers a committed/verified local artifact matching the exact scoped session, snapshot descriptor and restore specification. It pins and fully hashes the file, then links it under the new operation's private artifact name. An absent or invalid cache falls back to the supplied download grant for that same snapshot. There is no fallback to another snapshot or an empty image.

An operation-specific image tag and immutable import-tag label make a lost import acknowledgement discoverable. Container creation uses the original stable session name but a new native ID, plus operation and generation labels. The chosen image/container IDs are durable before startup; retries inspect and reuse those resources. The execution lease starts immediately before startup. Readiness verifies the running instance and execution environment while local user entry remains closed.

When an [independent guard](runtime-lease-guard.md) is installed, controlled startup requires its matching lease acknowledgement before starting the container; readiness, activation and reconnect also validate its receipt. Rollback persists its lease before thaw/start, and retries preserve the recorded deadline. Renewal does not succeed from a journal intent alone. Native cleanup includes the guard’s serialized forget acknowledgement. Production startup installs the verified helper before Hello. Initial adoption requires a live guard receipt; its renewal and reconnect use the exact immutable instance and recorded lease, and expiry retains the rootfs.

Activation requires the same operation, generation and live lease. Ordinary delayed cancellation cannot delete an already-active instance. Console uses an explicit destructive cancellation flag only while reconciling published but expired target ownership; that operation still holds Console's execution gate. A target is reported canceled only after its owned container has been removed. Shared snapshot data is never consumed by a failed restore.

## Direct transfers and cache

`internal/runner/snapshot_transfer.go` performs Worker-to-object-store HTTP requests using ephemeral grants. It validates methods/ranges/expiry, refuses redirects, disables transparent response decompression, bounds all transfers by one Worker-wide concurrency limit and cancels requests with no byte progress. Errors omit URLs, credentials and response bodies. Expired grants and retryable network/storage responses request a fresh control/grant through the `busy` result.

Private partial downloads survive transient failures. A resumed reply must describe exactly the requested remainder; a server that ignores Range restarts from byte zero. Only a complete file with matching size and full SHA-256 is synced and atomically published for import. Verification and import use a fixed open descriptor. Grant URLs and signed headers are never part of journal records.

Cache eviction considers only verified shared-snapshot copies. Current held operations and pinned files are protected. Unused copies can be removed for TTL, cache budget or staging pressure. Uncommitted source archives are excluded from cache eviction. Periodic TTL/budget eviction runs in lifecycle maintenance. Retired import images use the independent retirement checks below. Completed-deletion history is [compacted with its cleanup locators retained](sandbox-snapshot-journal.md#completed-deletion-compaction); unregistered native debris and running/suspended history maintenance still require separate work.

`TestSnapshotCacheCopyOverlapsPressureEviction` deterministically pauses the real cache-copy path at its first checksum read, with the source file pinned and the destination unpublished. Concurrent zero-budget eviction retains that source. Completing the copy permits source eviction while preserving the destination bytes; canceling the copy publishes no destination and releases the pin. Both cases leave journal ownership bytes unchanged. `TestSnapshotCacheEvictedBeforeCopyIsAMiss` covers the opposite ordering. `TestSnapshotCacheIdentityMismatchIsAMiss` rejects another snapshot/checksum, backend version, platform or startup environment without changing the retained archive. These tests use real private files and journal/controller code; they do not run a Docker import or establish full Console/S3 eviction-overlap acceptance.

### Retired import images

Repeated restore/suspend cycles need not retain every imported Docker image until permanent session deletion. The background maintenance loop collects an operation's import tag and operation-labeled dangling images only after the journal supplies one of these proofs:

- That target was confirmed canceled, after exact native removal and lease-release acknowledgement.
- The same scope, generation, immutable container ID and restore specification appear in a finalized source whose archive was committed and fully verified remotely. Finalization includes physical source removal and lease release.
- The scope has a confirmed physical deletion fence at a higher generation.

A newer operation, an expired lease, a frozen source or a committed-but-not-finalized snapshot is insufficient. Current active/ready/importing targets retain their images. The collector never removes a container, changes a lease, updates lifecycle state, discards journal identities or deletes archive caches. Delayed controls remain fenced by the existing operation/generation rules.

Cleanup acquires the same per-session action admission as lifecycle controls and rechecks the journal after admission. Reconnect pauses admission, and shutdown cancels/joins accepted cleanup jobs. Each maintenance pass starts at most `WORKER_SNAPSHOT_MAX_INFLIGHT` jobs, sharing action admission; each job is bounded to ten seconds. Each eligible operation is revisited at most once per minute, including after success, because an ambiguously acknowledged import can appear late. Journal reopen retains the retirement proof; in-memory retry timing restarts on Worker startup. Native errors keep the operation metadata and emit a sanitized pending-cleanup warning.

Image deletion verifies the operation tag, archive checksum and native image identity/operation label. It uses no forced removal. Another container referencing the image prevents physical deletion; another image tag remains outside the collector's authority. A conflict retains the data for a later attempt. Unregistered images and imports without a durable known archive identity are retained for separate reconciliation; this is not a daemon-wide prune.

For an operation-labeled dangling image, the collector creates a deterministic owned `:gc-<image-ID>` tag and removes that reference instead of deleting by image ID. A foreign tag added after inspection remains a separate reference when Docker removes the GC tag. A crash after tagging leaves an operation-labeled image discoverable on the next pass, including when it is no longer dangling. Only the operation's main tag and derived GC tags belong to this cleanup path; other tags are preserved. Native regression `TestIntegrationSnapshotImportGCReferenceSurvivesCrashAndPreservesConcurrentForeignTag` recreates the interrupted-tagging window and injects a foreign tag immediately before deletion, then verifies recovery and replay preserve that reference.

`TestIntegrationSnapshotRetiredImportsProtectLiveRootsAndRetryLateImages` uses real Docker imports and containers to verify active/frozen-root protection, finalization, a borrowed-image conflict, retry after the borrower is removed, archive-cache retention and background cleanup after journal reopen and a late import. The fixture uses tiny non-executed roots and does not replace full runtime or cross-machine acceptance:

```bash
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<pinned-digest>' \
go test -race ./internal/runner -run '^TestIntegrationSnapshotRetiredImports' -count=1 -v
```

## Validation and remaining integration

Unit/race tests cover resumed/ignored ranges, corrupted content, redirects, expired grants, idle transfer cancellation, exact multipart acknowledgements, private errors, cache pins, uncommitted-source protection, canceled-operation replay and delayed cancellation after activation.

A real Docker integration test invokes typed lifecycle messages directly. It creates source data, freezes and exports, transfers parts through HTTP, verifies full remote content, finalizes source cleanup, restores using the same-node cache, and activates a new instance under the same session ID. It then removes the target and restores the same snapshot and stable session through a fresh controller/journal with no local cache, exercising the actual download path. It also checks that transfer secrets are absent from journal state.

```bash
go test -race ./...
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<pinned-digest>' \
go test -race ./internal/runner -run '^TestIntegrationSnapshotController' -count=1 -v
```

Additional tests exercise a real gRPC stream during a blocked HTTP upload: heartbeats remain live, disconnect cancels the request, reconnect accepts controls and frozen ownership survives. A native Worker `Run` test verifies startup capability advertisement, lifecycle dispatch, durable cancellation acknowledgement and journal lock release on shutdown. Native journal-reopen tests retain paused sources, roll back the same instance, recover running ownership, reject stale lease expiry and stop expired backends without deleting the sole writable rootfs. Ready-but-unactivated targets are also checked for lease cleanup.

The HTTP fixture exercises real archive PUT/GET but is not a certified S3 provider. Native tests use one Docker daemon. A separate actual Worker `Run` process SIGKILL test covers independent expiry and retained rootfs after initial adoption and ordinary renewal against a registry fixture. These fixtures do not prove every lifecycle crash boundary or cross-machine scheduling.

### Real Console / S3 acceptance

2026-10-24 acceptance on macOS arm64 with OrbStack, Docker 29.4.0, the real Console and MinIO passed a complete lifecycle using two independent Docker Worker identities and journal volumes. After source suspension and physical source finalization, stopping the source Worker forces the peer to restore the same session from shared storage. File SHA-256, permissions and symlinks survive. Creating and modifying files and deleting a file on the restored instance, then suspending and restoring again on that Worker, preserves all changes. Stopping both Workers produces the existing asynchronous `no_worker` result and retains the snapshot; reconnect permits restoration. Permanent deletion finishes with zero pending session/Worker/snapshot cleanup jobs.

The local entry point is `scripts/dev.sh start sandbox-persistence-docker`, followed by `python3 scripts/tests/snapshot_e2e.py --runtime docker`. See [development acceptance](../../../scripts/README.md#本地沙箱持久化验收) for isolated state/configuration, required image pins and the optional return-to-source cache check. Docker's independent helpers remain supervised across Worker stops, and the orchestration retains their bound volumes.

The additional `--return-to-source` cycle also passed: after the peer changes files and commits newer snapshots, only the original Worker is brought online for the last restore. Before startup, the test reads the stopped source's volume, confirms the older committed cache still exists and fully matches its original SHA-256, and requires a distinct latest snapshot ID. The peer's additions/modifications/deletions survive restoration, and bringing both Workers back permits permanent cleanup to drain to zero. A separate run observed retryable `no_capacity` immediately after source finalization while the prior inflight heartbeat was still visible; its operation resolved with the session suspended and the same committed snapshot. This is conservative admission evidence, not a measured sustained-capacity test.

These are two logical Workers on one daemon and one physical host. They establish the real API/publication/S3 transfer/recovery/cleanup path, while cross-physical-host scheduling, broader fault injection, host-reboot recovery, unregistered native orphan reconciliation, journal maintenance and measured performance budgets remain outstanding.

## Session deletion and late controls

A private `CANCEL` with `purge_session` advances to Console's durable deletion generation. It cancels older lifecycle actions, writes a deletion journal fence, removes the immutable native instance and the scope's import tags, archives, partial downloads, export temporary files and owned export helpers. The fence survives journal reopen and prevents stale rollback, ordinary creation and higher-generation reuse of the deleted stable identity.

Unknown cancellation cannot replace a `deleting` or `deleted` scope, including with a higher generation. Once claimed, the deletion operation ID is fixed: controls for other operation IDs are refused before native I/O, including a new purge ID or a restore carrying a purge flag. The journal independently rejects ownership replacement and inconsistent persisted deletion history. Native deletion replay verifies the same stopped name marker after rejection; the prior generation and cleanup identities remain retained.

A stopped, read-only helper container reserves the deterministic native name. It contains no sandbox archive, mounts or user startup environment and consumes no terminal session slot. Already-accepted Docker creation requests encounter that reserved name; no user start action is issued for the marker. Workers sharing a Docker daemon can acknowledge the same global deletion marker. The marker remains control metadata and is not an active sandbox.

Export temporary filenames and helper names bind to the durable operation identity, allowing an interrupted export to be cleaned without touching another operation. Deletion replays inspect the same fence and retry known artifact/tag cleanup. Worker maintenance rechecks each completed deletion at most once per minute, independently of Console cleanup jobs, to collect native imports that finish after the purge acknowledgement. Imports carry their operation tag as an immutable image label; cleanup also removes matching dangling images left by repeated imports. Images with other tags are retained, and cleanup never forces removal of an image used by another container. A newer native generation or a mismatched owned identity is preserved and reported as a conflict. Unregistered runtime debris and non-deleted history compaction remain separate GC work.

Completed deletion now commits private journal v2 cleanup records before returning `deleted`. They discard full environments and startup configuration while retaining the exact source/export/artifact and target/import/checksum locators used above. Matching replays verify a supplied full specification against the saved SHA-256; private maintenance may omit it only for that already completed deletion. Native tests recreate late archive/partial/export files and imports after compaction, reopen the journal, and verify cleanup and the unchanged native name marker. Active or unresolved histories remain full and cannot be treated as disposed merely because a cache file is missing.
