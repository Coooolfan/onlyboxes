package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

type staticInflightProvider struct {
	snapshots []grpcserver.WorkerInflightSnapshot
}

func (p *staticInflightProvider) InflightStats() []grpcserver.WorkerInflightSnapshot {
	if p == nil {
		return nil
	}
	return p.snapshots
}

func TestWorkerStatsAggregatesAllWorkers(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_500, 0)

	for i := 0; i < 120; i++ {
		store.Upsert(&registryv1.ConnectHello{NodeId: fmt.Sprintf("online-%d", i)}, fmt.Sprintf("session-online-%d", i), now.Add(-5*time.Second))
	}
	for i := 0; i < 20; i++ {
		store.Upsert(&registryv1.ConnectHello{NodeId: fmt.Sprintf("offline-not-stale-%d", i)}, fmt.Sprintf("session-offline-a-%d", i), now.Add(-20*time.Second))
	}
	for i := 0; i < 10; i++ {
		store.Upsert(&registryv1.ConnectHello{NodeId: fmt.Sprintf("offline-stale-%d", i)}, fmt.Sprintf("session-offline-b-%d", i), now.Add(-40*time.Second))
	}

	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	handler.nowFn = func() time.Time {
		return now
	}
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}

	var payload workerStatsResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload.Total != 150 {
		t.Fatalf("expected total=150, got %d", payload.Total)
	}
	if payload.Online != 120 {
		t.Fatalf("expected online=120, got %d", payload.Online)
	}
	if payload.Offline != 30 {
		t.Fatalf("expected offline=30, got %d", payload.Offline)
	}
	if payload.Stale != 10 {
		t.Fatalf("expected stale=10, got %d", payload.Stale)
	}
	if payload.StaleAfterSec != defaultStaleAfterSec {
		t.Fatalf("expected stale_after_sec=%d, got %d", defaultStaleAfterSec, payload.StaleAfterSec)
	}
}

func TestWorkerStatsSupportsCustomStaleThreshold(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_600, 0)
	store.Upsert(&registryv1.ConnectHello{NodeId: "fresh"}, "session-fresh", now.Add(-5*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "old-a"}, "session-old-a", now.Add(-20*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "old-b"}, "session-old-b", now.Add(-40*time.Second))

	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	handler.nowFn = func() time.Time {
		return now
	}
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats?stale_after_sec=10", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}

	var payload workerStatsResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload.Stale != 2 {
		t.Fatalf("expected stale=2 for stale_after_sec=10, got %d", payload.Stale)
	}
	if payload.StaleAfterSec != 10 {
		t.Fatalf("expected stale_after_sec=10, got %d", payload.StaleAfterSec)
	}
}

func TestWorkerStatsRejectsInvalidStaleThreshold(t *testing.T) {
	store := registrytest.NewStore(t)
	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats?stale_after_sec=0", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.Code)
	}
}

func TestWorkerStatsAndInflightAreOwnerScopedForNonAdmin(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_700, 0)
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-own-sys",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-member-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
		},
	}, "session-own-sys", now.Add(-5*time.Second)); err != nil {
		t.Fatalf("seed own sys worker: %v", err)
	}
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-other-sys",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-other-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
		},
	}, "session-other-sys", now.Add(-5*time.Second)); err != nil {
		t.Fatalf("seed other sys worker: %v", err)
	}

	consoleAuth := newTestConsoleAuth(t)
	seedTestAccount(t, consoleAuth.queries, "acc-member-1", "member-test", "member-password", false)
	handler := NewWorkerHandler(
		store,
		15*time.Second,
		nil,
		nil,
		&staticInflightProvider{
			snapshots: []grpcserver.WorkerInflightSnapshot{
				{
					NodeID:             "node-own-sys",
					ActiveSessionCount: 1,
					Capabilities: []grpcserver.CapabilityInflightEntry{
						{Name: "computeruse", Inflight: 1, MaxInflight: 1},
					},
				},
				{
					NodeID:             "node-other-sys",
					ActiveSessionCount: 1,
					Capabilities: []grpcserver.CapabilityInflightEntry{
						{Name: "computeruse", Inflight: 0, MaxInflight: 1},
					},
				},
			},
		},
		"",
	)
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, consoleAuth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookieFor(t, router, "member-test", "member-password")

	statsReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats", nil)
	statsReq.AddCookie(cookie)
	statsRes := httptest.NewRecorder()
	router.ServeHTTP(statsRes, statsReq)
	if statsRes.Code != http.StatusOK {
		t.Fatalf("expected stats 200, got %d body=%s", statsRes.Code, statsRes.Body.String())
	}
	statsPayload := workerStatsResponse{}
	if err := json.Unmarshal(statsRes.Body.Bytes(), &statsPayload); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if statsPayload.Total != 1 {
		t.Fatalf("expected scoped total=1, got %d", statsPayload.Total)
	}

	inflightReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers/inflight", nil)
	inflightReq.AddCookie(cookie)
	inflightRes := httptest.NewRecorder()
	router.ServeHTTP(inflightRes, inflightReq)
	if inflightRes.Code != http.StatusOK {
		t.Fatalf("expected inflight 200, got %d body=%s", inflightRes.Code, inflightRes.Body.String())
	}
	inflightPayload := workerInflightResponse{}
	if err := json.Unmarshal(inflightRes.Body.Bytes(), &inflightPayload); err != nil {
		t.Fatalf("decode inflight response: %v", err)
	}
	if len(inflightPayload.Workers) != 1 || inflightPayload.Workers[0].NodeID != "node-own-sys" {
		t.Fatalf("expected scoped inflight worker list, got %#v", inflightPayload.Workers)
	}
	if inflightPayload.Workers[0].ActiveSessionCount != 1 {
		t.Fatalf("expected active_session_count=1, got %d", inflightPayload.Workers[0].ActiveSessionCount)
	}
}
