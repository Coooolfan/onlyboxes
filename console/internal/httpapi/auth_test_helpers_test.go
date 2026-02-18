package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testDashboardUsername = "admin-test"
	testDashboardPassword = "password-test"
	testMCPToken          = "mcp-token-test"
	testMCPTokenB         = "mcp-token-test-b"
)

func newTestConsoleAuth(t *testing.T) *ConsoleAuth {
	t.Helper()
	return NewConsoleAuth(DashboardCredentials{
		Username: testDashboardUsername,
		Password: testDashboardPassword,
	})
}

func newTestMCPAuth() *MCPAuth {
	return NewMCPAuth([]string{testMCPToken, testMCPTokenB})
}

func setMCPTokenHeader(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set(mcpTokenHeader, testMCPToken)
}

func loginSessionCookie(t *testing.T, router http.Handler) *http.Cookie {
	t.Helper()

	body, err := json.Marshal(loginRequest{
		Username: testDashboardUsername,
		Password: testDashboardPassword,
	})
	if err != nil {
		t.Fatalf("failed to marshal login request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected login success, got %d body=%s", rec.Code, rec.Body.String())
	}

	resp := rec.Result()
	defer resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == dashboardSessionCookieName {
			return cookie
		}
	}
	t.Fatalf("expected %s cookie in login response", dashboardSessionCookieName)
	return nil
}
