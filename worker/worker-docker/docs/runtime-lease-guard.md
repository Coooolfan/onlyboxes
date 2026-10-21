# Independent Docker execution lease guard

`internal/runtimelease` and `cmd/runtime-lease-guard` implement a Docker-side lease enforcer whose process and timers are independent of the Worker. Its immutable container IDs and durable lease records survive the loss of the process that issued a lease. The helper stops expired containers while retaining their writable layers.

The lifecycle controller accepts a private guard client for controlled restore/rollback, durable renewal, running/activation checks, reconnect validation and native cleanup. Production `Run` does not yet launch or install that client, and initial-session adoption is not yet guarded. Helper identity/volume reconciliation, production configuration and initial adoption must be connected before persistence can rely on independent lease enforcement. The isolated fixtures below are not Console-connected Worker SIGKILL, production shared-store or cross-machine acceptance tests.

## Ownership and controls

The helper connects only to a configured local Docker Unix socket and negotiates the daemon's API version. It requires API 1.42 or newer and explicitly overrides the sandbox's stop signal with `SIGKILL`, with no graceful wait, so lease expiry cannot invoke a guest signal handler. See the [Docker API signal override](https://docs.docker.com/reference/api/engine/version-history/#v142-api-changes). Before arming or stopping an instance, it verifies the full 64-character immutable container ID, Worker ID, scoped-session hash, native generation and managed `terminalExec` labels. An ordinary unlabeled native generation is accepted only for the initial generation one. Native sandbox containers must have no automatic restart policy. No stop operation addresses a name, deletes a container, unpauses it explicitly or starts it.

Controls use a private Unix socket, with no TCP listener. `arm`, `renew` and `revoke` carry scope, generation, full container ID, absolute expiry and an expected revision. Each accepted mutation advances the revision and fsyncs its private record before acknowledgement. Exact accepted requests can be replayed; stale revisions, changed scope/native generation and shortened leases are rejected. `renew` cannot revive an expired or revoked lease. Explicit `arm` first confirms that an expired prior execution has stopped, and can then authorize the caller's controlled boot. It never starts that boot itself.

`ping` checks private control-channel readiness without issuing a lease. `status` returns an existing record and rejects an expired lease that has not yet been durably marked revoked. The control client validates the exact acknowledged identity/revision and refuses a live receipt that is already expired on arrival.

A record keeps the native generation fixed for its full container ID; rollback re-arms that same generation. A restored generation uses a new native ID. One scope cannot acquire a second local native ID while its prior lease record exists. `forget` releases a record only after Docker confirms that the full immutable ID no longer exists; a stopped retained root is insufficient. Lifecycle ownership and cancellation must still wait for every accepted start/import action to finish before native deletion and ownership transfer. A stop response alone does not settle an outstanding start request.

## Controller ordering and retries

With a guard installed, target startup persists its native ID and absolute lease, obtains the matching guard acknowledgement, starts the container and checks that same lease before ready/activation. A running target with missing or revoked guard ownership cannot be rearmed through a restore replay. Source rollback persists `rolling_back` and its lease before any native start/unpause. Rolling-back retries retain that deadline; completed rollback replays cannot extend it or restart an expired instance.

Renewal persists the desired absolute expiry before requesting a guard CAS. A stored intent alone is insufficient proof of RPC completion: retry still queries/renews the guard and requires an exact receipt. The private exec client accepts only bounded, strict JSON replies from the immutable helper ID. Missing-record evidence uses a dedicated `not_found` stdout code; generic process errors or daemon diagnostics cannot authorize initial arming. Scope, generation, immutable ID, revision and lease are checked on acknowledgements.

After native deletion, cleanup calls `forget` before returning its lifecycle acknowledgement. Even a missing status must pass through the guard's per-ID forget lock: an earlier arm may have inspected the old root while its durable write is still in progress. A revision conflict keeps cleanup pending. Logical deletion may fence a newer generation while forgetting the older, physically removed native ID; it cannot forget a newer or foreign record.

## Persistence and enforcement

The state directory is private to the helper's effective UID, bound to one Worker, and protected by an exclusive lock. Each immutable ID has a separate bounded JSON record. Writes use a private temporary file, file fsync, atomic rename and directory fsync. Loading refuses unknown records, inconsistent IDs, symlinks, hardlinks, public modes and another Worker's owner marker. Interrupted write debris is removed only after the complete directory and matching ownership have been validated. The helper retains at most 16,384 native records; unremoved roots continue to consume that capacity.

Live leases use both absolute wall time and a monotonic deadline. Clock rollback cannot extend the existing lease. A guardian restart revokes historical leases before admitting controls because continuity of the former monotonic clocks cannot be proved. New explicit authority is required to arm them again. Journal write failure closes admission and causes known IDs to be stopped, including the ID whose acknowledgement failed.

The enforcement loop polls at 100 ms and performs at most eight concurrent native calls, each bounded to five seconds. Revoked IDs remain watched, so a late start is stopped on subsequent passes. These are loop settings, not a guaranteed stop latency under Docker failure or overload. A native timeout or ownership mismatch never proves that writers stopped. The lifecycle coordinator must retain ownership until confirmed cleanup; elapsed wall time alone is insufficient evidence for failover. Clean helper shutdown closes control admission, joins accepted handlers and enforcement work, revokes retained leases and attempts their stops.

## Helper deployment contract

The helper is a trusted control-plane component with Docker socket access. It receives no Console secret, registry credentials, snapshot grants or sandbox environment. The state volume must be durable and separate from guest storage. A helper container needs Docker init, no network, dropped capabilities, `no-new-privileges`, a private state volume and `--restart=always`. Native sandbox containers themselves must retain `restart=no`.

Before the first sandbox lease, the launcher must verify the helper's immutable image/container identity, volume and socket bindings, readiness and a successful continuous run of at least ten seconds. Docker documents this stabilization requirement for restart policy supervision. Manually stopping the helper disables automatic restart until an explicit start or daemon restart; removing or manually stopping an in-use helper therefore requires stopping its protected executions first. See [Docker restart policy details](https://docs.docker.com/engine/containers/start-containers-automatically/#restart-policy-details).

`Dockerfile.runtime-lease-guard` builds a static, minimal Linux helper from repository-root context. Its `GO_IMAGE` build argument must be pinned by digest. The resulting deployment image must also be pinned by digest. It exposes only the helper binary, which includes the private Unix control client; it does not require a Docker CLI or shell in the production helper image. Worker-side packaging and startup reconciliation remain required.

## Validation

Controller tests cover durable rollback-before-boot ordering, rejected unconfirmed renewal, lost arm acknowledgements, strict receipt identity and serialized missing-record cleanup. The controller transfer/cache fixture uses separate guard processes and state volumes for two logical Workers on one daemon; it covers verified restore, activation, renewal and cold HTTP download. It does not cover initial adoption or production helper launch.

Unit/race tests cover monotonic expiry under clock rollback, revision replay, denied implicit revival, restart revocation, stalled unrelated native admission, immutable-root cleanup, private journal validation, interrupted writes, Docker identity rejection, diagnostic redaction, duplicate listener rejection and orderly control-server shutdown.

The opt-in Docker fixture builds only the auxiliary Linux helper and creates uniquely owned temporary containers and a state volume. A pinned fixture image supplies `sh` and `sleep`. It verifies issuance from an owned subprocess followed by SIGKILL, expiry while the sandbox is paused, retention of written rootfs data, repeated stopping after a late start, and actual guardian-process SIGKILL followed by Docker restart and durable revocation. Fault injection addresses only processes inside the owned helper container. Container and volume cleanup does not touch existing Workers or services.

```sh
go test -race ./internal/runtimelease
DOCKER_INTEGRATION=1 \
DOCKER_SNAPSHOT_HELPER_IMAGE='repository@sha256:<digest>' \
go test -race ./internal/runtimelease -run '^TestIntegrationDockerIndependentLease$' -count=1 -v
```
