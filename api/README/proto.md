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

## Compatibility Rules
- Project is pre-release; breaking protocol refactors are allowed when all in-repo consumers are updated together.
- For external compatibility needs in the future, preserve existing field tags and avoid destructive changes.
