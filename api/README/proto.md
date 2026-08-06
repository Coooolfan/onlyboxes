# Proto Guide

## Files
- `proto/registry/v1/registry.proto`: shared worker registry API.
- `gen/go`: generated Go code.

## Prerequisites
Install generators:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

Make sure `$(go env GOPATH)/bin` is in your `PATH`.

## Generate

```bash
./scripts/gen-go.sh
```

## Terminal Session Capacity

Sandbox workers send `ConnectHello.terminal_session_capacity` on every connection:

- message absence means legacy/unknown capacity;
- `max_active_sessions=0` in a present message means unlimited;
- positive `max_active_sessions` declares the worker-local session limit;
- `active_session_count` is the reservation count when Hello is built and closes the reconnect window before the first heartbeat;
- later `HeartbeatFrame.active_session_count` values refresh only the active count.

`worker-sys` does not send this declaration. Console treats the snapshot as a scheduling hint; each sandbox worker remains the final capacity authority.

## Compatibility Rules
- Project is pre-release; protocol refactors are allowed when all in-repo consumers are updated together.
- Keep protobuf field tags stable within one rollout to avoid generator/caller mismatches.
