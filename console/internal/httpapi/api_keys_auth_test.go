package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

const (
	testSecondAccountID = "acc-test-second"
	testSecondUsername  = "member-two"
	testSecondPassword  = "password-two"
)

func TestRequireAuthAPIKeyPath(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)

	validRecord, err := bundle.APIKeyAuth.createAPIKey(context.Background(), testDashboardAccountID, "ci-access", "")
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	testCases := []struct {
		name       string
		header     string
		statusCode int
	}{
		{name: "missing header", header: "", statusCode: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Token abc", statusCode: http.StatusUnauthorized},
		{name: "invalid key", header: "Bearer obxk_invalid", statusCode: http.StatusUnauthorized},
		{name: "valid key", header: "Bearer " + validRecord.Key, statusCode: http.StatusOK},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/console/session", nil)
			if tc.header != "" {
				req.Header.Set(trustedTokenHeader, tc.header)
			}
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)

			if res.Code != tc.statusCode {
				t.Fatalf("expected %d, got %d body=%s", tc.statusCode, res.Code, res.Body.String())
			}
		})
	}
}

func TestAPIKeyPathDoesNotFallbackToCookie(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/session", nil)
	req.Header.Set(trustedTokenHeader, "Bearer obxk_invalid")
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestJITTokenDoesNotAuthenticateDashboardRoutes(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	cookie := loginSessionCookie(t, router)
	jitToken := makeTestJITToken(t, "issuer-dashboard", "subject-dashboard")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/session", nil)
	req.Header.Set(trustedTokenHeader, "Bearer "+jitToken)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestNonBearerAuthorizationFallsBackToCookie(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	cookie := loginSessionCookie(t, router)

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/session", nil)
	sessionReq.Header.Set(trustedTokenHeader, "Basic ZGFzaGJvYXJkOnNlY3JldA==")
	sessionReq.AddCookie(cookie)
	sessionRes := httptest.NewRecorder()
	router.ServeHTTP(sessionRes, sessionReq)
	if sessionRes.Code != http.StatusOK {
		t.Fatalf("expected session 200, got %d body=%s", sessionRes.Code, sessionRes.Body.String())
	}

	workersReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	workersReq.Header.Set(trustedTokenHeader, "Digest username=\"demo\"")
	workersReq.AddCookie(cookie)
	workersRes := httptest.NewRecorder()
	router.ServeHTTP(workersRes, workersReq)
	if workersRes.Code != http.StatusOK {
		t.Fatalf("expected workers 200, got %d body=%s", workersRes.Code, workersRes.Body.String())
	}
}

func TestRequireCookieSessionRejectsAPIKeyForSensitiveEndpoints(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)

	validRecord, err := bundle.APIKeyAuth.createAPIKey(context.Background(), testDashboardAccountID, "ci-sensitive", "")
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	passwordReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/console/password",
		strings.NewReader(`{"current_password":"password-test","new_password":"rotated-password"}`),
	)
	passwordReq.Header.Set("Content-Type", "application/json")
	passwordReq.Header.Set(trustedTokenHeader, "Bearer "+validRecord.Key)
	passwordRes := httptest.NewRecorder()
	router.ServeHTTP(passwordRes, passwordReq)
	if passwordRes.Code != http.StatusForbidden {
		t.Fatalf("expected password endpoint 403, got %d body=%s", passwordRes.Code, passwordRes.Body.String())
	}

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/console/api-keys",
		strings.NewReader(`{"name":"blocked"}`),
	)
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set(trustedTokenHeader, "Bearer "+validRecord.Key)
	createRes := httptest.NewRecorder()
	router.ServeHTTP(createRes, createReq)
	if createRes.Code != http.StatusForbidden {
		t.Fatalf("expected create endpoint 403, got %d body=%s", createRes.Code, createRes.Body.String())
	}
}

func TestRequireCookieSessionAllowsCookiePasswordChange(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/console/password",
		strings.NewReader(`{"current_password":"password-test","new_password":"rotated-password"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestAPIKeyCRUDFlow(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	cookie := loginSessionCookie(t, router)

	assertAPIKeyListTotal(t, router, cookie, 0)

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/console/api-keys",
		strings.NewReader(`{"name":"ci-prod"}`),
	)
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(cookie)
	createRes := httptest.NewRecorder()
	router.ServeHTTP(createRes, createReq)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", createRes.Code, createRes.Body.String())
	}

	var created createAPIKeyResponse
	if err := json.Unmarshal(createRes.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create api key response: %v", err)
	}
	if !strings.HasPrefix(created.Key, apiKeyPrefix) {
		t.Fatalf("expected generated api key prefix, got %q", created.Key)
	}
	if created.KeyMasked == "" || created.KeyMasked == created.Key {
		t.Fatalf("expected masked key distinct from plaintext, got %#v", created)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/api-keys", nil)
	listReq.AddCookie(cookie)
	listRes := httptest.NewRecorder()
	router.ServeHTTP(listRes, listReq)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected 200 from list, got %d body=%s", listRes.Code, listRes.Body.String())
	}

	var listed apiKeyListResponse
	if err := json.Unmarshal(listRes.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode api key list: %v", err)
	}
	if listed.Total != 1 || len(listed.Items) != 1 {
		t.Fatalf("expected 1 api key, got %#v", listed)
	}
	if listed.Items[0].KeyMasked != created.KeyMasked {
		t.Fatalf("expected masked key %q, got %#v", created.KeyMasked, listed.Items[0])
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/api-keys/"+created.ID, nil)
	deleteReq.AddCookie(cookie)
	deleteRes := httptest.NewRecorder()
	router.ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusNoContent {
		t.Fatalf("expected 204 from delete, got %d body=%s", deleteRes.Code, deleteRes.Body.String())
	}

	assertAPIKeyListTotal(t, router, cookie, 0)
}

func TestAPIKeyAccountIsolation(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	seedTestAccount(t, bundle.DB.Queries, testSecondAccountID, testSecondUsername, testSecondPassword, false)

	firstRecord, err := bundle.APIKeyAuth.createAPIKey(context.Background(), testDashboardAccountID, "shared-name", "")
	if err != nil {
		t.Fatalf("create first account api key: %v", err)
	}
	secondRecord, err := bundle.APIKeyAuth.createAPIKey(context.Background(), testSecondAccountID, "shared-name", "")
	if err != nil {
		t.Fatalf("create second account api key: %v", err)
	}
	if firstRecord.Name != secondRecord.Name {
		t.Fatalf("expected same api key name, got %q and %q", firstRecord.Name, secondRecord.Name)
	}

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)
	cookie := loginSessionCookie(t, router)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/api-keys", nil)
	listReq.AddCookie(cookie)
	listRes := httptest.NewRecorder()
	router.ServeHTTP(listRes, listReq)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected list 200, got %d body=%s", listRes.Code, listRes.Body.String())
	}

	var payload apiKeyListResponse
	if err := json.Unmarshal(listRes.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 || payload.Items[0].ID != firstRecord.ID {
		t.Fatalf("expected only first account api key, got %#v", payload)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/api-keys/"+secondRecord.ID, nil)
	deleteReq.AddCookie(cookie)
	deleteRes := httptest.NewRecorder()
	router.ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-account delete, got %d body=%s", deleteRes.Code, deleteRes.Body.String())
	}
}

func TestDashboardEndpointsAllowAPIKey(t *testing.T) {
	bundle := newTestAuthBundle(t, false)
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, ":50051")
	router := mustNewRouter(t, handler, bundle.ConsoleAuth, bundle.MCPAuth, bundle.APIKeyAuth)

	validRecord, err := bundle.APIKeyAuth.createAPIKey(context.Background(), testDashboardAccountID, "ci-dashboard", "")
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/api-keys", nil)
	listReq.Header.Set(trustedTokenHeader, "Bearer "+validRecord.Key)
	listRes := httptest.NewRecorder()
	router.ServeHTTP(listRes, listReq)
	if listRes.Code != http.StatusOK {
		t.Fatalf("expected api key list 200, got %d body=%s", listRes.Code, listRes.Body.String())
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/session", nil)
	sessionReq.Header.Set(trustedTokenHeader, "Bearer "+validRecord.Key)
	sessionRes := httptest.NewRecorder()
	router.ServeHTTP(sessionRes, sessionReq)
	if sessionRes.Code != http.StatusOK {
		t.Fatalf("expected session 200, got %d body=%s", sessionRes.Code, sessionRes.Body.String())
	}

	var sessionPayload accountSessionResponse
	if err := json.Unmarshal(sessionRes.Body.Bytes(), &sessionPayload); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	if sessionPayload.Account.AccountID != testDashboardAccountID {
		t.Fatalf("expected account %q, got %#v", testDashboardAccountID, sessionPayload.Account)
	}

	workersReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	workersReq.Header.Set(trustedTokenHeader, "Bearer "+validRecord.Key)
	workersRes := httptest.NewRecorder()
	router.ServeHTTP(workersRes, workersReq)
	if workersRes.Code != http.StatusOK {
		t.Fatalf("expected workers 200, got %d body=%s", workersRes.Code, workersRes.Body.String())
	}
}

func assertAPIKeyListTotal(t *testing.T, router http.Handler, cookie *http.Cookie, total int) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/api-keys", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 from list, got %d body=%s", res.Code, res.Body.String())
	}

	var payload apiKeyListResponse
	if err := json.NewDecoder(bytes.NewReader(res.Body.Bytes())).Decode(&payload); err != nil {
		t.Fatalf("decode api key list: %v", err)
	}
	if payload.Total != total || len(payload.Items) != total {
		t.Fatalf("expected total=%d, got %#v", total, payload)
	}
}
