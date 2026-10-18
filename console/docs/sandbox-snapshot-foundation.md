# Sandbox snapshot storage foundation

`internal/sandboxsnapshot` provides durable metadata, filesystem snapshot publication and atomic terminal route transitions. `internal/objectstore.SnapshotStore` supplies a separate S3-compatible snapshot namespace. The internal `grpcserver.SnapshotCoordinator` executes and reconciles lifecycle controls using these primitives. Management suspend/resume and operation polling endpoints are wired to the coordinator behind `CONSOLE_PERSISTENCE_ENABLED` (default false). Startup validates the separate S3 namespace and probes its required capabilities before enabling admission. See [configuration](config-file.md#sandbox-persistence). The Docker Worker supplies a native adapter; BoxLite integration remains pending.

## Publication contract

The lifecycle coordinator opts in an existing session with `RegistryService.AdoptSnapshotSession`, supplying its pinned image and restore specification. Adoption requires a matching unexpired execution route and cannot overwrite an existing lifecycle or specification. It claims a durable suspend operation through `RegistryService.BeginSnapshotOperation` before backend side effects. The worker must stop all filesystem writers before preparing an archive.

1. Construct a scoped archive reference with `SnapshotStore.Object`.
2. Reserve account storage capacity and persist the upload plan. If a multipart upload has already been created but reservation fails, abort it. Creation whose response was lost requires multipart reconciliation; it is not a completed snapshot.
3. Persist worker-reported part numbers, sizes and ETags. Use `PresignPart` to grant individual uploads without sharing bucket credentials. Persist IDs and plans, never presigned URLs.
4. `Publisher.Publish` compares the part plan with S3, completes the object conditionally, verifies its full SHA-256 through an `ArchiveVerifier`, and writes the immutable manifest. Only then does it atomically publish the current snapshot, suspended state and removal of the source execution route. Stable preview records remain until their own expiry or logical session deletion.
5. Stop/remove the source instance and acknowledge that cleanup with `Repository.Finish`. Publication alone does not authorize deleting the source before its commit, activate a worker, or publish a terminal route.

`ArchiveVerifier` is an explicit integration boundary: worker readback can run through a scoped GET grant so large archives do not flow through Console. `SnapshotStore.VerifySnapshot` implements streaming readback for adapter conformance and integration testing; it performs HEAD and GET and does not trust ETag or user object metadata as a content digest.

A lost Complete response can be reconciled only against the reserved size and full SHA-256. A retry of manifest creation accepts identical bytes and rejects different bytes. Archive and manifest version IDs are retained separately so physical deletion can address the stored versions.

## Durable operation ownership

SQLite records stable sessions, operations, idempotency aliases, snapshots and uploaded parts separately from transient execution routes. The repository publishes lifecycle state and execution route changes in the same transaction.

- Equivalent concurrent requests join one operation. Every admitted idempotency key remains mapped to that operation across later suspend/resume cycles.
- Restore claims advance generation, including failed attempts. Stale generations cannot replace the current owner or publish a snapshot.
- Account scope applies to session, operation, snapshot and part lookups.
- Uploads, failed operations awaiting reconciliation, part ETags and valid snapshot references survive Console database reopen. Ordinary task startup failure recovery does not rewrite these records.
- Canonical request parameters are stored alongside their SHA-256 so asynchronous recovery retains lease and retention settings without the original HTTP request. Transfer grants and credentials must not be stored as request parameters.
- `FailAndHold` retains operation ownership when backend state is unknown. `ResolveFailed` requires the coordinator to have confirmed the source is running again, or that a restore target can no longer execute. A timeout by itself is not confirmation.
- Source cleanup and target activation require separate acknowledgements. `Finish` releases operation ownership only after the corresponding committed lifecycle phase.

For restore, reserve a compatible target before `SetRestoreTarget`; verify its readiness and absolute execution lease before `CommitRestore(ctx, operation, worker, leaseExpiresUnixMS)`. That transaction records the new owner, execution route and preview worker binding. Call `RegistryService.RefreshSnapshotRoute` to refresh the in-memory route while the operation still holds the execution gate, then acknowledge activation before `Finish`. `Finish` refuses an expired or missing execution route. Worker activation itself remains the lifecycle coordinator's responsibility.

`ResolveFailed` republishes a confirmed source route with its worker-confirmed absolute lease after failed suspend, even if ordinary lease pruning removed the old route. The state, route and operation release commit atomically. For failed resume, it removes a committed restore target's route atomically when the coordinator has confirmed that target can no longer execute. The previous valid snapshot and its retention survive failed activation.

## Execution and preview gates

Console consults durable lifecycle ownership before task submission, command dispatch, lease renewal and preview authorization, including when an in-memory source route remains after suspend publication. A lifecycle-owned session returns `session_state_conflict`; a running persistent session without a live route is unavailable. `create_if_missing` is forced to false for persistent commands, and commands carry their current `generation`. Runtime adapters must enforce that generation and their own local lifecycle fences before execution.

`RegistryService.BeginSnapshotOperation` serializes lifecycle admission against persistent command enqueue. A command accepted before suspension can finish, but its captured generation cannot republish the source route or extend a later generation's lease. Database route guards also reject publication for suspended/restoring sessions and incorrect worker ownership. Direct repository admission is a storage primitive; the running lifecycle coordinator must use the RegistryService entry point.

Ordinary terminal reconnect recovery excludes frozen sources and unactivated targets owned by persistence operations. Worker persistence journals must protect those resources and reconcile them through lifecycle controls before this feature is enabled; ordinary orphan cleanup cannot own them.

Public preview URLs remain scoped to the stable account/session. Resolution obtains the current execution target rather than trusting the cached worker ID. Suspend and restore keep previews closed; activation lets the same unexpired URL reach the new worker. Existing preview TTLs remain independent of suspended-data retention. Ordinary non-persistent sessions still remove their previews when their runtime route is removed. Startup route pruning preserves durable operations and persistent preview records.

## Retention and storage accounting

Quota accounting includes every non-deleted snapshot, including uploads and prior snapshots. A new upload cannot discard the previous valid snapshot just to make space.

GC must first call `TombstoneSnapshot`. The repository refuses to tombstone the current recovery point or any snapshot associated with a session that still has an active operation. After the objects and any remaining multipart data are removed, `MarkSnapshotDeleted` releases storage accounting. Deleting objects and running the periodic GC/reconciliation loops are separate integration work; this package does not start background deletion on import.

Storage identity incorporates endpoint, region, bucket and prefix. Altering those requires an explicit migration of references. Changing admission size or multipart defaults does not invalidate already-reserved upload plans or committed objects.

## Configuration boundary

The store accepts an explicit Go `SnapshotConfig`: independent endpoint, signing region, bucket, prefix, credentials, path-style setting, maximum new archive size, multipart part size and presign TTL. It checks these values before generating any grants. It supports HTTP for local storage tests and HTTPS deployments.

The existing `CONSOLE_EXPORT_FILE_*` configuration and `exportFile` behavior remain unchanged. The planned `CONSOLE_PERSISTENCE_*` / `CONSOLE_SNAPSHOT_*` environment settings and suspend/resume APIs are not activated by this foundation. They should be wired together with the worker protocol and lifecycle coordinator so incomplete behavior cannot be exposed as a working persistence service.

## Validation

Database tests cover concurrent admission, durable idempotency, generation ownership, restart recovery, quota reservation, retention, manifest binding and GC exclusion. Route tests inject deletion/insertion failures to verify atomic lifecycle/route rollback, stable preview preservation and rebinding, failed activation cleanup and rejection of stale route writes. Migration tests cover existing rows and scope backfill. Publisher tests inject verification/manifest failures and verify the source ownership and previous recovery point remain intact.

Object store tests exercise signed multipart requests against an HTTP S3 protocol fixture, conditional writes, part-plan mismatches, full readback corruption, response-loss retries, abort idempotence and version-pinned deletion. They are not a certification of a production S3-compatible provider. The startup probe checks the configured endpoint from Console before enabling admission; deployment acceptance must additionally cover real Worker access, failure behavior and cross-machine restores.

```bash
go test -race ./internal/objectstore ./internal/sandboxsnapshot ./internal/persistence ./internal/registry
go test ./...
```

The overall target behavior and planned APIs are documented in [the lifecycle design](../../docs/plan/sandbox-session-persistence-lifecycle.zh-CN.md) and [the storage/API design](../../docs/plan/sandbox-session-persistence-storage-api.zh-CN.md).

The separate [worker protocol and restore placement](sandbox-snapshot-worker-protocol.md) implementation provides bounded controls and connection-scoped capacity reservations. BoxLite lifecycle integration, unregistered storage/runtime orphan GC, independent runtime lease enforcement and real cross-machine acceptance are still required before the persistence feature is complete.


## Asynchronous lifecycle executor

`grpcserver.SnapshotCoordinator` runs accepted operations under the server context, independently of HTTP request cancellation. Its bounded recovery loop scans pending operations and unresolved failures. Canonical retention and lease parameters, target assignment, upload plans and completed parts are durable; presigned URLs and connection reservations remain ephemeral.

Suspend requires the worker to report a frozen archive with the original restore specification, full SHA-256, compressed archive size and expanded storage budget. Multipart grants are issued in bounded batches; acknowledged part results are persisted. Completion, full worker-side readback, immutable manifest publication and the atomic suspended-state commit precede source cleanup. A resume cannot start until source cleanup is acknowledged. Lost Complete responses also use worker-side verification, through `CompleteUploadWithVerifier`, so the Console never downloads archive bodies on that recovery path.

Resume prefers the original compatible worker and persists its target before side effects. Target readiness must include the matching specification, instance identity and future absolute lease. Route publication precedes activation, while the operation gate remains closed. A recreated coordinator or replacement connection first inspects the same target's durable operation. A confirmed ready instance is activated without importing again; a missing journal is canceled and reconciled before another operation can be admitted. A lost activation acknowledgement can finish by inspecting the already-active operation, including after its original deadline.

Worker-reported errors, timeouts and disconnects keep operation ownership until rollback, source cleanup or target cancellation is confirmed. An unconfirmed cancellation never releases the target reservation or opens execution. Failure codes and messages are sanitized rather than copying worker messages that may contain grants or environment secrets.

`api/sandboxpersistence` defines shared private JSON payloads. Restore specifications require a pinned image, exact backend version/platform/format, CPU and memory limits, and a positive process limit. CPU limits retain fractional values. Manifests bind expanded storage to the reserved upload plan; restore placement budgets both archive and expanded workspace.

Protocol-actor tests use real HTTP archive PUT/GET, the real SQLite repository and registry dispatch. They cover multipart publication and remote readback, request cancellation, first-session adoption, corruption rollback, source cleanup ordering, no compatible worker, unconfirmed cancellation, missing target journals, coordinator/connection replacement, and lost activation acknowledgements. These actors do not execute Docker or BoxLite. Real runtime export/import and cross-machine acceptance remain required.

## Management views and logical deletion

Session list/get responses combine ordinary running routes with durable lifecycle records. Suspended and restoring sessions remain visible without a live route; their worker/status/lease fields are null. Private restore specifications and object-store references are excluded from public projections. Successful lease renewal returns the same lifecycle-aware session view.

Suspend/resume require management cookie, API key or Dashboard JIT authentication plus an `Idempotency-Key`. They accept strict, bounded JSON requests, return `202` with a management operation polling URL, and preserve operation identities across retries and restarts. An already stable target can return `200`. Execution tokens do not authorize these endpoints. Admin item requests select an account explicitly; account isolation also applies to operation lookups. Operation errors expose allowlisted codes and sanitized messages, with unavailable transfer progress represented by zero completed bytes and a null total.

`DELETE` atomically creates a durable cleanup job, marks the stable session deleting, advances its generation, cancels dispatchable lifecycle work and removes its execution/preview routes. GET/list/operation polling hide deleted sessions, including stale route-cache entries. Old operation completions and idempotency retries cannot republish a deleted session. Snapshot references stay retained until native cleanup is confirmed.

`SnapshotCleaner` processes the durable deletion queue and retention policy independently of new-operation admission. Required current owners must confirm purge before shared references are cleared; historical cache holders are tracked separately. Failed storage deletion retains quota state and version identities for retry. Deleted and old unreferenced snapshots are tombstoned transactionally before storage work; active restore/upload references prevent collection.

Archive/manifest keys are atomically replaced with small deletion markers. These markers retain the old version IDs before version deletion, so a lost reply or restart cannot discard the identities needed for physical cleanup. Conditional writes cannot recreate those keys after deletion. The account-scoped cleanup stats endpoint exposes durable backlog/failure/retry counters. Unregistered storage orphans, unregistered runtime debris and journal compaction remain separate cleanup work.
