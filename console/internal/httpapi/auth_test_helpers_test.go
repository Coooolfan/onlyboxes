package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
)

const (
	testDashboardUsername = "admin-test"
	testDashboardPassword = "password-test"
	testMCPToken          = "mcp-token-test"
	testMCPTokenB         = "mcp-token-test-b"
)

func newTestConsoleAuth(t *testing.T) *ConsoleAuth {
	t.Helper()
	hasher, err := persistence.NewHasher("test-hash-key")
	if err != nil {
		t.Fatalf("create test hasher: %v", err)
	}
	return NewConsoleAuth(DashboardCredentialMaterial{
		Username:     testDashboardUsername,
		PasswordHash: hasher.Hash(testDashboardPassword),
		HashAlgo:     persistence.HashAlgorithmHMACSHA256,
		Hasher:       hasher,
	})
}

func newTestMCPAuth() *MCPAuth {
	auth := newBareTestMCPAuth()
	tokenA := testMCPToken
	tokenB := testMCPTokenB
	if _, _, err := auth.createToken("token-a", &tokenA); err != nil {
		panic(err)
	}
	if _, _, err := auth.createToken("token-b", &tokenB); err != nil {
		panic(err)
	}
	return auth
}

func newBareTestMCPAuth() *MCPAuth {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := fmt.Sprintf("file:onlyboxes-mcpauth-test-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := persistence.Open(ctx, persistence.Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		panic(err)
	}
	return NewMCPAuthWithPersistence(db)
}

func setMCPTokenHeader(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set(trustedTokenHeader, testMCPToken)
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
