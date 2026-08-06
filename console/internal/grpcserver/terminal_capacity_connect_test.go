package grpcserver

import (
	"context"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestActiveSessionTerminalCapacityPresence(t *testing.T) {
	now := time.Unix(1_700_200_000, 0)
	legacy := newActiveSessionAt("legacy", "session-legacy", &registryv1.ConnectHello{}, now)
	legacySnapshot := legacy.terminalSessionCapacitySnapshot()
	if legacySnapshot.known {
		t.Fatalf("legacy hello must remain capacity unknown: %#v", legacySnapshot)
	}

	unlimited := newActiveSessionAt("unlimited", "session-unlimited", &registryv1.ConnectHello{
		TerminalSessionCapacity: &registryv1.TerminalSessionCapacity{
			MaxActiveSessions:  0,
			ActiveSessionCount: 9,
		},
	}, now)
	unlimitedSnapshot := unlimited.terminalSessionCapacitySnapshot()
	if !unlimitedSnapshot.known || unlimitedSnapshot.maxActiveSessions != 0 || unlimitedSnapshot.activeSessionCount != 9 {
		t.Fatalf("explicit unlimited capacity was not preserved: %#v", unlimitedSnapshot)
	}
	if !unlimitedSnapshot.observedAt.Equal(now) {
		t.Fatalf("unexpected initial observation time: %s", unlimitedSnapshot.observedAt)
	}
}

func TestConnectInitializesAndHeartbeatUpdatesTerminalCapacity(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_200_100, 0)
	svc := NewRegistryService(store, map[string]string{"node-1": "secret-1"}, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, sessionID, err := connectWorkerWithHello(client, &registryv1.ConnectHello{
		NodeId:       "node-1",
		WorkerSecret: "secret-1",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 4}},
		TerminalSessionCapacity: &registryv1.TerminalSessionCapacity{
			MaxActiveSessions:  4,
			ActiveSessionCount: 3,
		},
	})
	if err != nil {
		t.Fatalf("connect worker: %v", err)
	}
	defer stream.CloseSend()

	session := svc.getSession("node-1")
	if session == nil {
		t.Fatal("expected active session")
	}
	initial := session.terminalSessionCapacitySnapshot()
	if !initial.known || initial.maxActiveSessions != 4 || initial.activeSessionCount != 3 || !initial.observedAt.Equal(now) {
		t.Fatalf("unexpected initial capacity snapshot: %#v", initial)
	}

	now = now.Add(time.Second)
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:             "node-1",
				SessionId:          sessionID,
				ActiveSessionCount: 2,
			},
		},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	if response, err := stream.Recv(); err != nil || response.GetHeartbeatAck() == nil {
		t.Fatalf("receive heartbeat ack: response=%#v err=%v", response, err)
	}

	updated := session.terminalSessionCapacitySnapshot()
	if !updated.known || updated.maxActiveSessions != 4 || updated.activeSessionCount != 2 || !updated.observedAt.Equal(now) {
		t.Fatalf("unexpected updated capacity snapshot: %#v", updated)
	}
}

func TestReconnectReplacesTerminalCapacityBeforeFirstHeartbeat(t *testing.T) {
	now := time.Unix(1_700_200_150, 0)
	svc := NewRegistryService(
		registrytest.NewStore(t),
		map[string]string{"node-1": "secret-1"},
		5,
		15,
		time.Minute,
	)
	svc.nowFn = func() time.Time { return now }
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	firstStream, _, err := connectWorkerWithHello(client, &registryv1.ConnectHello{
		NodeId:                  "node-1",
		WorkerSecret:            "secret-1",
		Capabilities:            []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 4}},
		TerminalSessionCapacity: terminalCapacity(4, 1),
	})
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	defer firstStream.CloseSend()

	now = now.Add(time.Second)
	secondStream, _, err := connectWorkerWithHello(client, &registryv1.ConnectHello{
		NodeId:                  "node-1",
		WorkerSecret:            "secret-1",
		Capabilities:            []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 4}},
		TerminalSessionCapacity: terminalCapacity(4, 4),
	})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer secondStream.CloseSend()

	session := svc.getSession("node-1")
	if session == nil {
		t.Fatal("expected replacement session")
	}
	snapshot := session.terminalSessionCapacitySnapshot()
	if !snapshot.known || snapshot.maxActiveSessions != 4 || snapshot.activeSessionCount != 4 || !snapshot.observedAt.Equal(now) {
		t.Fatalf("replacement Hello did not initialize capacity: %#v", snapshot)
	}
}

func TestValidateHelloRejectsNegativeTerminalCapacity(t *testing.T) {
	tests := []struct {
		name     string
		capacity *registryv1.TerminalSessionCapacity
	}{
		{
			name: "negative max",
			capacity: &registryv1.TerminalSessionCapacity{
				MaxActiveSessions: -1,
			},
		},
		{
			name: "negative active",
			capacity: &registryv1.TerminalSessionCapacity{
				ActiveSessionCount: -1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHello(&registryv1.ConnectHello{
				NodeId:                  "node-1",
				TerminalSessionCapacity: tc.capacity,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestHandleHeartbeatRejectsNegativeActiveSessionCount(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_200_200, 0)
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return now }
	hello := &registryv1.ConnectHello{
		NodeId: "node-1",
		TerminalSessionCapacity: &registryv1.TerminalSessionCapacity{
			MaxActiveSessions:  4,
			ActiveSessionCount: 3,
		},
	}
	if err := store.Upsert(hello, "session-1", now); err != nil {
		t.Fatalf("upsert worker: %v", err)
	}
	session := newActiveSessionAt("node-1", "session-1", hello, now)

	err := svc.handleHeartbeat(context.Background(), session, &registryv1.HeartbeatFrame{
		NodeId:             "node-1",
		SessionId:          "session-1",
		ActiveSessionCount: -1,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if got := session.terminalSessionCapacitySnapshot().activeSessionCount; got != 3 {
		t.Fatalf("negative heartbeat changed active count to %d", got)
	}
}

func TestLegacyHeartbeatUpdatesActiveCountWithoutClaimingCapacityKnown(t *testing.T) {
	now := time.Unix(1_700_200_300, 0)
	session := newActiveSessionAt("legacy", "session-legacy", &registryv1.ConnectHello{}, now)
	session.setActiveSessionCount(7, now.Add(time.Second))
	snapshot := session.terminalSessionCapacitySnapshot()
	if snapshot.known || snapshot.activeSessionCount != 7 {
		t.Fatalf("legacy heartbeat changed capacity presence semantics: %#v", snapshot)
	}
}

func TestInflightStatsExposeTerminalCapacityPresence(t *testing.T) {
	now := time.Unix(1_700_200_350, 0)
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	addTerminalCapacityTestWorker(t, svc, "known", now, terminalCapacity(4, 3))
	addTerminalCapacityTestWorker(t, svc, "legacy", now, nil)

	byNode := make(map[string]WorkerInflightSnapshot)
	for _, snapshot := range svc.InflightStats() {
		byNode[snapshot.NodeID] = snapshot
	}
	known := byNode["known"]
	if known.ActiveSessionCount != 3 || !known.TerminalSessionCapacity.Known || known.TerminalSessionCapacity.MaxActiveSessions != 4 {
		t.Fatalf("unexpected known inflight snapshot: %#v", known)
	}
	legacy := byNode["legacy"]
	if legacy.TerminalSessionCapacity.Known {
		t.Fatalf("legacy capacity unexpectedly known: %#v", legacy)
	}
}

func TestResolveWorkerSysHelloDropsTerminalCapacity(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_200_400, 0)
	if seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-sys",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second); seeded != 1 {
		t.Fatalf("expected one seeded worker, got %d", seeded)
	}
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)

	resolved, err := svc.resolveHelloByWorkerType(&registryv1.ConnectHello{
		NodeId: "node-sys",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: computerUseCapabilityDeclared, MaxInflight: 2},
			{Name: readImageCapabilityDeclared, MaxInflight: 2},
		},
		TerminalSessionCapacity: &registryv1.TerminalSessionCapacity{
			MaxActiveSessions:  4,
			ActiveSessionCount: 1,
		},
	})
	if err != nil {
		t.Fatalf("resolve worker-sys hello: %v", err)
	}
	if resolved.GetTerminalSessionCapacity() != nil {
		t.Fatalf("worker-sys retained terminal capacity: %#v", resolved.GetTerminalSessionCapacity())
	}
}
