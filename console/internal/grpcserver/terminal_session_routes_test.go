package grpcserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

// fakeTerminalSessionRouteStore wraps a real store and allows injecting
// failures for specific operations. It implements terminalSessionRouteStore.
type fakeTerminalSessionRouteStore struct {
	mu                 sync.Mutex
	inner              *registry.Store
	upsertErr          error
	deleteErr          error
	deleteByNodeErr    error
	deleteExpiredErr   error
	loadErr            error
	upsertCallCount    int
	deleteCallCount    int
	deleteByNodeCount  int
	deleteExpiredCount int
	loadCallCount      int
	lastUpsertRoute    registry.TerminalSessionRoute
	lastDeleteRefs     []registry.TerminalSessionRouteRef
	lastDeleteNodeID   string
}

type recordingProxyRouteSessionRevoker struct {
	mu         sync.Mutex
	sessionIDs []string
}

func (r *recordingProxyRouteSessionRevoker) RevokeSessionRoutes(sessionIDs ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionIDs = append(r.sessionIDs, sessionIDs...)
	return len(sessionIDs)
}

func (f *fakeTerminalSessionRouteStore) LoadActiveTerminalSessionRoutes(ctx context.Context, nowUnixMs int64) ([]registry.TerminalSessionRoute, error) {
	f.mu.Lock()
	f.loadCallCount++
	err := f.loadErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.inner.LoadActiveTerminalSessionRoutes(ctx, nowUnixMs)
}

func (f *fakeTerminalSessionRouteStore) UpsertConfirmedTerminalSessionRoute(ctx context.Context, route registry.TerminalSessionRoute) error {
	f.mu.Lock()
	f.upsertCallCount++
	f.lastUpsertRoute = route
	err := f.upsertErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.inner.UpsertConfirmedTerminalSessionRoute(ctx, route)
}

func (f *fakeTerminalSessionRouteStore) DeleteTerminalSessionRoute(ctx context.Context, scopedSessionID string, expectedNodeID string) (bool, error) {
	f.mu.Lock()
	f.deleteCallCount++
	err := f.deleteErr
	f.mu.Unlock()
	if err != nil {
		return false, err
	}
	return f.inner.DeleteTerminalSessionRoute(ctx, scopedSessionID, expectedNodeID)
}

func (f *fakeTerminalSessionRouteStore) DeleteTerminalSessionRoutes(ctx context.Context, routes []registry.TerminalSessionRouteRef) error {
	f.mu.Lock()
	f.deleteCallCount++
	f.lastDeleteRefs = routes
	err := f.deleteErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.inner.DeleteTerminalSessionRoutes(ctx, routes)
}

func (f *fakeTerminalSessionRouteStore) DeleteTerminalSessionRoutesByNode(ctx context.Context, nodeID string) (int64, error) {
	f.mu.Lock()
	f.deleteByNodeCount++
	f.lastDeleteNodeID = nodeID
	err := f.deleteByNodeErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return f.inner.DeleteTerminalSessionRoutesByNode(ctx, nodeID)
}

func (f *fakeTerminalSessionRouteStore) DeleteExpiredTerminalSessionRoutes(ctx context.Context, nowUnixMs int64) (int64, error) {
	f.mu.Lock()
	f.deleteExpiredCount++
	err := f.deleteExpiredErr
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return f.inner.DeleteExpiredTerminalSessionRoutes(ctx, nowUnixMs)
}

func (f *fakeTerminalSessionRouteStore) setUpsertErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upsertErr = err
}

func (f *fakeTerminalSessionRouteStore) setDeleteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

func (f *fakeTerminalSessionRouteStore) setUpsertCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upsertCallCount
}

func (f *fakeTerminalSessionRouteStore) setDeleteCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteCallCount
}

func newFakeRouteStore(t testing.TB) *fakeTerminalSessionRouteStore {
	t.Helper()
	return &fakeTerminalSessionRouteStore{
		inner: registrytest.NewStore(t),
	}
}

// ---- Persistence fault tests ----

func TestCommitConfirmedRouteFailsOnStoreError(t *testing.T) {
	store := newFakeRouteStore(t)
	svc := NewRegistryService(store.inner, nil, 5, 15, time.Minute)
	svc.terminalRouteStore = store
	base := time.Unix(1_700_700_000, 0)
	svc.nowFn = func() time.Time { return base }

	nodeID, reservationID := svc.reserveTerminalSessionRoute("session-fault", "node-a", base)
	if reservationID == 0 {
		t.Fatal("expected non-zero reservation ID")
	}

	store.setUpsertErr(errors.New("disk full"))
	confirmed, err := svc.commitConfirmedTerminalSessionRoute(
		"session-fault",
		nodeID,
		reservationID,
		base.Add(10*time.Minute).UnixMilli(),
		base,
	)
	if err == nil {
		t.Fatal("expected persistence error from commit")
	}
	if confirmed {
		t.Fatal("route must not be confirmed when persistence fails")
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-fault"]
	svc.terminalRoutesMu.RUnlock()
	if route.ReservationID == 0 {
		t.Fatal("reservation must not be cleared when persistence fails")
	}
	if route.ConfirmedReservationID != 0 {
		t.Fatal("route must not be marked confirmed when persistence fails")
	}
	if route.LeaseExpiresUnixMs != 0 {
		t.Fatal("lease must not be set when persistence fails")
	}
}

func TestRecoveryReportFailsOnStoreError(t *testing.T) {
	store := newFakeRouteStore(t)
	svc := NewRegistryService(store.inner, nil, 5, 15, time.Minute)
	svc.terminalRouteStore = store
	base := time.Unix(1_700_710_000, 0)
	svc.nowFn = func() time.Time { return base }

	svc.bindTerminalSessionRoute("session-recovery-fault", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-recovery-fault", "node-a", base.Add(time.Minute).UnixMilli(), base)
	candidates := svc.beginTerminalSessionRecovery("node-a", base)
	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, base)
	session.setRecoveryCandidates(candidates)

	store.setDeleteErr(errors.New("disk full"))
	err := svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{
		Results: []*registryv1.TerminalSessionRecoveryResult{
			{SessionId: "session-recovery-fault", Status: registryv1.TerminalSessionRecoveryResult_MISSING},
		},
	}, base.Add(time.Second))
	if err == nil {
		t.Fatal("expected persistence error from recovery report")
	}

	svc.terminalRoutesMu.RLock()
	_, stillExists := svc.terminalSessionToNode["session-recovery-fault"]
	svc.terminalRoutesMu.RUnlock()
	if !stillExists {
		t.Fatal("in-memory route must not be deleted when persistence fails")
	}
}

// ---- ABA tests ----

func TestLateCommandResultDoesNotConfirmReassignedRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_720_000, 0)
	svc.nowFn = func() time.Time { return base }

	nodeID, reservationID := svc.reserveTerminalSessionRoute("session-aba", "node-a", base)
	if reservationID == 0 {
		t.Fatal("expected non-zero reservation ID")
	}

	// Simulate the route being reassigned to a different worker before the
	// late result arrives.
	svc.bindTerminalSessionRoute("session-aba", "node-b", base.Add(time.Second))

	confirmed, err := svc.commitConfirmedTerminalSessionRoute(
		"session-aba",
		nodeID,
		reservationID,
		base.Add(10*time.Minute).UnixMilli(),
		base.Add(2*time.Second),
	)
	if err != nil {
		t.Fatalf("commit should not error on ABA mismatch: %v", err)
	}
	if confirmed {
		t.Fatal("late result must not confirm a route reassigned to another worker")
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-aba"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != "node-b" {
		t.Fatalf("route should still belong to node-b, got %s", route.NodeID)
	}
}

func TestStaleReservationClearDoesNotDeleteConfirmedRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_730_000, 0)
	svc.nowFn = func() time.Time { return base }

	nodeID, reservationID := svc.reserveTerminalSessionRoute("session-stale", "node-a", base)
	if reservationID == 0 {
		t.Fatal("expected non-zero reservation ID")
	}

	// Confirm the route via durable commit.
	confirmed, err := svc.commitConfirmedTerminalSessionRoute(
		"session-stale",
		nodeID,
		reservationID,
		base.Add(10*time.Minute).UnixMilli(),
		base,
	)
	if err != nil || !confirmed {
		t.Fatalf("durable commit failed: confirmed=%v err=%v", confirmed, err)
	}

	// A late dispatch sharing the old reservation tries to roll back.
	result := svc.clearTerminalSessionRouteReservation("session-stale", nodeID, reservationID)
	if result != routeReservationNotOwned {
		t.Fatalf("stale reservation clear should be not-owned, got %v", result)
	}

	svc.terminalRoutesMu.RLock()
	_, ok := svc.terminalSessionToNode["session-stale"]
	svc.terminalRoutesMu.RUnlock()
	if !ok {
		t.Fatal("confirmed route must not be deleted by stale reservation clear")
	}
}

func TestLateRecoveryReportDoesNotDeleteReassignedRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_740_000, 0)
	svc.nowFn = func() time.Time { return base }

	svc.bindTerminalSessionRoute("session-late-recovery", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-late-recovery", "node-a", base.Add(time.Minute).UnixMilli(), base)
	candidates := svc.beginTerminalSessionRecovery("node-a", base)
	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, base)
	session.setRecoveryCandidates(candidates)

	// Route gets reassigned to node-b while node-a's recovery is in flight.
	svc.bindTerminalSessionRoute("session-late-recovery", "node-b", base.Add(time.Second))

	err := svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{
		Results: []*registryv1.TerminalSessionRecoveryResult{
			{SessionId: "session-late-recovery", Status: registryv1.TerminalSessionRecoveryResult_MISSING},
		},
	}, base.Add(2*time.Second))
	if err == nil {
		t.Fatal("late recovery report should be rejected for reassigned route")
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-late-recovery"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != "node-b" {
		t.Fatalf("reassigned route should still belong to node-b, got %s", route.NodeID)
	}
}

func TestSessionNotFoundDeletesOnlyMatchingNode(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_750_000, 0)
	svc.nowFn = func() time.Time { return base }

	svc.bindTerminalSessionRoute("session-node-a", "node-a", base)
	svc.updateTerminalSessionRouteLease("session-node-a", "node-a", base.Add(time.Minute).UnixMilli(), base)

	// A late session_not_found from a different node must not delete the route.
	if err := svc.clearTerminalSessionRoute("session-node-a", "node-b"); err != nil {
		t.Fatalf("clear from wrong node should be noop: %v", err)
	}
	svc.terminalRoutesMu.RLock()
	_, ok := svc.terminalSessionToNode["session-node-a"]
	svc.terminalRoutesMu.RUnlock()
	if !ok {
		t.Fatal("route must not be deleted by a different node's session_not_found")
	}

	// The correct node's session_not_found should delete it.
	if err := svc.clearTerminalSessionRoute("session-node-a", "node-a"); err != nil {
		t.Fatalf("clear from correct node failed: %v", err)
	}
	svc.terminalRoutesMu.RLock()
	_, ok = svc.terminalSessionToNode["session-node-a"]
	svc.terminalRoutesMu.RUnlock()
	if ok {
		t.Fatal("route must be deleted by the correct node's session_not_found")
	}
}

// ---- Console restart tests ----

func TestRestoreTerminalSessionRoutesLoadsActiveRoutes(t *testing.T) {
	store := registrytest.NewStore(t)
	base := time.Unix(1_700_800_000, 0)
	lease := base.Add(30 * time.Minute).UnixMilli()

	// First service: persist a confirmed route.
	svc1 := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc1.nowFn = func() time.Time { return base }
	ctx := context.Background()
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:owner-a:session-restart",
		NodeID:             "node-a",
		LeaseExpiresUnixMs: lease,
		LastUsedUnixMs:     base.UnixMilli(),
		CreatedAtUnixMs:    base.UnixMilli(),
		UpdatedAtUnixMs:    base.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist route: %v", err)
	}

	// Second service: simulate restart.
	svc2 := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc2.nowFn = func() time.Time { return base.Add(time.Second) }
	if err := svc2.RestoreTerminalSessionRoutes(ctx, base.Add(time.Second)); err != nil {
		t.Fatalf("restore routes: %v", err)
	}

	svc2.terminalRoutesMu.RLock()
	route, ok := svc2.terminalSessionToNode["obx:owner-a:session-restart"]
	svc2.terminalRoutesMu.RUnlock()
	if !ok {
		t.Fatal("restored route not found in memory")
	}
	if route.NodeID != "node-a" {
		t.Fatalf("restored route node=%s, want node-a", route.NodeID)
	}
	if route.RecoveryState != terminalSessionRecoveryUnavailable {
		t.Fatalf("restored route recovery state=%v, want unavailable", route.RecoveryState)
	}
	if route.ReservationID != 0 {
		t.Fatal("restored route must not carry a reservation")
	}
	if route.LeaseExpiresUnixMs != lease {
		t.Fatalf("restored route lease=%d, want %d", route.LeaseExpiresUnixMs, lease)
	}
}

func TestRestoreTerminalSessionRoutesDeletesExpiredRoutes(t *testing.T) {
	store := registrytest.NewStore(t)
	base := time.Unix(1_700_810_000, 0)
	ctx := context.Background()

	// Persist an already-expired route.
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:owner-a:session-expired",
		NodeID:             "node-a",
		LeaseExpiresUnixMs: base.Add(-time.Minute).UnixMilli(),
		LastUsedUnixMs:     base.Add(-2 * time.Minute).UnixMilli(),
		CreatedAtUnixMs:    base.Add(-2 * time.Minute).UnixMilli(),
		UpdatedAtUnixMs:    base.Add(-2 * time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("persist expired route: %v", err)
	}

	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return base }
	if err := svc.RestoreTerminalSessionRoutes(ctx, base); err != nil {
		t.Fatalf("restore routes: %v", err)
	}

	svc.terminalRoutesMu.RLock()
	_, ok := svc.terminalSessionToNode["obx:owner-a:session-expired"]
	svc.terminalRoutesMu.RUnlock()
	if ok {
		t.Fatal("expired route must not be loaded into memory")
	}

	// Verify it was deleted from the database.
	routes, err := store.LoadActiveTerminalSessionRoutes(ctx, base.UnixMilli())
	if err != nil {
		t.Fatalf("load active routes: %v", err)
	}
	for _, r := range routes {
		if r.ScopedSessionID == "obx:owner-a:session-expired" {
			t.Fatal("expired route must be deleted from database")
		}
	}
}

func TestRestoreTerminalSessionRoutesRejectsAlreadyInitialized(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, time.Minute)
	base := time.Unix(1_700_820_000, 0)
	svc.nowFn = func() time.Time { return base }

	// Manually populate the in-memory map to simulate already-initialized state.
	svc.terminalRoutesMu.Lock()
	svc.terminalSessionToNode["session-existing"] = terminalSessionRoute{NodeID: "node-a"}
	svc.terminalRoutesMu.Unlock()

	if err := svc.RestoreTerminalSessionRoutes(context.Background(), base); err == nil {
		t.Fatal("restore should fail when routes are already initialized")
	}
}

func TestConsoleRestartRecoveryRoundTrip(t *testing.T) {
	store := registrytest.NewStore(t)
	base := time.Unix(1_700_830_000, 0)
	lease := base.Add(30 * time.Minute).UnixMilli()
	ctx := context.Background()

	// First service: persist a confirmed route, then simulate crash.
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:owner-a:session-restart-rt",
		NodeID:             "node-a",
		LeaseExpiresUnixMs: lease,
		LastUsedUnixMs:     base.UnixMilli(),
		CreatedAtUnixMs:    base.UnixMilli(),
		UpdatedAtUnixMs:    base.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist route: %v", err)
	}

	// Second service: restart and restore.
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	restartTime := base.Add(5 * time.Second)
	svc.nowFn = func() time.Time { return restartTime }
	if err := svc.RestoreTerminalSessionRoutes(ctx, restartTime); err != nil {
		t.Fatalf("restore routes: %v", err)
	}

	// Route should be unavailable until worker reconnects and recovers.
	_, _, err := svc.pickSessionForDispatch(
		taskCapabilityTerminalExec,
		"owner-a",
		"obx:owner-a:session-restart-rt",
		sessionPickOptions{terminalSessionIntent: terminalSessionIntentKnownNew},
	)
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) || commandErr.Code != terminalSessionUnavailableCode {
		t.Fatalf("expected session_unavailable after restart, got %v", err)
	}

	// Worker reconnects and recovery begins.
	candidates := svc.beginTerminalSessionRecovery("node-a", restartTime)
	if len(candidates) != 1 || candidates[0].GetSessionId() != "obx:owner-a:session-restart-rt" {
		t.Fatalf("unexpected recovery candidates: %#v", candidates)
	}
	if candidates[0].GetLeaseExpiresUnixMs() != lease {
		t.Fatalf("recovery candidate lease=%d, want %d", candidates[0].GetLeaseExpiresUnixMs(), lease)
	}

	session := newActiveSessionAt("node-a", "worker-session-a", &registryv1.ConnectHello{
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec, MaxInflight: 1}},
	}, restartTime)
	session.setRecoveryCandidates(candidates)

	// Worker reports successful recovery.
	err = svc.applyTerminalSessionRecoveryReport(session, &registryv1.TerminalSessionRecoveryReport{
		Results: []*registryv1.TerminalSessionRecoveryResult{
			{SessionId: "obx:owner-a:session-restart-rt", Status: registryv1.TerminalSessionRecoveryResult_RECOVERED},
		},
	}, restartTime.Add(time.Second))
	if err != nil {
		t.Fatalf("apply recovery report: %v", err)
	}

	// Route should now be ready.
	route, ok := svc.terminalSessionRouteSnapshot("obx:owner-a:session-restart-rt", restartTime.Add(time.Second))
	if !ok || route.RecoveryState != terminalSessionRecoveryReady {
		t.Fatalf("route should be ready after recovery: %#v ok=%v", route, ok)
	}
	if route.LeaseExpiresUnixMs != lease {
		t.Fatalf("recovered route lease=%d, want %d", route.LeaseExpiresUnixMs, lease)
	}
}

func TestConsoleRestartExpiredLeaseNotOfferedForRecovery(t *testing.T) {
	store := registrytest.NewStore(t)
	base := time.Unix(1_700_840_000, 0)
	ctx := context.Background()

	// Persist a route whose lease expires during the console offline period.
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:owner-a:session-offline-expired",
		NodeID:             "node-a",
		LeaseExpiresUnixMs: base.Add(time.Minute).UnixMilli(),
		LastUsedUnixMs:     base.UnixMilli(),
		CreatedAtUnixMs:    base.UnixMilli(),
		UpdatedAtUnixMs:    base.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist route: %v", err)
	}

	// Restart after the lease has expired.
	restartTime := base.Add(2 * time.Minute)
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return restartTime }
	if err := svc.RestoreTerminalSessionRoutes(ctx, restartTime); err != nil {
		t.Fatalf("restore routes: %v", err)
	}

	// No candidates should be offered.
	candidates := svc.beginTerminalSessionRecovery("node-a", restartTime)
	if len(candidates) != 0 {
		t.Fatalf("expired route should not be offered for recovery: %#v", candidates)
	}
}

func TestDeleteProvisionedWorkerRemovesPersistedRoutes(t *testing.T) {
	store := registrytest.NewStore(t)
	base := time.Unix(1_700_850_000, 0)
	ctx := context.Background()

	// Persist a route for the worker that will be deleted.
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:owner-a:session-delete-worker",
		NodeID:             "node-delete",
		LeaseExpiresUnixMs: base.Add(30 * time.Minute).UnixMilli(),
		LastUsedUnixMs:     base.UnixMilli(),
		CreatedAtUnixMs:    base.UnixMilli(),
		UpdatedAtUnixMs:    base.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist route: %v", err)
	}

	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	svc.nowFn = func() time.Time { return base }

	// Manually bind the route in memory so deleteTerminalSessionRoutesByNode has something to remove.
	svc.bindTerminalSessionRoute("obx:owner-a:session-delete-worker", "node-delete", base)
	svc.updateTerminalSessionRouteLease("obx:owner-a:session-delete-worker", "node-delete", base.Add(30*time.Minute).UnixMilli(), base)

	removed, err := svc.deleteTerminalSessionRoutesByNode("node-delete")
	if err != nil {
		t.Fatalf("delete routes by node: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}

	// Verify the route is gone from the database.
	routes, err := store.LoadActiveTerminalSessionRoutes(ctx, base.UnixMilli())
	if err != nil {
		t.Fatalf("load active routes: %v", err)
	}
	for _, r := range routes {
		if r.NodeID == "node-delete" {
			t.Fatal("persisted route should be deleted when worker is deleted")
		}
	}

	// Verify the route is gone from memory.
	svc.terminalRoutesMu.RLock()
	_, ok := svc.terminalSessionToNode["obx:owner-a:session-delete-worker"]
	svc.terminalRoutesMu.RUnlock()
	if ok {
		t.Fatal("in-memory route should be deleted when worker is deleted")
	}
}

func TestPruneExpiredTerminalSessionRoutesDeletesFromPersistence(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := NewRegistryService(store, nil, 5, 15, time.Minute)
	revoker := &recordingProxyRouteSessionRevoker{}
	svc.SetProxyRouteSessionRevoker(revoker)
	base := time.Unix(1_700_860_000, 0)
	svc.nowFn = func() time.Time { return base }
	ctx := context.Background()

	// Persist a route with a short lease.
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:owner-a:session-prune",
		NodeID:             "node-a",
		LeaseExpiresUnixMs: base.Add(time.Minute).UnixMilli(),
		LastUsedUnixMs:     base.UnixMilli(),
		CreatedAtUnixMs:    base.UnixMilli(),
		UpdatedAtUnixMs:    base.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist route: %v", err)
	}

	// Load it into memory via restore.
	if err := svc.RestoreTerminalSessionRoutes(ctx, base); err != nil {
		t.Fatalf("restore routes: %v", err)
	}

	// Prune after the lease has expired.
	removed := svc.PruneExpiredTerminalSessionRoutes(base.Add(2 * time.Minute))
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}

	// Verify it's gone from the database.
	routes, err := store.LoadActiveTerminalSessionRoutes(ctx, base.Add(2*time.Minute).UnixMilli())
	if err != nil {
		t.Fatalf("load active routes: %v", err)
	}
	for _, r := range routes {
		if r.ScopedSessionID == "obx:owner-a:session-prune" {
			t.Fatal("expired route should be pruned from database")
		}
	}
	revoker.mu.Lock()
	defer revoker.mu.Unlock()
	if len(revoker.sessionIDs) != 1 || revoker.sessionIDs[0] != "obx:owner-a:session-prune" {
		t.Fatalf("unexpected revoked proxy route sessions: %#v", revoker.sessionIDs)
	}
}
