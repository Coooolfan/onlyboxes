package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

func TestListWorkersEmpty(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, nil)
	handler.nowFn = func() time.Time {
		return time.Unix(1_700_000_000, 0)
	}
	router := NewRouter(handler, newTestConsoleAuth(t))
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}

	var payload listWorkersResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload.Total != 0 || len(payload.Items) != 0 {
		t.Fatalf("expected empty result, got total=%d len=%d", payload.Total, len(payload.Items))
	}
	if payload.Page != 1 || payload.PageSize != 20 {
		t.Fatalf("expected default pagination, got page=%d page_size=%d", payload.Page, payload.PageSize)
	}
}

func TestListWorkersPaginationAndFilter(t *testing.T) {
	store := registry.NewStore()
	base := time.Unix(1_700_000_100, 0)

	store.Upsert(&registryv1.ConnectHello{NodeId: "node-2", NodeName: "node-2"}, "session-2", base)
	store.Upsert(&registryv1.ConnectHello{NodeId: "node-1", NodeName: "node-1"}, "session-1", base.Add(10*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "node-3", NodeName: "node-3"}, "session-3", base.Add(12*time.Second))

	handler := NewWorkerHandler(store, 15*time.Second, nil)
	handler.nowFn = func() time.Time {
		return base.Add(20 * time.Second)
	}
	router := NewRouter(handler, newTestConsoleAuth(t))
	cookie := loginSessionCookie(t, router)

	resPage := httptest.NewRecorder()
	reqPage := httptest.NewRequest(http.MethodGet, "/api/v1/workers?page=2&page_size=1&status=all", nil)
	reqPage.AddCookie(cookie)
	router.ServeHTTP(resPage, reqPage)
	if resPage.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resPage.Code)
	}
	var pagePayload listWorkersResponse
	if err := json.Unmarshal(resPage.Body.Bytes(), &pagePayload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if pagePayload.Total != 3 || len(pagePayload.Items) != 1 {
		t.Fatalf("expected total=3, one item in page 2, got total=%d len=%d", pagePayload.Total, len(pagePayload.Items))
	}
	if pagePayload.Items[0].NodeID != "node-1" {
		t.Fatalf("expected second registration order item node-1, got %s", pagePayload.Items[0].NodeID)
	}

	resOffline := httptest.NewRecorder()
	reqOffline := httptest.NewRequest(http.MethodGet, "/api/v1/workers?status=offline", nil)
	reqOffline.AddCookie(cookie)
	router.ServeHTTP(resOffline, reqOffline)
	if resOffline.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resOffline.Code)
	}
	var offlinePayload listWorkersResponse
	if err := json.Unmarshal(resOffline.Body.Bytes(), &offlinePayload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if offlinePayload.Total != 1 || len(offlinePayload.Items) != 1 {
		t.Fatalf("expected exactly one offline worker, got total=%d len=%d", offlinePayload.Total, len(offlinePayload.Items))
	}
	if offlinePayload.Items[0].NodeID != "node-2" {
		t.Fatalf("expected node-2 to be offline, got %s", offlinePayload.Items[0].NodeID)
	}
}

func TestListWorkersRequiresAuthentication(t *testing.T) {
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, nil)
	router := NewRouter(handler, newTestConsoleAuth(t))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}
