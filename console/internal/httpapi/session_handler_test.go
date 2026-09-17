package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func TestListSessionsRequiresDashboardAuth(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	unauth := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	unauthRec := httptest.NewRecorder()
	router.ServeHTTP(unauthRec, unauth)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", unauthRec.Code, unauthRec.Body.String())
	}

	execReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	setMCPTokenHeader(execReq)
	execRec := httptest.NewRecorder()
	router.ServeHTTP(execRec, execReq)
	if execRec.Code != http.StatusUnauthorized {
		t.Fatalf("execution token expected 401, got %d body=%s", execRec.Code, execRec.Body.String())
	}

	legacy := httptest.NewRequest(http.MethodGet, "/api/v1/console/sessions", nil)
	legacyRec := httptest.NewRecorder()
	router.ServeHTTP(legacyRec, legacy)
	if legacyRec.Code != http.StatusNotFound {
		t.Fatalf("legacy console path expected 404, got %d body=%s", legacyRec.Code, legacyRec.Body.String())
	}
}

func TestListSessionsEmpty(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := grpcserver.NewRegistryService(store, nil, 5, 15, time.Minute)
	handler := NewWorkerHandler(store, 15*time.Second, svc, svc, svc, "")
	handler.SetTerminalSessionRegistry(svc)
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body listSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Total != 0 || len(body.Items) != 0 || body.Page != 1 || body.PageSize != defaultSessionPageSize {
		t.Fatalf("unexpected empty list: %#v", body)
	}
}

func TestAdminSessionHTTPListsAllAndRequiresAccountIDForItem(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_701_000_000, 0).UTC()
	ctx := context.Background()
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:" + testDashboardAccountID + ":sess-owned",
		NodeID:             "node-owned",
		LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(),
		LastUsedUnixMs:     now.UnixMilli(),
		CreatedAtUnixMs:    now.UnixMilli(),
		UpdatedAtUnixMs:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist owned route: %v", err)
	}
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:acc-other:sess-other",
		NodeID:             "node-other",
		LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(),
		LastUsedUnixMs:     now.UnixMilli(),
		CreatedAtUnixMs:    now.Add(time.Minute).UnixMilli(),
		UpdatedAtUnixMs:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist other route: %v", err)
	}

	svc := grpcserver.NewRegistryService(store, nil, 5, 15, time.Minute)
	if err := svc.RestoreTerminalSessionRoutes(ctx, now); err != nil {
		t.Fatalf("restore routes: %v", err)
	}
	handler := NewWorkerHandler(store, 15*time.Second, svc, svc, svc, "")
	handler.SetTerminalSessionRegistry(svc)
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	listReq.AddCookie(cookie)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d body=%s", listRec.Code, listRec.Body.String())
	}
	var listBody listSessionsResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listBody.Total != 2 || len(listBody.Items) != 2 {
		t.Fatalf("admin list should include all accounts: %#v", listBody)
	}
	if listBody.Items[0].SessionID != "sess-other" || listBody.Items[0].AccountID != "acc-other" {
		t.Fatalf("unexpected newest session: %#v", listBody.Items[0])
	}

	filtered := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?account_id=acc-other", nil)
	filtered.AddCookie(cookie)
	filteredRec := httptest.NewRecorder()
	router.ServeHTTP(filteredRec, filtered)
	var filteredBody listSessionsResponse
	if err := json.Unmarshal(filteredRec.Body.Bytes(), &filteredBody); err != nil {
		t.Fatalf("decode filtered list: %v", err)
	}
	if filteredBody.Total != 1 || filteredBody.Items[0].SessionID != "sess-other" {
		t.Fatalf("unexpected filtered list: %#v", filteredBody)
	}

	missingAccount := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-other", nil)
	missingAccount.AddCookie(cookie)
	missingAccountRec := httptest.NewRecorder()
	router.ServeHTTP(missingAccountRec, missingAccount)
	if missingAccountRec.Code != http.StatusBadRequest {
		t.Fatalf("admin get without account_id expected 400, got %d body=%s", missingAccountRec.Code, missingAccountRec.Body.String())
	}
	missingRenewAccount := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-other/renew", strings.NewReader(`{"lease_ttl_sec":300}`))
	missingRenewAccount.Header.Set("Content-Type", "application/json")
	missingRenewAccount.AddCookie(cookie)
	missingRenewAccountRec := httptest.NewRecorder()
	router.ServeHTTP(missingRenewAccountRec, missingRenewAccount)
	if missingRenewAccountRec.Code != http.StatusBadRequest {
		t.Fatalf("admin renew without account_id expected 400, got %d body=%s", missingRenewAccountRec.Code, missingRenewAccountRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-other?account_id=acc-other", nil)
	getReq.AddCookie(cookie)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get expected 200, got %d body=%s", getRec.Code, getRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-other?account_id=acc-other", nil)
	deleteReq.AddCookie(cookie)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete expected 204, got %d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

func TestMemberSessionHTTPIsAccountScoped(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_701_000_000, 0).UTC()
	ctx := context.Background()
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:acc-member-1:sess-owned",
		NodeID:             "node-owned",
		LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(),
		LastUsedUnixMs:     now.UnixMilli(),
		CreatedAtUnixMs:    now.UnixMilli(),
		UpdatedAtUnixMs:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist owned route: %v", err)
	}
	if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
		ScopedSessionID:    "obx:acc-other:sess-other",
		NodeID:             "node-other",
		LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(),
		LastUsedUnixMs:     now.UnixMilli(),
		CreatedAtUnixMs:    now.UnixMilli(),
		UpdatedAtUnixMs:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("persist other route: %v", err)
	}

	svc := grpcserver.NewRegistryService(store, nil, 5, 15, time.Minute)
	if err := svc.RestoreTerminalSessionRoutes(ctx, now); err != nil {
		t.Fatalf("restore routes: %v", err)
	}
	consoleAuth := newTestConsoleAuth(t)
	seedTestAccount(t, consoleAuth.queries, "acc-member-1", "member-test", "member-password", false)
	handler := NewWorkerHandler(store, 15*time.Second, svc, svc, svc, "")
	handler.SetTerminalSessionRegistry(svc)
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, consoleAuth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookieFor(t, router, "member-test", "member-password")

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil)
	listReq.AddCookie(cookie)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list expected 200, got %d body=%s", listRec.Code, listRec.Body.String())
	}
	var listBody listSessionsResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listBody.Total != 1 || listBody.Items[0].SessionID != "sess-owned" || listBody.Items[0].AccountID != "acc-member-1" {
		t.Fatalf("member list should be self-scoped: %#v", listBody)
	}

	foreignList := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?account_id=acc-other", nil)
	foreignList.AddCookie(cookie)
	foreignListRec := httptest.NewRecorder()
	router.ServeHTTP(foreignListRec, foreignList)
	if foreignListRec.Code != http.StatusNotFound {
		t.Fatalf("member filtered to another account expected 404, got %d body=%s", foreignListRec.Code, foreignListRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-owned", nil)
	getReq.AddCookie(cookie)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("member get own session expected 200, got %d body=%s", getRec.Code, getRec.Body.String())
	}

	foreignGet := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-other", nil)
	foreignGet.AddCookie(cookie)
	foreignGetRec := httptest.NewRecorder()
	router.ServeHTTP(foreignGetRec, foreignGet)
	if foreignGetRec.Code != http.StatusNotFound {
		t.Fatalf("member get other session expected 404, got %d body=%s", foreignGetRec.Code, foreignGetRec.Body.String())
	}
	foreignRenew := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-other/renew?account_id=acc-other", strings.NewReader(`{"lease_ttl_sec":300}`))
	foreignRenew.Header.Set("Content-Type", "application/json")
	foreignRenew.AddCookie(cookie)
	foreignRenewRec := httptest.NewRecorder()
	router.ServeHTTP(foreignRenewRec, foreignRenew)
	if foreignRenewRec.Code != http.StatusNotFound {
		t.Fatalf("member renew other session expected 404, got %d body=%s", foreignRenewRec.Code, foreignRenewRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-owned", nil)
	deleteReq.AddCookie(cookie)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("member delete expected 204, got %d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

func TestListSessionsPagination(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_701_010_000, 0).UTC()
	ctx := context.Background()
	for i, sessionID := range []string{"sess-a", "sess-b", "sess-c"} {
		if err := store.UpsertConfirmedTerminalSessionRoute(ctx, registry.TerminalSessionRoute{
			ScopedSessionID:    "obx:" + testDashboardAccountID + ":" + sessionID,
			NodeID:             "node-a",
			LeaseExpiresUnixMs: now.Add(time.Hour).UnixMilli(),
			LastUsedUnixMs:     now.UnixMilli(),
			CreatedAtUnixMs:    now.Add(time.Duration(i) * time.Minute).UnixMilli(),
			UpdatedAtUnixMs:    now.UnixMilli(),
		}); err != nil {
			t.Fatalf("persist %s: %v", sessionID, err)
		}
	}
	svc := grpcserver.NewRegistryService(store, nil, 5, 15, time.Minute)
	if err := svc.RestoreTerminalSessionRoutes(ctx, now); err != nil {
		t.Fatalf("restore routes: %v", err)
	}
	handler := NewWorkerHandler(store, 15*time.Second, svc, svc, svc, "")
	handler.SetTerminalSessionRegistry(svc)
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?page=1&page_size=2", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body listSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if body.Total != 3 || body.Page != 1 || body.PageSize != 2 || len(body.Items) != 2 {
		t.Fatalf("unexpected page 1: %#v", body)
	}
	if body.Items[0].SessionID != "sess-c" || body.Items[1].SessionID != "sess-b" {
		t.Fatalf("expected newest-first page, got %#v", body.Items)
	}
}

func TestSessionHTTPRejectsInvalidPage(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := grpcserver.NewRegistryService(store, nil, 5, 15, time.Minute)
	handler := NewWorkerHandler(store, 15*time.Second, svc, svc, svc, "")
	handler.SetTerminalSessionRegistry(svc)
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?page=0", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}
