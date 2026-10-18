package grpcserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/sandboxsnapshot"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
	"google.golang.org/protobuf/proto"
)

func snapshotTestSpec() sandboxsnapshot.RestoreSpec {
	return sandboxsnapshot.RestoreSpec{Runtime: "docker", RuntimeVersion: "29.4.0", Platform: "linux/arm64", Format: "docker-rootfs-v1", Image: "example/runtime@sha256:" + strings.Repeat("a", 64), MemoryMiB: 256, CPUs: 1, MaxProcesses: 128}
}

func snapshotTestHello(id string) *registryv1.ConnectHello {
	return &registryv1.ConnectHello{
		NodeId: id, WorkerSecret: "secret", ExecutorKind: "docker",
		Capabilities:            []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 4}},
		TerminalSessionCapacity: terminalCapacity(4, 0),
		SandboxPersistence: &registryv1.SandboxPersistenceCapabilities{
			Enabled: true, Runtime: "docker", RuntimeVersion: "29.4.0", GuestPlatform: "linux/arm64",
			ReadFormats: []string{"docker-rootfs-v1"}, WriteFormats: []string{"docker-rootfs-v1"}, MaxInflight: 4, AvailableStagingBytes: 1024,
		},
	}
}

func snapshotTestControl(id string) *registryv1.SandboxPersistenceControl {
	return &registryv1.SandboxPersistenceControl{RequestId: id, OperationId: "operation-1", ScopedSessionId: scopeTerminalSessionID("owner-1", "session-1"), Generation: 2, Action: registryv1.SandboxPersistenceControl_PREPARE_RESTORE, DeadlineUnixMs: time.Now().Add(5 * time.Second).UnixMilli(), PayloadJson: []byte(`{}`)}
}

func snapshotTestReport(c *registryv1.SandboxPersistenceControl) *registryv1.SandboxPersistenceReport {
	return &registryv1.SandboxPersistenceReport{RequestId: c.RequestId, OperationId: c.OperationId, ScopedSessionId: c.ScopedSessionId, Generation: c.Generation, Action: c.Action, Completed: true, Phase: "reserved", PayloadJson: []byte(`{"reserved":true}`)}
}

func addSnapshotTestWorker(t *testing.T, svc *RegistryService, hello *registryv1.ConnectHello) *activeSession {
	t.Helper()
	if err := validateHello(hello); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Upsert(hello, "connection-"+hello.NodeId, svc.nowFn()); err != nil {
		t.Fatal(err)
	}
	session := newActiveSession(hello.NodeId, "connection-"+hello.NodeId, hello)
	svc.swapSession(session)
	return session
}

func TestSnapshotRestoreCompatibilityAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*activeSession)
		want error
	}{
		{"compatible", func(*activeSession) {}, nil},
		{"legacy", func(s *activeSession) { s.persistence.capabilities = nil }, ErrNoCapabilityWorker},
		{"disabled", func(s *activeSession) { s.persistence.capabilities.Enabled = false }, ErrNoCapabilityWorker},
		{"runtime", func(s *activeSession) { s.persistence.capabilities.Runtime = "boxlite" }, ErrNoCapabilityWorker},
		{"runtime version", func(s *activeSession) { s.persistence.capabilities.RuntimeVersion = "29.3.0" }, ErrNoCapabilityWorker},
		{"guest architecture", func(s *activeSession) { s.persistence.capabilities.GuestPlatform = "linux/amd64" }, ErrNoCapabilityWorker},
		{"read format", func(s *activeSession) { s.persistence.capabilities.ReadFormats = nil }, ErrNoCapabilityWorker},
		{"full sessions", func(s *activeSession) { s.terminalCapacity.activeSessionCount = 4 }, ErrNoCapabilityWorker},
		{"unknown capacity", func(s *activeSession) { s.terminalCapacity.known = false }, ErrNoCapabilityWorker},
		{"staging space", func(s *activeSession) { s.persistence.availableBytes = 1 }, ErrNoCapabilityWorker},
		{"busy", func(s *activeSession) { s.persistence.reportedInflight = 4 }, ErrNoWorkerCapacity},
		{"offline", func(s *activeSession) { s.lastHeartbeatUnixMs.Store(time.Now().Add(-time.Minute).UnixMilli()) }, ErrNoCapabilityWorker},
		{"reconciling", func(s *activeSession) { s.recoveryComplete = false }, ErrNoCapabilityWorker},
		{"closed", func(s *activeSession) { s.close(errors.New("disconnected")) }, ErrNoCapabilityWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
			worker := addSnapshotTestWorker(t, svc, snapshotTestHello("worker"))
			tc.edit(worker)
			reservation, err := svc.ReserveSnapshotRestore(RestorePlacement{Spec: snapshotTestSpec(), StagingBytes: 100})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if reservation != nil {
				reservation.Release()
			}
		})
	}
}

func TestSnapshotRestoreReservationsCannotOverbook(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sessions int32
		slots    int32
		bytes    int64
	}{
		{"session slots", 2, 8, 1024},
		{"staging bytes", 8, 8, 200},
		{"operation slots", 8, 2, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) { testSnapshotRestoreReservationsCannotOverbook(t, tc.sessions, tc.slots, tc.bytes) })
	}
}

func testSnapshotRestoreReservationsCannotOverbook(t *testing.T, sessions, slots int32, bytes int64) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	hello := snapshotTestHello("worker")
	hello.TerminalSessionCapacity.MaxActiveSessions = sessions
	hello.SandboxPersistence.MaxInflight = slots
	hello.SandboxPersistence.AvailableStagingBytes = bytes
	worker := addSnapshotTestWorker(t, svc, hello)
	var wg sync.WaitGroup
	results := make(chan *PersistenceReservation, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := svc.ReserveSnapshotRestore(RestorePlacement{Spec: snapshotTestSpec(), StagingBytes: 100})
			if err == nil {
				results <- r
			} else if !errors.Is(err, ErrNoCapabilityWorker) && !errors.Is(err, ErrNoWorkerCapacity) {
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	close(results)
	if len(results) != 2 {
		t.Fatalf("admitted %d, want exactly 2", len(results))
	}
	for r := range results {
		r.Release()
		r.Release()
	}
	worker.persistence.mu.Lock()
	defer worker.persistence.mu.Unlock()
	if worker.persistence.reservedBytes != 0 || worker.persistence.reservations != 0 {
		t.Fatalf("reservation leak: %d bytes, %d sessions", worker.persistence.reservedBytes, worker.persistence.reservations)
	}
}

func TestSnapshotControlLocalRejectionAndDisconnect(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	worker := addSnapshotTestWorker(t, svc, snapshotTestHello("worker"))
	for _, edit := range []func(*registryv1.SandboxPersistenceControl){
		func(c *registryv1.SandboxPersistenceControl) { c.ScopedSessionId = "unscoped" },
		func(c *registryv1.SandboxPersistenceControl) { c.Generation = 0 },
		func(c *registryv1.SandboxPersistenceControl) {
			c.DeadlineUnixMs = time.Now().Add(-time.Second).UnixMilli()
		},
		func(c *registryv1.SandboxPersistenceControl) {
			c.Action = registryv1.SandboxPersistenceControl_ACTION_UNSPECIFIED
		},
		func(c *registryv1.SandboxPersistenceControl) { c.PayloadJson = []byte(`{`) },
	} {
		control := snapshotTestControl("delivery-1")
		edit(control)
		_, err := svc.DispatchPersistenceControl(context.Background(), "worker", control)
		var local *PersistenceControlError
		if !errors.As(err, &local) || local.MayHaveExecuted {
			t.Fatalf("invalid control must be locally rejected: %v", err)
		}
	}
	if len(worker.controlOutbound) != 0 {
		t.Fatal("invalid control was sent")
	}
	finished := make(chan error, 1)
	go func() {
		_, err := svc.DispatchPersistenceControl(context.Background(), "worker", snapshotTestControl("delivery-1"))
		finished <- err
	}()
	select {
	case <-worker.controlOutbound:
	case <-time.After(time.Second):
		t.Fatal("control not enqueued")
	}
	disconnected := errors.New("worker disconnected")
	worker.close(disconnected)
	select {
	case err := <-finished:
		var uncertain *PersistenceControlError
		if !errors.Is(err, disconnected) || !errors.As(err, &uncertain) || !uncertain.MayHaveExecuted {
			t.Fatalf("disconnect lost uncertain outcome: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not finish delivery")
	}
}

func TestSnapshotHeartbeatCannotOmitOrForgePersistenceUsage(t *testing.T) {
	var enabled, legacy persistenceSession
	enabled.initialize(snapshotTestHello("worker").SandboxPersistence)
	legacy.initialize(nil)
	if enabled.validateUsage(nil) == nil {
		t.Fatal("enabled worker omitted usage")
	}
	if err := legacy.validateUsage(nil); err != nil {
		t.Fatalf("legacy heartbeat rejected: %v", err)
	}
	if legacy.validateUsage(&registryv1.SandboxPersistenceUsage{}) == nil {
		t.Fatal("legacy worker forged usage")
	}
	for _, usage := range []*registryv1.SandboxPersistenceUsage{{Inflight: -1}, {AvailableStagingBytes: -1}} {
		if enabled.validateUsage(usage) == nil {
			t.Fatal("accepted negative usage")
		}
	}
}

func TestSnapshotRestorePrefersOriginalNodeAndFallsBack(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	addSnapshotTestWorker(t, svc, snapshotTestHello("worker-a"))
	original := addSnapshotTestWorker(t, svc, snapshotTestHello("worker-b"))
	request := RestorePlacement{Spec: snapshotTestSpec(), StagingBytes: 1024, PreferredWorkerID: "worker-b"}
	first, err := svc.ReserveSnapshotRestore(request)
	if err != nil || first.WorkerID() != "worker-b" {
		t.Fatalf("original-node preference: %v %v", first, err)
	}
	defer first.Release()
	second, err := svc.ReserveSnapshotRestore(request)
	if err != nil || second.WorkerID() != "worker-a" {
		t.Fatalf("cross-node fallback: %v %v", second, err)
	}
	defer second.Release()
	if _, err := svc.ReserveSnapshotRestore(request); !errors.Is(err, ErrNoCapabilityWorker) {
		t.Fatalf("both staging budgets reserved: %v", err)
	}
	// Replacing a connection must not let a reservation send to the replacement.
	svc.swapSession(newActiveSession("worker-b", "new-connection", snapshotTestHello("worker-b")))
	_, err = first.Dispatch(context.Background(), snapshotTestControl("delivery-1"))
	var controlErr *PersistenceControlError
	if !errors.As(err, &controlErr) || controlErr.MayHaveExecuted {
		t.Fatalf("replaced reservation: %v", err)
	}
	if len(original.controlOutbound) != 0 {
		t.Fatal("sent a control using a replaced reservation")
	}
}

func TestSnapshotControlRoundTripAndHeartbeat(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"worker": "secret"}, 5, 15, time.Minute)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()
	stream, connectionID, err := connectWorkerWithHello(client, snapshotTestHello("worker"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseSend()
	r, err := svc.ReserveSnapshotRestore(RestorePlacement{Spec: snapshotTestSpec(), StagingBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	finished := make(chan error, 1)
	go func() {
		report, err := r.Dispatch(context.Background(), snapshotTestControl("delivery-1"))
		if err == nil && string(report.PayloadJson) != `{"reserved":true}` {
			err = errors.New("unexpected result payload")
		}
		finished <- err
	}()
	response, err := stream.Recv()
	if err != nil || response.GetSandboxPersistenceControl() == nil {
		t.Fatalf("receive control: %v %v", response, err)
	}
	control := response.GetSandboxPersistenceControl()
	progress := snapshotTestReport(control)
	progress.Completed = false
	progress.Phase = "preparing"
	if err := stream.Send(&registryv1.ConnectRequest{Payload: &registryv1.ConnectRequest_SandboxPersistenceReport{SandboxPersistenceReport: progress}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&registryv1.ConnectRequest{Payload: &registryv1.ConnectRequest_Heartbeat{Heartbeat: &registryv1.HeartbeatFrame{NodeId: "worker", SessionId: connectionID, ActiveSessionCount: 1, SandboxPersistence: &registryv1.SandboxPersistenceUsage{Inflight: 1, AvailableStagingBytes: 900}}}}); err != nil {
		t.Fatal(err)
	}
	if response, err := stream.Recv(); err != nil || response.GetHeartbeatAck() == nil {
		t.Fatalf("heartbeat blocked by lifecycle control: %v %v", response, err)
	}
	select {
	case err := <-finished:
		t.Fatalf("progress finished control: %v", err)
	default:
	}
	if err := stream.Send(&registryv1.ConnectRequest{Payload: &registryv1.ConnectRequest_SandboxPersistenceReport{SandboxPersistenceReport: snapshotTestReport(control)}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control did not finish")
	}
	p := &svc.getSession("worker").persistence
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.availableBytes != 900 || p.reportedInflight != 1 {
		t.Fatal("persistence heartbeat usage was not retained")
	}
}

func TestSnapshotControlTimeoutRetainsReservationAndIgnoresLateResult(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	worker := addSnapshotTestWorker(t, svc, snapshotTestHello("worker"))
	r, err := svc.ReserveSnapshotRestore(RestorePlacement{Spec: snapshotTestSpec(), StagingBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := r.Dispatch(ctx, snapshotTestControl("delivery-1")); finished <- err }()
	var control *registryv1.SandboxPersistenceControl
	select {
	case response := <-worker.controlOutbound:
		control = response.GetSandboxPersistenceControl()
	case <-time.After(time.Second):
		t.Fatal("control not sent")
	}
	cancel()
	select {
	case err = <-finished:
	case <-time.After(time.Second):
		t.Fatal("canceled control did not return")
	}
	var uncertain *PersistenceControlError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &uncertain) || !uncertain.MayHaveExecuted {
		t.Fatalf("lost uncertain-outcome classification: %v", err)
	}
	worker.persistence.mu.Lock()
	if worker.persistence.reservations != 1 || worker.persistence.reservedBytes != 100 || len(worker.persistence.pending) != 0 {
		t.Errorf("timeout released backend reservation or leaked delivery")
	}
	worker.persistence.mu.Unlock()
	if err := worker.persistence.resolve(snapshotTestReport(control)); err != nil {
		t.Fatalf("late report: %v", err)
	}
}

func TestSnapshotControlReportFencingAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*registryv1.SandboxPersistenceReport)
	}{
		{"operation", func(r *registryv1.SandboxPersistenceReport) { r.OperationId = "different" }},
		{"scope", func(r *registryv1.SandboxPersistenceReport) {
			r.ScopedSessionId = scopeTerminalSessionID("other-owner", "session-1")
		}},
		{"generation", func(r *registryv1.SandboxPersistenceReport) { r.Generation-- }},
		{"action", func(r *registryv1.SandboxPersistenceReport) { r.Action = registryv1.SandboxPersistenceControl_ACTIVATE }},
		{"payload too large", func(r *registryv1.SandboxPersistenceReport) {
			r.PayloadJson = []byte(`"` + strings.Repeat("a", maxPersistencePayload) + `"`)
		}},
		{"invalid JSON", func(r *registryv1.SandboxPersistenceReport) { r.PayloadJson = []byte(`{`) }},
		{"empty final result", func(r *registryv1.SandboxPersistenceReport) { r.PayloadJson = nil }},
		{"error and result", func(r *registryv1.SandboxPersistenceReport) { r.Error = &registryv1.CommandError{Code: "failed"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p persistenceSession
			p.initialize(snapshotTestHello("worker").SandboxPersistence)
			control := snapshotTestControl("delivery-1")
			result, err := p.register(control)
			if err != nil {
				t.Fatal(err)
			}
			report := snapshotTestReport(control)
			tc.edit(report)
			if err := p.resolve(report); err == nil {
				t.Fatal("accepted invalid/mismatched report")
			}
			select {
			case <-result:
				t.Fatal("invalid report resolved delivery")
			default:
			}
			if err := p.resolve(snapshotTestReport(control)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotPersistenceHelloValidation(t *testing.T) {
	for _, edit := range []func(*registryv1.ConnectHello){
		func(h *registryv1.ConnectHello) { h.SandboxPersistence.MaxInflight = 0 },
		func(h *registryv1.ConnectHello) { h.SandboxPersistence.MaxInflight = 65 },
		func(h *registryv1.ConnectHello) { h.SandboxPersistence.AvailableStagingBytes = -1 },
		func(h *registryv1.ConnectHello) { h.TerminalSessionCapacity = nil },
		func(h *registryv1.ConnectHello) { h.ExecutorKind = "e2b" },
		func(h *registryv1.ConnectHello) { h.SandboxPersistence.GuestPlatform = "darwin/arm64" },
		func(h *registryv1.ConnectHello) { h.SandboxPersistence.ReadFormats = []string{"boxlite-archive-v1"} },
		func(h *registryv1.ConnectHello) {
			h.SandboxPersistence.WriteFormats = []string{"docker-rootfs-v1", "docker-rootfs-v1"}
		},
	} {
		hello := snapshotTestHello("worker")
		edit(hello)
		if validateHello(hello) == nil {
			t.Fatal("accepted invalid enabled declaration")
		}
	}
	hello := snapshotTestHello("worker")
	clone := proto.Clone(hello).(*registryv1.ConnectHello)
	session := newActiveSession("worker", "connection", hello)
	hello.SandboxPersistence.RuntimeVersion = "changed"
	if session.persistence.capabilities.RuntimeVersion != clone.SandboxPersistence.RuntimeVersion {
		t.Fatal("session retained mutable caller declaration")
	}
}
