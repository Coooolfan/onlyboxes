# Sandbox snapshot storage foundation

`internal/sandboxsnapshot` provides durable metadata and publication primitives for filesystem snapshots. `internal/objectstore.SnapshotStore` supplies a separate S3-compatible snapshot namespace. These packages do not expose suspend/resume HTTP endpoints or dispatch worker lifecycle commands.

## Publication contract

The lifecycle coordinator must stop filesystem writers before preparing an archive. It registers the running session with a pinned image and restore specification, then claims a durable suspend operation before backend side effects.

1. Construct a scoped archive reference with `SnapshotStore.Object`.
2. Reserve account storage capacity and persist the upload plan. If a multipart upload has already been created but reservation fails, abort it. Creation whose response was lost requires multipart reconciliation; it is not a completed snapshot.
3. Persist worker-reported part numbers, sizes and ETags. Use `PresignPart` to grant individual uploads without sharing bucket credentials. Persist IDs and plans, never presigned URLs.
4. `Publisher.Publish` compares the part plan with S3, completes the object conditionally, verifies its full SHA-256 through an `ArchiveVerifier`, and writes the immutable manifest. Only then does it atomically publish the current snapshot and suspended state.
5. Stop/remove the source instance and acknowledge that cleanup with `Repository.Finish`. Publication alone does not authorize deleting the source before its commit, activate a worker, or publish a terminal route.

`ArchiveVerifier` is an explicit integration boundary: worker readback can run through a scoped GET grant so large archives do not flow through Console. `SnapshotStore.VerifySnapshot` implements streaming readback for adapter conformance and integration testing; it performs HEAD and GET and does not trust ETag or user object metadata as a content digest.

A lost Complete response can be reconciled only against the reserved size and full SHA-256. A retry of manifest creation accepts identical bytes and rejects different bytes. Archive and manifest version IDs are retained separately so physical deletion can address the stored versions.

## Durable operation ownership

The SQLite migration adds stable sessions, operations, idempotency aliases, snapshots and uploaded parts. It does not modify the existing terminal route state machine.

- Equivalent concurrent requests join one operation. Every admitted idempotency key remains mapped to that operation across later suspend/resume cycles.
- Restore claims advance generation, including failed attempts. Stale generations cannot replace the current owner or publish a snapshot.
- Account scope applies to session, operation, snapshot and part lookups.
- Uploads, failed operations awaiting reconciliation, part ETags and valid snapshot references survive Console database reopen. Ordinary task startup failure recovery does not rewrite these records.
- Canonical request parameters are stored alongside their SHA-256 so asynchronous recovery retains lease and retention settings without the original HTTP request. Transfer grants and credentials must not be stored as request parameters.
- `FailAndHold` retains operation ownership when backend state is unknown. `ResolveFailed` requires the coordinator to have confirmed the source is running again, or that a restore target can no longer execute. A timeout by itself is not confirmation.
- Source cleanup and target activation require separate acknowledgements. `Finish` releases operation ownership only after the corresponding committed lifecycle phase.

For restore, reserve a compatible target before `SetRestoreTarget`; verify its readiness before `CommitRestore`; acknowledge activation before `Finish`. Publishing terminal routes and fencing worker execution remain responsibilities of the lifecycle coordinator and are not performed by the repository.

## Retention and storage accounting

Quota accounting includes every non-deleted snapshot, including uploads and prior snapshots. A new upload cannot discard the previous valid snapshot just to make space.

GC must first call `TombstoneSnapshot`. The repository refuses to tombstone the current recovery point or any snapshot associated with a session that still has an active operation. After the objects and any remaining multipart data are removed, `MarkSnapshotDeleted` releases storage accounting. Deleting objects and running the periodic GC/reconciliation loops are separate integration work; this package does not start background deletion on import.

Storage identity incorporates endpoint, region, bucket and prefix. Altering those requires an explicit migration of references. Changing admission size or multipart defaults does not invalidate already-reserved upload plans or committed objects.

## Configuration boundary

The store accepts an explicit Go `SnapshotConfig`: independent endpoint, signing region, bucket, prefix, credentials, path-style setting, maximum new archive size, multipart part size and presign TTL. It checks these values before generating any grants. It supports HTTP for local storage tests and HTTPS deployments.

The existing `CONSOLE_EXPORT_FILE_*` configuration and `exportFile` behavior remain unchanged. The planned `CONSOLE_PERSISTENCE_*` / `CONSOLE_SNAPSHOT_*` environment settings and suspend/resume APIs are not activated by this foundation. They should be wired together with the worker protocol and lifecycle coordinator so incomplete behavior cannot be exposed as a working persistence service.

## Validation

Database tests cover concurrent admission, durable idempotency, generation ownership, restart recovery, quota reservation, retention, manifest binding and GC exclusion. Publisher tests inject verification/manifest failures and verify the source ownership and previous recovery point remain intact.

Object store tests exercise signed multipart requests against an HTTP S3 protocol fixture, conditional writes, part-plan mismatches, full readback corruption, response-loss retries, abort idempotence and version-pinned deletion. They are not a certification of a production S3-compatible provider. Real bucket capability and cross-machine worker restore tests are required before enabling the lifecycle APIs.

```bash
go test -race ./internal/objectstore ./internal/sandboxsnapshot ./internal/persistence ./internal/registry
go test ./...
```

The overall target behavior and planned APIs are documented in [the lifecycle design](../../docs/plan/sandbox-session-persistence-lifecycle.zh-CN.md) and [the storage/API design](../../docs/plan/sandbox-session-persistence-storage-api.zh-CN.md).

The separate [worker protocol and restore placement](sandbox-snapshot-worker-protocol.md) implementation provides bounded controls and connection-scoped capacity reservations. It still requires runtime adapters and lifecycle/route integration before enabling public suspend/resume APIs.
