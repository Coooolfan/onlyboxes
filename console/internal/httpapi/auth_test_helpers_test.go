package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
)

const (
	testDashboardUsername = "admin-test"
	testDashboardPassword = "password-test"
	testMCPToken          = "mcp-token-test"
	testMCPTokenB         = "mcp-token-test-b"
	testJITSigningKey     = "test-jit-signing-key"
)

const testDashboardAccountID = "acc-test-dashboard"

type testAuthBundle struct {
	DB          *persistence.DB
	ConsoleAuth *ConsoleAuth
	MCPAuth     *MCPAuth
	APIKeyAuth  *APIKeyAuth
}

func newTestConsoleAuth(t *testing.T) *ConsoleAuth {
	return newTestConsoleAuthWithRegistration(t, false)
}

func newTestConsoleAuthWithRegistration(t *testing.T, registrationEnabled bool) *ConsoleAuth {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := fmt.Sprintf("file:onlyboxes-consoleauth-test-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := persistence.Open(ctx, persistence.Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open test console auth db: %v", err)
	}
	seedTestAccount(t, db.Queries, testDashboardAccountID, testDashboardUsername, testDashboardPassword, true)
	auth, err := NewConsoleAuth(db.Queries, registrationEnabled)
	if err != nil {
		t.Fatalf("new console auth: %v", err)
	}
	return auth
}

func newTestMCPAuth(t testing.TB) *MCPAuth {
	t.Helper()

	auth := newBareTestMCPAuth(t)
	tokenA := testMCPToken
	tokenB := testMCPTokenB
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-a", &tokenA); err != nil {
		t.Fatalf("seed token-a: %v", err)
	}
	if _, _, err := auth.createToken(context.Background(), testDashboardAccountID, "token-b", &tokenB); err != nil {
		t.Fatalf("seed token-b: %v", err)
	}
	return auth
}

func newBareTestMCPAuth(t testing.TB) *MCPAuth {
	t.Helper()

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
		t.Fatalf("open test mcp auth db: %v", err)
	}
	seedTestAccount(t, db.Queries, testDashboardAccountID, testDashboardUsername, testDashboardPassword, true)
	auth, err := NewMCPAuthWithPersistence(db)
	if err != nil {
		t.Fatalf("new mcp auth: %v", err)
	}
	auth.SetJITSigningKey(testJITSigningKey)
	return auth
}

func newTestAuthBundle(t testing.TB, registrationEnabled bool) *testAuthBundle {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := fmt.Sprintf("file:onlyboxes-authbundle-test-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := persistence.Open(ctx, persistence.Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open test auth bundle db: %v", err)
	}
	seedTestAccount(t, db.Queries, testDashboardAccountID, testDashboardUsername, testDashboardPassword, true)

	consoleAuth, err := NewConsoleAuth(db.Queries, registrationEnabled)
	if err != nil {
		t.Fatalf("new console auth bundle: %v", err)
	}
	mcpAuth, err := NewMCPAuthWithPersistence(db)
	if err != nil {
		t.Fatalf("new mcp auth bundle: %v", err)
	}
	mcpAuth.SetJITSigningKey(testJITSigningKey)
	apiKeyAuth, err := NewAPIKeyAuth(db)
	if err != nil {
		t.Fatalf("new api key auth bundle: %v", err)
	}

	return &testAuthBundle{
		DB:          db,
		ConsoleAuth: consoleAuth,
		MCPAuth:     mcpAuth,
		APIKeyAuth:  apiKeyAuth,
	}
}

func mustNewRouter(t *testing.T, workerHandler *WorkerHandler, consoleAuth *ConsoleAuth, mcpAuth *MCPAuth, apiKeyAuth *APIKeyAuth, hiddenTools ...map[string]bool) *gin.Engine {
	t.Helper()

	var ht map[string]bool
	if len(hiddenTools) > 0 {
		ht = hiddenTools[0]
	}
	router, err := NewRouter(workerHandler, consoleAuth, mcpAuth, apiKeyAuth, ht, nil)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router
}

func setMCPTokenHeader(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set(trustedTokenHeader, "Bearer "+testMCPToken)
}

func seedTestAccount(t testing.TB, queries *sqlc.Queries, accountID string, username string, password string, isAdmin bool) {
	t.Helper()

	if queries == nil {
		t.Fatalf("seed test account requires non-nil queries")
	}
	passwordHash, err := hashDashboardPassword(password)
	if err != nil {
		t.Fatalf("hash dashboard password: %v", err)
	}
	nowMS := time.Now().UnixMilli()
	if err := queries.InsertAccount(context.Background(), sqlc.InsertAccountParams{
		AccountID:       strings.TrimSpace(accountID),
		Username:        strings.TrimSpace(username),
		UsernameKey:     strings.ToLower(strings.TrimSpace(username)),
		PasswordHash:    passwordHash,
		HashAlgo:        dashboardPasswordHashAlgo,
		IsAdmin:         boolToInt64(isAdmin),
		CreatedAtUnixMs: nowMS,
		UpdatedAtUnixMs: nowMS,
	}); err != nil {
		t.Fatalf("insert test account: %v", err)
	}
}

func makeTestJITToken(t testing.TB, issuer string, subject string) string {
	t.Helper()

	return makeTestJITTokenWithClaims(t, jitTokenClaims{
		Issuer:  issuer,
		Subject: subject,
	})
}

func makeTestJITTokenWithClaims(t testing.TB, claims jitTokenClaims) string {
	t.Helper()

	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal jit token payload: %v", err)
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payloadJSON)
	mac := hmac.New(sha256.New, []byte(testJITSigningKey))
	_, _ = mac.Write([]byte(jitTokenPrefix + payloadEncoded))
	signatureEncoded := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return jitTokenPrefix + payloadEncoded + "." + signatureEncoded
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
