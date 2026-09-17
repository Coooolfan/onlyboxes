package grpcserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRenewTerminalSessionLeaseDispatchesToBoundWorkerAndPersists(t *testing.T) {
	now := time.UnixMilli(1_730_000_000_000)
	store := registrytest.NewStore(t)
	service := NewRegistryService(store, nil, 5, 15, time.Minute)
	service.nowFn = func() time.Time { return now }
	scopedSessionID := scopeTerminalSessionID("owner-a", "session-a")
	service.terminalSessionToNode[scopedSessionID] = terminalSessionRoute{
		NodeID: "worker-1", LeaseExpiresUnixMs: now.Add(time.Minute).UnixMilli(),
		CreatedAtUnixMs: now.Add(-time.Minute).UnixMilli(), RecoveryState: terminalSessionRecoveryReady,
	}
	session := newActiveSessionAt("worker-1", "connection-1", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalLeaseRenew, MaxInflight: 2}},
	}, now)
	session.markRecoveryComplete()
	service.swapSession(session)

	resultCh := make(chan struct {
		view TerminalSessionView
		err  error
	}, 1)
	go func() {
		view, err := service.RenewTerminalSessionLease(context.Background(), "owner-a", "session-a", 300)
		resultCh <- struct {
			view TerminalSessionView
			err  error
		}{view: view, err: err}
	}()

	dispatch := (<-session.commandOutbound).GetCommandDispatch()
	if normalizeCapability(dispatch.GetCapability()) != taskCapabilityTerminalLeaseRenew {
		t.Fatalf("unexpected capability %q", dispatch.GetCapability())
	}
	request := terminalLeaseRenewPayload{}
	if err := json.Unmarshal(dispatch.GetPayloadJson(), &request); err != nil {
		t.Fatalf("decode renewal payload: %v", err)
	}
	if request.SessionID != scopedSessionID || request.LeaseTTLSec != 300 {
		t.Fatalf("unexpected renewal payload: %#v", request)
	}
	wantExpiry := now.Add(5 * time.Minute).UnixMilli()
	payload, _ := json.Marshal(struct {
		SessionID          string `json:"session_id"`
		LeaseExpiresUnixMS int64  `json:"lease_expires_unix_ms"`
	}{SessionID: scopedSessionID, LeaseExpiresUnixMS: wantExpiry})
	session.resolvePending(&registryv1.CommandResult{CommandId: dispatch.GetCommandId(), PayloadJson: payload})

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("renew lease: %v", result.err)
	}
	if result.view.LeaseExpiresUnixMs != wantExpiry || result.view.WorkerID != "worker-1" {
		t.Fatalf("unexpected renewed view: %#v", result.view)
	}
	persisted, err := store.LoadActiveTerminalSessionRoutes(context.Background(), now.UnixMilli())
	if err != nil {
		t.Fatalf("load persisted routes: %v", err)
	}
	if len(persisted) != 1 || persisted[0].LeaseExpiresUnixMs != wantExpiry {
		t.Fatalf("renewed lease was not persisted: %#v", persisted)
	}
}

func TestTerminalLeaseRenewCapabilityCannotBeSubmittedAsUserTask(t *testing.T) {
	service := NewRegistryService(nil, nil, 5, 15, time.Minute)
	_, err := service.SubmitTask(context.Background(), SubmitTaskRequest{
		OwnerID: "owner-a", Capability: "terminalLeaseRenew", InputJSON: []byte(`{"session_id":"session-a","lease_ttl_sec":300}`),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected internal capability rejection, got %v", err)
	}
}
