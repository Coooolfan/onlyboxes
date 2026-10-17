# Sandbox snapshot worker protocol

Console's internal snapshot control channel uses the authenticated WorkerRegistry `Connect` stream. It is separate from `CommandDispatch`, ordinary capability inflight accounting and terminal command task recovery. This transport and restore placement implementation does not enable public suspend/resume endpoints. Runtime snapshot adapters, durable operation execution, route publication and configuration wiring are still required.

## Runtime declaration and placement

`ConnectHello.sandbox_persistence` is an opt-in declaration. Workers must provide a supported runtime (`docker` or `boxlite`), backend version, guest platform (`linux/amd64` or `linux/arm64`), supported archive read/write formats, an operation limit from 1 to 64 and non-negative available staging space. Enabled declarations require explicit terminal session capacity. Worker build versions and user labels do not establish archive compatibility. The first protocol version recognizes `docker-rootfs-v1` and `boxlite-archive-v1` for their respective runtimes.

An enabled worker must include `HeartbeatFrame.sandbox_persistence` with current operation usage and available staging bytes. An absent declaration does not receive snapshot controls. Existing workers currently omit the declaration and retain their existing execution behavior; the BoxLite protobuf consumers explicitly omit the optional fields.

`RegistryService.ReserveSnapshotRestore` checks readiness, heartbeat freshness, runtime, exact backend version, guest platform and archive read format. It atomically reserves staging bytes, an operation slot and a terminal session slot before returning a `PersistenceReservation`. `StagingBytes` must include the runtime import workspace as well as archive storage; a local archive cache does not eliminate this space requirement. Local reservations and worker usage are counted conservatively and may overlap temporarily. Worker-local capacity checks remain authoritative.

The original worker can be preferred, but any compatible available worker can be selected. Session capacity, disk shortage, incompatibility, exclusion or offline workers can leave no candidate (`ErrNoCapabilityWorker`, mapped to the existing `no_worker` error). Exhausted persistence operation slots return `ErrNoWorkerCapacity` (the existing `no_capacity` error). Placement does not publish a route or activate an instance.

Reservations are pinned to the authenticated connection that admitted them. A replacement connection cannot inherit their controls. After reconnect, durable operations must be inspected and worker-local reservations reconciled before placement can be retried. `Release` is idempotent and is only appropriate after confirmed cleanup or ownership handoff; a timeout never releases the reservation automatically.

## Control and report envelopes

`SandboxPersistenceControl` carries a new request ID per delivery, durable operation ID, account-scoped session ID, generation, action, absolute deadline and bounded JSON metadata. Supported actions are prepare snapshot, upload, remote verification, finalize suspend, prepare restore, restore, activate, cancel and inspect. Metadata may contain private restore specifications and scoped transfer grants; it must not be logged. Archive bytes do not belong in these frames.

`SandboxPersistenceReport` echoes the complete control identity. A final report contains either valid JSON or a `CommandError`. Progress reports do not complete the delivery or extend its deadline. The payload limit is 64 KiB for both directions. Console validates operation, session, generation and action against the pending delivery before accepting its result. Unknown late/duplicate deliveries have no durable side effects. Mismatched reports reject the stream. Connection replacement or closure wakes pending callers.

`DispatchPersistenceControl` is internal transport: its caller must own the durable operation and generation before dispatch. A `PersistenceControlError` with `MayHaveExecuted=false` indicates local rejection before enqueue. Once enqueued, cancellation, timeout, disconnection or a worker failure can leave backend side effects, so `MayHaveExecuted=true` requires lifecycle reconciliation. A worker error alone does not prove its instance stopped. Transport never automatically retries a control, moves ownership or opens execution.

The durable operation record retains canonical request parameters alongside its hash, so restart recovery can recover retention and lease settings without the original HTTP request. It must not contain transfer grants or storage credentials.

## Validation

Tests cover a real in-process gRPC control/report exchange while heartbeats continue, progress/final separation, scope and generation fencing, local validation, disconnect/cancellation uncertainty, late result handling, runtime compatibility, original-node fallback, reconnect rejection and independent concurrent limits for session slots, operation slots and staging bytes. These tests validate the control plane; they do not demonstrate a runtime snapshot export or a cross-machine restore.

```bash
go test -race ./internal/grpcserver ./internal/sandboxsnapshot
```

See the [storage foundation](sandbox-snapshot-foundation.md), [lifecycle design](../../docs/plan/sandbox-session-persistence-lifecycle.zh-CN.md) and [storage/API design](../../docs/plan/sandbox-session-persistence-storage-api.zh-CN.md).
