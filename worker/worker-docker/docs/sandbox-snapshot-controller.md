# Docker snapshot lifecycle controller and transfers

`internal/runner/snapshot_controller*.go` joins the runtime adapter, operation journal, local session fences and archive transport behind an internal `Handle` entry point for `SandboxPersistenceControl` messages. It supports source preparation/upload/verification/finalization and target reservation/restore/activation/cancellation/inspection. Worker startup, authenticated stream dispatch, reconnect reconciliation and periodic maintenance are not wired to this controller yet. Workers do not advertise persistence capability.

## Source lifecycle

An initial read-only inspection reports the existing native container specification and lease. It durably records that existing backend ID for a narrow generation-1 bootstrap: Console can adopt the session without allowing a blank replacement if operation admission fails after inspection. The probe survives process restart and rejects a later same-name replacement, including delayed legacy creation. This bootstrap permits only the inspected existing instance; later durable generations use the journal fence.

Preparation reserves staging capacity, closes local command/proxy admission, durably claims the source, drains accepted command slots, pauses the complete container cgroup, exports and syncs a private archive, then records its size, expanded budget and SHA-256. Retried frozen preparation verifies the stored artifact and rejects an unexpectedly running/unpaused source. A failed preparation retains source ownership for explicit rollback.

Upload actions read only the journal-owned archive. They acknowledge exact part numbers, byte counts and opaque ETags. Full remote readback uses a separate GET grant and records successful verification durably. Source finalization requires that verification marker and Console's committed-snapshot flag before removing the immutable container ID. The finalized archive can remain as a cache of the committed snapshot.

Failed-source rollback thaws the same backend and reports its confirmed lease/specification/identity. Console supplies the expected private specification when local preparation failed before a journal claim, allowing the controller to verify and recover that source for rollback. A lease extension for an active persistent session is written to the journal before command/renewal acknowledgement, so unrelated journal updates cannot shorten a newer confirmed lease.

## Target lifecycle

Target preparation validates runtime/version/platform/format, reserves disk and session capacity, and persists the operation before import or container creation. Admission checks the private staging filesystem and Docker daemon filesystem, budgets a second archive copy plus materialized data, and adds 20 percent headroom. Existing pending reservations participate in local admission; Console scheduling remains a hint.

Restore first considers a committed/verified local artifact matching the exact scoped session, snapshot descriptor and restore specification. It pins and fully hashes the file, then links it under the new operation's private artifact name. An absent or invalid cache falls back to the supplied download grant for that same snapshot. There is no fallback to another snapshot or an empty image.

An operation-specific image tag makes a lost import acknowledgement discoverable. Container creation uses the original stable session name but a new native ID, plus operation and generation labels. The chosen image/container IDs are durable before startup; retries inspect and reuse those resources. The execution lease starts immediately before startup. Readiness verifies the running instance and execution environment while local user entry remains closed.

Activation requires the same operation, generation and live lease. Ordinary delayed cancellation cannot delete an already-active instance. Console uses an explicit destructive cancellation flag only while reconciling published but expired target ownership; that operation still holds Console's execution gate. A target is reported canceled only after its owned container has been removed. Shared snapshot data is never consumed by a failed restore.

## Direct transfers and cache

`internal/runner/snapshot_transfer.go` performs Worker-to-object-store HTTP requests using ephemeral grants. It validates methods/ranges/expiry, refuses redirects, disables transparent response decompression, bounds all transfers by one Worker-wide concurrency limit and cancels requests with no byte progress. Errors omit URLs, credentials and response bodies. Expired grants and retryable network/storage responses request a fresh control/grant through the `busy` result.

Private partial downloads survive transient failures. A resumed reply must describe exactly the requested remainder; a server that ignores Range restarts from byte zero. Only a complete file with matching size and full SHA-256 is synced and atomically published for import. Verification and import use a fixed open descriptor. Grant URLs and signed headers are never part of journal records.

Cache eviction considers only verified shared-snapshot copies. Current held operations and pinned files are protected. Unused copies can be removed for TTL, cache budget or staging pressure. Uncommitted source archives are excluded from cache eviction. Periodic eviction, obsolete-artifact/image GC and terminal journal compaction still need lifecycle maintenance wiring.

## Validation and remaining integration

Unit/race tests cover resumed/ignored ranges, corrupted content, redirects, expired grants, idle transfer cancellation, exact multipart acknowledgements, private errors, cache pins, uncommitted-source protection, canceled-operation replay and delayed cancellation after activation.

A real Docker integration test invokes typed lifecycle messages directly. It creates source data, freezes and exports, transfers parts through HTTP, verifies full remote content, finalizes source cleanup, restores using the same-node cache, and activates a new instance under the same session ID. It then removes the target and restores the same snapshot and stable session through a fresh controller/journal with no local cache, exercising the actual download path. It also checks that transfer secrets are absent from journal state.

```bash
go test -race ./...
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<pinned-digest>' \
go test -race ./internal/runner -run '^TestIntegrationSnapshotController' -count=1 -v
```

The HTTP fixture exercises real archive PUT/GET but is not a certified S3 provider. The test uses one Docker daemon and direct handler calls; it does not prove registry-stream wiring, Console HTTP APIs, source/target process-crash recovery, lease/watchdog maintenance or cross-machine scheduling. These integrations, BoxLite support and retention/deletion acceptance remain required before persistence is complete or enabled.
