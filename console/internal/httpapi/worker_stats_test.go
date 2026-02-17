package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

func TestWorkerStatsAggregatesAllWorkers(t *testing.T) {
	store := registry.NewStore()
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

	handler := NewWorkerHandler(store, 15*time.Second)
	handler.nowFn = func() time.Time {
		return now
	}
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats", nil)
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
	store := registry.NewStore()
	now := time.Unix(1_700_000_600, 0)
	store.Upsert(&registryv1.ConnectHello{NodeId: "fresh"}, "session-fresh", now.Add(-5*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "old-a"}, "session-old-a", now.Add(-20*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "old-b"}, "session-old-b", now.Add(-40*time.Second))

	handler := NewWorkerHandler(store, 15*time.Second)
	handler.nowFn = func() time.Time {
		return now
	}
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats?stale_after_sec=10", nil)
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
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second)
	router := NewRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/stats?stale_after_sec=0", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.Code)
	}
}
