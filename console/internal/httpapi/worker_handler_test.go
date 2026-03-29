package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

type fakeWorkerProvisioning struct {
	secrets      map[string]string
	createNodeID string
	createSecret string
	createErr    error
	lastOwnerID  string
	lastType     string
}

func (p *fakeWorkerProvisioning) GetWorkerSecret(nodeID string) (string, bool) {
	if p == nil || p.secrets == nil {
		return "", false
	}
	secret, ok := p.secrets[nodeID]
	return secret, ok
}

func (p *fakeWorkerProvisioning) CreateProvisionedWorkerForOwner(ownerID string, workerType string, _ time.Time, _ time.Duration) (string, string, error) {
	if p == nil {
		return "", "", errors.New("provisioning unavailable")
	}
	p.lastOwnerID = ownerID
	p.lastType = workerType
	if p.createErr != nil {
		return "", "", p.createErr
	}
	if p.createNodeID == "" || p.createSecret == "" {
		return "", "", errors.New("missing create payload")
	}
	if p.secrets == nil {
		p.secrets = make(map[string]string)
	}
	p.secrets[p.createNodeID] = p.createSecret
	return p.createNodeID, p.createSecret, nil
}

func (p *fakeWorkerProvisioning) DeleteProvisionedWorker(nodeID string) bool {
	if p == nil || p.secrets == nil {
		return false
	}
	if _, ok := p.secrets[nodeID]; !ok {
		return false
	}
	delete(p.secrets, nodeID)
	return true
}

func TestListWorkersEmpty(t *testing.T) {
	store := registrytest.NewStore(t)
	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	handler.nowFn = func() time.Time {
		return time.Unix(1_700_000_000, 0)
	}
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
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
	store := registrytest.NewStore(t)
	base := time.Unix(1_700_000_100, 0)

	store.Upsert(&registryv1.ConnectHello{NodeId: "node-2", NodeName: "node-2"}, "session-2", base)
	store.Upsert(&registryv1.ConnectHello{NodeId: "node-1", NodeName: "node-1"}, "session-1", base.Add(10*time.Second))
	store.Upsert(&registryv1.ConnectHello{NodeId: "node-3", NodeName: "node-3"}, "session-3", base.Add(12*time.Second))

	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	handler.nowFn = func() time.Time {
		return base.Add(20 * time.Second)
	}
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
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
	store := registrytest.NewStore(t)
	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestCreateWorkerSuccess(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{
		secrets:      map[string]string{},
		createNodeID: "node-new-1",
		createSecret: "secret-new-1",
	}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(`{"type":"normal"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "console.local:8089"
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", res.Code, res.Body.String())
	}

	var payload workerStartupCommandResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload.NodeID != "node-new-1" {
		t.Fatalf("expected node_id node-new-1, got %q", payload.NodeID)
	}
	if payload.Type != registry.WorkerTypeNormal {
		t.Fatalf("expected type %q, got %q", registry.WorkerTypeNormal, payload.Type)
	}
	if payload.WorkerSecret != "secret-new-1" {
		t.Fatalf("expected worker_secret secret-new-1, got %q", payload.WorkerSecret)
	}
	if provisioning.lastType != registry.WorkerTypeNormal {
		t.Fatalf("expected provisioning type %q, got %q", registry.WorkerTypeNormal, provisioning.lastType)
	}
	if strings.TrimSpace(provisioning.lastOwnerID) == "" {
		t.Fatalf("expected non-empty owner id for created worker")
	}
}

func TestCreateWorkerRequiresAuthentication(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, &fakeWorkerProvisioning{}, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(`{"type":"worker-sys"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestCreateWorkerRejectsMissingType(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{createNodeID: "node-new-1", createSecret: "secret-new-1"}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestCreateWorkerRejectsNormalTypeForNonAdmin(t *testing.T) {
	consoleAuth := newTestConsoleAuth(t)
	seedTestAccount(t, consoleAuth.queries, "acc-member-1", "member-test", "member-password", false)
	provisioning := &fakeWorkerProvisioning{createNodeID: "node-new-1", createSecret: "secret-new-1"}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, consoleAuth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookieFor(t, router, "member-test", "member-password")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(`{"type":"normal"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestCreateWorkerMapsWorkerSysConflict(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{
		createErr: grpcserver.ErrWorkerSysAlreadyExists,
	}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(`{"type":"worker-sys"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestListWorkersScopesToOwnWorkerSysForNonAdmin(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_200, 0)
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-own-sys",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-member-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
		},
	}, "session-own-sys", now); err != nil {
		t.Fatalf("seed own sys worker: %v", err)
	}
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-own-normal",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-member-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeNormal,
		},
	}, "session-own-normal", now); err != nil {
		t.Fatalf("seed own normal worker: %v", err)
	}
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-other-sys",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-other-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
		},
	}, "session-other-sys", now); err != nil {
		t.Fatalf("seed other sys worker: %v", err)
	}

	consoleAuth := newTestConsoleAuth(t)
	seedTestAccount(t, consoleAuth.queries, "acc-member-1", "member-test", "member-password", false)
	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, nil, "")
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, consoleAuth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookieFor(t, router, "member-test", "member-password")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.Code, res.Body.String())
	}
	payload := listWorkersResponse{}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 {
		t.Fatalf("expected only own worker-sys item, got total=%d len=%d", payload.Total, len(payload.Items))
	}
	if payload.Items[0].NodeID != "node-own-sys" {
		t.Fatalf("expected node-own-sys, got %s", payload.Items[0].NodeID)
	}
}

func TestDeleteWorkerScopesToOwnWorkerSysForNonAdmin(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_250, 0)
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-own-sys",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-member-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
		},
	}, "session-own-sys", now); err != nil {
		t.Fatalf("seed own sys worker: %v", err)
	}
	if err := store.Upsert(&registryv1.ConnectHello{
		NodeId: "node-own-normal",
		Labels: map[string]string{
			registry.LabelOwnerIDKey:    "acc-member-1",
			registry.LabelWorkerTypeKey: registry.WorkerTypeNormal,
		},
	}, "session-own-normal", now); err != nil {
		t.Fatalf("seed own normal worker: %v", err)
	}

	consoleAuth := newTestConsoleAuth(t)
	seedTestAccount(t, consoleAuth.queries, "acc-member-1", "member-test", "member-password", false)
	provisioning := &fakeWorkerProvisioning{
		secrets: map[string]string{
			"node-own-sys":    "secret-own-sys",
			"node-own-normal": "secret-own-normal",
		},
	}
	handler := NewWorkerHandler(store, 15*time.Second, nil, provisioning, nil, "")
	handler.nowFn = func() time.Time { return now }
	router := mustNewRouter(t, handler, consoleAuth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookieFor(t, router, "member-test", "member-password")

	reqOwnSys := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/node-own-sys", nil)
	reqOwnSys.AddCookie(cookie)
	resOwnSys := httptest.NewRecorder()
	router.ServeHTTP(resOwnSys, reqOwnSys)
	if resOwnSys.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for own worker-sys, got %d body=%s", resOwnSys.Code, resOwnSys.Body.String())
	}

	reqOwnNormal := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/node-own-normal", nil)
	reqOwnNormal.AddCookie(cookie)
	resOwnNormal := httptest.NewRecorder()
	router.ServeHTTP(resOwnNormal, reqOwnNormal)
	if resOwnNormal.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for own normal worker, got %d body=%s", resOwnNormal.Code, resOwnNormal.Body.String())
	}
}

func TestDeleteWorkerSuccess(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{
		secrets: map[string]string{"node-delete-1": "secret-delete-1"},
	}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/node-delete-1", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", res.Code, res.Body.String())
	}
	if _, ok := provisioning.GetWorkerSecret("node-delete-1"); ok {
		t.Fatalf("expected worker to be removed from provisioning secrets")
	}
}

func TestDeleteWorkerNotFound(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{
		secrets: map[string]string{"node-delete-1": "secret-delete-1"},
	}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/node-missing", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestDeleteWorkerRequiresAuthentication(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{
		secrets: map[string]string{"node-delete-1": "secret-delete-1"},
	}
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, provisioning, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/node-delete-1", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestGetWorkerStartupCommandReturnsGone(t *testing.T) {
	handler := NewWorkerHandler(
		registrytest.NewStore(t),
		15*time.Second,
		nil,
		&fakeWorkerProvisioning{secrets: map[string]string{"node-copy-1": "secret-copy-1"}},
		nil,
		":50051",
	)
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	req.Host = "console.local:8089"
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestGetWorkerStartupCommandRequiresAuthentication(t *testing.T) {
	handler := NewWorkerHandler(
		registrytest.NewStore(t),
		15*time.Second,
		nil,
		&fakeWorkerProvisioning{secrets: map[string]string{"node-copy-1": "secret-copy-1"}},
		nil,
		":50051",
	)
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestGetWorkerStartupCommandForMissingWorkerStillReturnsGone(t *testing.T) {
	handler := NewWorkerHandler(
		registrytest.NewStore(t),
		15*time.Second,
		nil,
		&fakeWorkerProvisioning{secrets: map[string]string{"node-copy-1": "secret-copy-1"}},
		nil,
		":50051",
	)
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-missing/startup-command", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestListTrustedTokensSuccess(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	mcpAuth := newBareTestMCPAuth(t)
	tokenA := "token-a"
	tokenB := "token-b"
	if _, _, err := mcpAuth.createToken(context.Background(), testDashboardAccountID, "token-a", &tokenA); err != nil {
		t.Fatalf("seed token-a failed: %v", err)
	}
	if _, _, err := mcpAuth.createToken(context.Background(), testDashboardAccountID, "token-b", &tokenB); err != nil {
		t.Fatalf("seed token-b failed: %v", err)
	}
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), mcpAuth, nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/tokens", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.Code, res.Body.String())
	}

	var payload trustedTokenListResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload.Total != 2 {
		t.Fatalf("expected total=2, got %d", payload.Total)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(payload.Items))
	}
	tokenNames := map[string]struct{}{}
	for _, item := range payload.Items {
		tokenNames[item.Name] = struct{}{}
		if item.TokenMasked != "*******" {
			t.Fatalf("unexpected masked token payload: %#v", payload.Items)
		}
	}
	if _, ok := tokenNames["token-a"]; !ok {
		t.Fatalf("token-a not found in list: %#v", payload.Items)
	}
	if _, ok := tokenNames["token-b"]; !ok {
		t.Fatalf("token-b not found in list: %#v", payload.Items)
	}
}

func TestListTrustedTokensRequiresAuthentication(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newBareTestMCPAuth(t), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/tokens", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestNewRouterReturnsErrorWhenMCPAuthIsNil(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router, err := NewRouter(handler, newTestConsoleAuth(t), nil, nil, nil)
	if !errors.Is(err, ErrMCPAuthRequired) {
		t.Fatalf("expected ErrMCPAuthRequired, got %v", err)
	}
	if router != nil {
		t.Fatalf("expected nil router when mcpAuth is nil")
	}
}

func TestCreateTrustedTokenGetValueReturnsGone(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newBareTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/tokens", strings.NewReader(`{"name":"ci-prod"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(cookie)
	createRes := httptest.NewRecorder()
	router.ServeHTTP(createRes, createReq)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createRes.Code, createRes.Body.String())
	}

	payload := createTrustedTokenResponse{}
	if err := json.Unmarshal(createRes.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/tokens/"+payload.ID+"/value", nil)
	getReq.AddCookie(cookie)
	getRes := httptest.NewRecorder()
	router.ServeHTTP(getRes, getReq)
	if getRes.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d body=%s", getRes.Code, getRes.Body.String())
	}
}

func TestDeleteTrustedTokenSuccess(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newBareTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/tokens", strings.NewReader(`{"name":"ci-prod","token":"manual-token"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(cookie)
	createRes := httptest.NewRecorder()
	router.ServeHTTP(createRes, createReq)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createRes.Code, createRes.Body.String())
	}
	payload := createTrustedTokenResponse{}
	if err := json.Unmarshal(createRes.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/tokens/"+payload.ID, nil)
	deleteReq.AddCookie(cookie)
	deleteRes := httptest.NewRecorder()
	router.ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", deleteRes.Code, deleteRes.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/tokens/"+payload.ID+"/value", nil)
	getReq.AddCookie(cookie)
	getRes := httptest.NewRecorder()
	router.ServeHTTP(getRes, getReq)
	if getRes.Code != http.StatusGone {
		t.Fatalf("expected 410 after delete, got %d body=%s", getRes.Code, getRes.Body.String())
	}
}
