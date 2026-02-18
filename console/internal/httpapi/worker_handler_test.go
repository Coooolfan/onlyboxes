package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
)

type fakeWorkerProvisioning struct {
	secrets      map[string]string
	createNodeID string
	createSecret string
	createErr    error
}

func (p *fakeWorkerProvisioning) GetWorkerSecret(nodeID string) (string, bool) {
	if p == nil || p.secrets == nil {
		return "", false
	}
	secret, ok := p.secrets[nodeID]
	return secret, ok
}

func (p *fakeWorkerProvisioning) CreateProvisionedWorker(_ time.Time, _ time.Duration) (string, string, error) {
	if p == nil {
		return "", "", errors.New("provisioning unavailable")
	}
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
	store := registry.NewStore()
	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, "")
	handler.nowFn = func() time.Time {
		return time.Unix(1_700_000_000, 0)
	}
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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

	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, "")
	handler.nowFn = func() time.Time {
		return base.Add(20 * time.Second)
	}
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
	handler := NewWorkerHandler(store, 15*time.Second, nil, nil, "")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())

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
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, provisioning, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader("{}"))
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
	if !strings.Contains(payload.Command, "WORKER_ID=node-new-1") {
		t.Fatalf("expected WORKER_ID in command, got %q", payload.Command)
	}
	if !strings.Contains(payload.Command, "WORKER_SECRET=secret-new-1") {
		t.Fatalf("expected WORKER_SECRET in command, got %q", payload.Command)
	}
}

func TestCreateWorkerRequiresAuthentication(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, &fakeWorkerProvisioning{}, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestDeleteWorkerSuccess(t *testing.T) {
	provisioning := &fakeWorkerProvisioning{
		secrets: map[string]string{"node-delete-1": "secret-delete-1"},
	}
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, provisioning, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, provisioning, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, provisioning, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/node-delete-1", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestGetWorkerStartupCommandSuccess(t *testing.T) {
	handler := NewWorkerHandler(
		registry.NewStore(),
		15*time.Second,
		nil,
		&fakeWorkerProvisioning{secrets: map[string]string{"node-copy-1": "secret-copy-1"}},
		":50051",
	)
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	req.Host = "console.local:8089"
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", res.Code, res.Body.String())
	}

	var payload workerStartupCommandResponse
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload.NodeID != "node-copy-1" {
		t.Fatalf("expected node_id node-copy-1, got %q", payload.NodeID)
	}
	if !strings.Contains(payload.Command, "WORKER_CONSOLE_GRPC_TARGET=console.local:50051") {
		t.Fatalf("expected resolved grpc target in command, got %q", payload.Command)
	}
	if !strings.Contains(payload.Command, "WORKER_ID=node-copy-1") {
		t.Fatalf("expected WORKER_ID in command, got %q", payload.Command)
	}
	if !strings.Contains(payload.Command, "WORKER_SECRET=secret-copy-1") {
		t.Fatalf("expected WORKER_SECRET in command, got %q", payload.Command)
	}
	if !strings.Contains(payload.Command, "go run ./cmd/worker-docker") {
		t.Fatalf("expected worker command tail, got %q", payload.Command)
	}
}

func TestGetWorkerStartupCommandRequiresAuthentication(t *testing.T) {
	handler := NewWorkerHandler(
		registry.NewStore(),
		15*time.Second,
		nil,
		&fakeWorkerProvisioning{secrets: map[string]string{"node-copy-1": "secret-copy-1"}},
		":50051",
	)
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestGetWorkerStartupCommandNotFound(t *testing.T) {
	handler := NewWorkerHandler(
		registry.NewStore(),
		15*time.Second,
		nil,
		&fakeWorkerProvisioning{secrets: map[string]string{"node-copy-1": "secret-copy-1"}},
		":50051",
	)
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-missing/startup-command", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestListTrustedTokensSuccess(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, nil, ":50051")
	mcpAuth := NewMCPAuth()
	tokenA := "token-a"
	tokenB := "token-b"
	if _, _, err := mcpAuth.createToken("token-a", &tokenA); err != nil {
		t.Fatalf("seed token-a failed: %v", err)
	}
	if _, _, err := mcpAuth.createToken("token-b", &tokenB); err != nil {
		t.Fatalf("seed token-b failed: %v", err)
	}
	router := NewRouter(handler, newTestConsoleAuth(t), mcpAuth)
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
	if payload.Items[0].Name != "token-a" || payload.Items[1].Name != "token-b" {
		t.Fatalf("unexpected token names: %#v", payload.Items)
	}
	if payload.Items[0].TokenMasked != "*******" || payload.Items[1].TokenMasked != "*******" {
		t.Fatalf("unexpected masked token payload: %#v", payload.Items)
	}
}

func TestListTrustedTokensRequiresAuthentication(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, nil, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), NewMCPAuth())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/tokens", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestCreateAndGetTrustedTokenValueSuccess(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, nil, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), NewMCPAuth())
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
	if getRes.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", getRes.Code, getRes.Body.String())
	}
	valuePayload := trustedTokenValueResponse{}
	if err := json.Unmarshal(getRes.Body.Bytes(), &valuePayload); err != nil {
		t.Fatalf("decode value response: %v", err)
	}
	if valuePayload.ID != payload.ID || valuePayload.Token != payload.Token {
		t.Fatalf("unexpected value payload: %#v", valuePayload)
	}
}

func TestDeleteTrustedTokenSuccess(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, nil, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), NewMCPAuth())
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
	if getRes.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d body=%s", getRes.Code, getRes.Body.String())
	}
}

func TestResolveWorkerGRPCTargetPortOnlyUsesRequestHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	req.Host = "panel.example.com:8089"

	target := resolveWorkerGRPCTarget(":50051", req)
	if target != "panel.example.com:50051" {
		t.Fatalf("expected panel.example.com:50051, got %s", target)
	}
}

func TestResolveWorkerGRPCTargetWildcardHostUsesRequestHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	req.Host = "panel.example.com:8089"

	target := resolveWorkerGRPCTarget("0.0.0.0:50051", req)
	if target != "panel.example.com:50051" {
		t.Fatalf("expected panel.example.com:50051, got %s", target)
	}
}

func TestResolveWorkerGRPCTargetFallbackHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/node-copy-1/startup-command", nil)
	req.Host = ""

	target := resolveWorkerGRPCTarget(":50051", req)
	if target != "127.0.0.1:50051" {
		t.Fatalf("expected 127.0.0.1:50051, got %s", target)
	}
}
