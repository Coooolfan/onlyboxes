# Snapshot operation ownership and recovery

The Docker Worker uses a private journal and session fences for filesystem persistence. With `WORKER_PERSISTENCE_ENABLED=true`, the [lifecycle controller](sandbox-snapshot-controller.md) and independent lease guard are installed before persistence-capable registration and authenticated lifecycle dispatch. Persistence is disabled by default.

## Durable journal

`openSnapshotJournal` opens an explicitly selected absolute private directory and binds it to one Worker ID. The directory and artifacts directory must exclude group/other access. State and lock files use mode 600; state/lock symlinks are rejected. An exclusive kernel file lock prevents two processes from sharing ownership.

The versioned JSON state records current session ownership and operation records: scoped session ID, operation ID, generation, role, phase, private restore specification, immutable archive identity, local artifact basename, immutable container/image IDs, absolute lease and staging reservation. Initial probes separately persist their immutable native IDs and nondecreasing absolute leases; admission stays closed until the independent guard confirms ownership. Probed roots survive ordinary lease expiry and orphan cleanup. Transfer grants, storage credentials and signed request headers are not part of this schema. Restore specifications can contain environment secrets and must not be logged.

Writes create a private temporary file, sync its contents, atomically replace the state file, and sync the parent directory. In-memory state changes only after persistence succeeds. Any write failure disables further decisions for that journal instance. Corrupt, incomplete, incompatible or differently owned state fails startup; it does not become an empty journal. State is limited to 64 MiB. Lifecycle/cache GC remains responsible for retiring artifacts and obsolete records before this limit is reached.

Claims serialize session ownership. Source operations can claim the same generation of an active session; restore targets require a newer generation after prior terminal ownership. Phase updates cannot alter scope, generation, role, restore specification, chosen backend IDs or a known archive. Canceled/finished operations cannot re-enter preparation. Superseded operations cannot mutate the current session.

Only empty `kind=cancel` tombstones below the retained current generation are pruned during a claim. Their schema excludes restore specifications, native IDs, archives, artifacts and lease/staging state, and the newer current generation rejects delayed controls. An empty artifact basename on a regular source/target record does not prove its native image, root or export intent was retired. Those records remain available to native cleanup and restart recovery. Full resource-aware history compaction remains pending; the 64 MiB limit continues to fail closed rather than dropping ownership metadata.

A `deleting` or `deleted` current operation is a permanent stable-session fence. `CancelUnknown` rejects cancellations for that scope even when they carry a higher generation; they cannot replace the deletion with an ordinary cancellation tombstone. Claims cannot replace it with another operation, including a new deletion ID. The controller rejects controls for other operation IDs before native inspection or mutation; setting `purge_session` on a different action or operation does not bypass the fence. Matching deletion inspect/purge retries retain their existing identity and behavior.

Startup validates historical records against the current scope/generation. A deletion record must remain that scope's current operation, and normal resource operations cannot exceed the current generation. Historical empty cancellation records may refer to a future canceled request without becoming current ownership. Inconsistent history fails startup with the original journal retained; it is not rewritten as an empty or reusable session.

## Local execution and cleanup fences

Session execution, lease renewal and file access carry the durable generation. An inactive lifecycle gate, stale/missing generation or unknown positive generation returns `session_state_conflict`. A persistent command requires its existing local instance even when `create_if_missing` is true. It cannot construct a blank replacement.

Preview route tokens bind their HMAC-protected `generation` to the active local execution generation. Ordinary sessions use generation zero. The Worker checks the signed generation both before waiting for readiness and immediately before resolving the upstream; request headers cannot override it. Holding a session cancels existing proxy contexts. After restoration, including on the same Worker, tokens from an earlier generation cannot access the new instance.

`HoldSource` closes admission under the same lock used by session claims and lease cleanup. Existing command slots can drain. A timed-out accepted command records a drain failure while preserving the source. Entering the hold cancels existing proxy contexts and stops the source's ordinary lease timer. All background writers must still be frozen with the runtime adapter after a successful drain.

Ordinary cleanup never removes a journal-owned persistent root filesystem. For an active persistent session, expiry or unsafe command cancellation stops execution using the immutable container ID and retains its disk pending lifecycle reconciliation/deletion. A failed stop is not proof of backend termination. Held sources remain owned across ordinary expiry and Worker close. Retained resources continue to consume a local session reservation, including when they are absent from the manager's live session map. Snapshot finalization/deletion must perform explicit, acknowledged cleanup through the lifecycle controller; GC must account for retained disk.

Commands and file operations use the recorded immutable container ID after adoption. This prevents an old session pointer from executing in a later instance that reuses the same name. Proxy access remains gated by local lifecycle state and its session cancellation context.

## Console reconnect handshake

Console creates running-session candidates and lifecycle-held resources from one SQLite snapshot. A target transitioning from activation to ordinary running ownership appears in exactly one category, so it cannot disappear between two independent reads.

`TerminalSessionRecoveryCandidate.generation` is zero for ordinary sessions and positive for persistent sessions. `ConnectAck.sandbox_persistence_recovery_candidates` identifies unresolved source/target operations by scoped session, operation ID and generation. Source hints report committed shared snapshots; target hints report committed running-route ownership. Hints contain no restore environments or transfer credentials.

A Worker must install these hints before ordinary terminal recovery/orphan cleanup. They protect resources even when a local journal is missing. A known, explicitly activated or rolled-back local operation can remain active when the hint was captured just before Console finished it; the hint still protects it from orphan discovery without disabling its lease handling. An unconfigured Worker rejects lifecycle recovery before ordinary cleanup. Console also rejects reconnects without persistence capability when that Worker owns persistent running sessions or unresolved lifecycle resources.

Ordinary persistent recovery verifies generation and immutable backend ID in addition to existing session labels. Ambiguous or different same-name resources are neither adopted nor removed. Lifecycle resources are never restarted through ordinary recovery. Journal-aware registry/controller initialization, target lease reconciliation, transfer/cache ownership, explicit finalization and retention GC must be connected before enabling persistence.

## Validation

Unit and race tests cover exclusive journal ownership, reopen, private modes, state corruption, write failure, immutable metadata, operation/generation replay, command/resource/renewal gates, drain timeout, lease expiry, retained capacity, missing-journal hints, late activation hints and ambiguous same-name resources. Console tests exercise the real gRPC acknowledgement and concurrent activation completion against SQLite.

A real Docker test freezes and archives a source, closes its manager and journal, reopens them, and runs ordinary recovery with no source candidate. The source remains paused with its original backend ID. Explicit rollback then recovers the same root filesystem and reads its original data.

```bash
go test -race ./...
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<pinned-digest>' \
go test -race ./internal/runner -run '^TestIntegrationSnapshotJournalRestart' -count=1 -v
```

This validates local ownership/restart primitives. Real Console/S3 local two-Worker recovery and deletion acceptance are described in the [controller documentation](sandbox-snapshot-controller.md#real-console--s3-acceptance); cross-physical-machine and broader failure acceptance remain separate gates. Journal regressions also cover higher-generation unknown cancellation after deletion, reopen, rejection before native I/O, and retention of native cleanup identity when artifact names are empty.
