package grpcserver

import (
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
)

func TestSnapshotRecoveryReplacesStaleCachedRouteAndPreservesLegacyCandidates(t *testing.T) {
	svc, _, _, scope := setupSnapshotRouteTest(t)
	ordinary := []*registryv1.TerminalSessionRecoveryCandidate{{SessionId: scope, LeaseExpiresUnixMs: 1}, {SessionId: "legacy", LeaseExpiresUnixMs: 123}}
	running, held, required, err := svc.snapshotRecoveryPlan(context.Background(), "worker-a", ordinary)
	if err != nil || !required || len(running) != 2 || len(held) != 0 || running[1].Generation != 1 || running[1].LeaseExpiresUnixMs <= 1 {
		t.Fatalf("running recovery lost durable generation/lease: %v %v %v", running, held, err)
	}
	op := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
	running, held, required, err = svc.snapshotRecoveryPlan(context.Background(), "worker-a", ordinary)
	if err != nil || !required || len(running) != 1 || running[0].SessionId != "legacy" || len(held) != 1 || held[0].OperationId != op.ID || held[0].Role != registryv1.SandboxPersistenceRecoveryCandidate_SOURCE {
		t.Fatalf("snapshot source remained ordinary or lost protection: %v %v %v", running, held, err)
	}
}

func TestSnapshotRecoveryConnectAckProtectsSourceAndRejectsUnsupportedWorker(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			svc, _, _, scope := setupSnapshotRouteTest(t)
			op := beginRouteSnapshotTest(t, svc, sandboxsnapshot.Suspend, "suspend")
			restarted := NewRegistryService(svc.store, map[string]string{"worker-a": "secret"}, 5, 15, time.Minute)
			client, cleanup := newBufClient(t, restarted)
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream, err := client.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.CloseSend()
			hello := snapshotTestHello("worker-a")
			hello.SandboxPersistence.Enabled = enabled
			if err := stream.Send(&registryv1.ConnectRequest{Payload: &registryv1.ConnectRequest_Hello{Hello: hello}}); err != nil {
				t.Fatal(err)
			}
			response, err := stream.Recv()
			if !enabled {
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("unsupported worker was allowed to clean lifecycle resources: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ack := response.GetConnectAck()
			if ack == nil || len(ack.GetTerminalSessionRecoveryCandidates()) != 0 || len(ack.GetSandboxPersistenceRecoveryCandidates()) != 1 {
				t.Fatalf("wrong recovery acknowledgement: %v", response)
			}
			held := ack.GetSandboxPersistenceRecoveryCandidates()[0]
			if held.OperationId != op.ID || held.ScopedSessionId != scope || held.Generation != 1 || held.Role != registryv1.SandboxPersistenceRecoveryCandidate_SOURCE {
				t.Fatal("lost durable source ownership", held)
			}
		})
	}
}

func TestSnapshotRecoveryRejectsDisabledWorkerAfterRouteExpires(t *testing.T) {
	svc, _, _, _ := setupSnapshotRouteTest(t)
	if _, err := svc.store.Persistence().SQL.ExecContext(context.Background(), `DELETE FROM terminal_session_routes`); err != nil {
		t.Fatal(err)
	}
	restarted := NewRegistryService(svc.store, map[string]string{"worker-a": "secret"}, 5, 15, time.Minute)
	client, cleanup := newBufClient(t, restarted)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseSend()
	hello := snapshotTestHello("worker-a")
	hello.SandboxPersistence = nil
	if err := stream.Send(&registryv1.ConnectRequest{Payload: &registryv1.ConnectRequest_Hello{Hello: hello}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("expired route allowed destructive ordinary recovery", err)
	}
}
