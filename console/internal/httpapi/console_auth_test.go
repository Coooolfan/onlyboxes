package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence/sqlc"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func TestResolveDashboardCredentials(t *testing.T) {
	tests := []struct {
		name             string
		username         string
		password         string
		expectUsername   string
		expectPassword   string
		expectRandomUser bool
		expectRandomPass bool
	}{
		{
			name:             "both-random",
			expectRandomUser: true,
			expectRandomPass: true,
		},
		{
			name:             "only-username-configured",
			username:         "admin-fixed",
			expectUsername:   "admin-fixed",
			expectRandomPass: true,
		},
		{
			name:             "only-password-configured",
			password:         "secret-fixed",
			expectPassword:   "secret-fixed",
			expectRandomUser: true,
		},
		{
			name:           "both-configured",
			username:       "admin-fixed",
			password:       "secret-fixed",
			expectUsername: "admin-fixed",
			expectPassword: "secret-fixed",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			credentials, err := ResolveDashboardCredentials(tc.username, tc.password)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.expectRandomUser {
				if !strings.HasPrefix(credentials.Username, dashboardUsernamePrefix) {
					t.Fatalf("expected username prefix %q, got %q", dashboardUsernamePrefix, credentials.Username)
				}
				if len(credentials.Username) <= len(dashboardUsernamePrefix) {
					t.Fatalf("expected random username suffix, got %q", credentials.Username)
				}
			} else if credentials.Username != tc.expectUsername {
				t.Fatalf("expected username %q, got %q", tc.expectUsername, credentials.Username)
			}

			if tc.expectRandomPass {
				if strings.TrimSpace(credentials.Password) == "" {
					t.Fatalf("expected random password, got empty")
				}
			} else if credentials.Password != tc.expectPassword {
				t.Fatalf("expected password %q, got %q", tc.expectPassword, credentials.Password)
			}
		})
	}
}

func TestInitializeDashboardCredentialsPersistsOnFirstRun(t *testing.T) {
	ctx := context.Background()
	db := openTestDashboardDB(t)
	defer func() {
		_ = db.Close()
	}()

	result, err := InitializeDashboardCredentials(ctx, db.Queries, db.Hasher, "", "")
	if err != nil {
		t.Fatalf("initialize dashboard credentials: %v", err)
	}
	if !result.InitializedNow {
		t.Fatalf("expected initialized now")
	}
	if result.EnvIgnored {
		t.Fatalf("expected env_ignored=false")
	}
	if !strings.HasPrefix(result.Username, dashboardUsernamePrefix) {
		t.Fatalf("expected generated username prefix %q, got %q", dashboardUsernamePrefix, result.Username)
	}
	if strings.TrimSpace(result.PasswordPlaintext) == "" {
		t.Fatalf("expected plaintext password in first initialization result")
	}
	if strings.TrimSpace(result.PasswordHash) == "" {
		t.Fatalf("expected non-empty password hash")
	}
	if result.PasswordHash == result.PasswordPlaintext {
		t.Fatalf("expected hash storage, got plaintext")
	}
	if !strings.EqualFold(result.HashAlgo, persistence.HashAlgorithmHMACSHA256) {
		t.Fatalf("unexpected hash algo: %q", result.HashAlgo)
	}

	stored, err := db.Queries.GetDashboardCredential(ctx)
	if err != nil {
		t.Fatalf("get persisted dashboard credential: %v", err)
	}
	if stored.SingletonID != dashboardCredentialSingletonID {
		t.Fatalf("unexpected singleton id: %d", stored.SingletonID)
	}
	if stored.Username != result.Username {
		t.Fatalf("unexpected persisted username: %q", stored.Username)
	}
	if stored.PasswordHash != result.PasswordHash {
		t.Fatalf("unexpected persisted hash")
	}
}

func TestInitializeDashboardCredentialsLoadsPersistedAndIgnoresEnv(t *testing.T) {
	ctx := context.Background()
	db := openTestDashboardDB(t)
	defer func() {
		_ = db.Close()
	}()

	first, err := InitializeDashboardCredentials(ctx, db.Queries, db.Hasher, "admin-first", "password-first")
	if err != nil {
		t.Fatalf("initialize first dashboard credential: %v", err)
	}
	if !first.InitializedNow {
		t.Fatalf("expected first initialization")
	}

	second, err := InitializeDashboardCredentials(ctx, db.Queries, db.Hasher, "admin-second", "password-second")
	if err != nil {
		t.Fatalf("initialize second dashboard credential: %v", err)
	}
	if second.InitializedNow {
		t.Fatalf("expected loading existing credential, got initialized=true")
	}
	if !second.EnvIgnored {
		t.Fatalf("expected env ignored when persisted credential exists")
	}
	if second.Username != first.Username {
		t.Fatalf("expected persisted username %q, got %q", first.Username, second.Username)
	}
	if second.PasswordHash != first.PasswordHash {
		t.Fatalf("expected persisted password hash")
	}
	if second.PasswordPlaintext != "" {
		t.Fatalf("expected empty plaintext password for loaded credential")
	}

	auth := NewConsoleAuth(DashboardCredentialMaterial{
		Username:     second.Username,
		PasswordHash: second.PasswordHash,
		HashAlgo:     second.HashAlgo,
		Hasher:       db.Hasher,
	})
	if !auth.checkCredentials("admin-first", "password-first") {
		t.Fatalf("expected persisted credential to remain valid")
	}
	if auth.checkCredentials("admin-second", "password-second") {
		t.Fatalf("expected env credential not to override persisted credential")
	}
}

func TestInitializeDashboardCredentialsRejectsUnsupportedHashAlgo(t *testing.T) {
	ctx := context.Background()
	db := openTestDashboardDB(t)
	defer func() {
		_ = db.Close()
	}()

	if err := db.Queries.InsertDashboardCredential(ctx, sqlc.InsertDashboardCredentialParams{
		SingletonID:     dashboardCredentialSingletonID,
		Username:        "admin",
		PasswordHash:    db.Hasher.Hash("secret"),
		HashAlgo:        "legacy-plain",
		CreatedAtUnixMs: time.Now().UnixMilli(),
		UpdatedAtUnixMs: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed dashboard credential: %v", err)
	}

	_, err := InitializeDashboardCredentials(ctx, db.Queries, db.Hasher, "", "")
	if err == nil {
		t.Fatalf("expected error for unsupported hash algo")
	}
}

func TestCheckCredentialsAlwaysEvaluatesPasswordPath(t *testing.T) {
	auth := newTestConsoleAuth(t)
	originalEqual := auth.passwordEqualFn
	var equalCalls int32
	auth.passwordEqualFn = func(hash string, plain string) bool {
		atomic.AddInt32(&equalCalls, 1)
		return originalEqual(hash, plain)
	}

	if auth.checkCredentials("wrong-user", testDashboardPassword) {
		t.Fatalf("expected invalid credentials for username mismatch")
	}
	if atomic.LoadInt32(&equalCalls) != 1 {
		t.Fatalf("expected password compare path to run once, got %d", atomic.LoadInt32(&equalCalls))
	}
}

func TestConsoleAuthLoginLogoutLifecycle(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, "")
	auth := newTestConsoleAuth(t)
	router := NewRouter(handler, auth, newTestMCPAuth())

	failedReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/login", strings.NewReader(`{"username":"wrong","password":"wrong"}`))
	failedReq.Header.Set("Content-Type", "application/json")
	failedRec := httptest.NewRecorder()
	router.ServeHTTP(failedRec, failedReq)
	if failedRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid login, got %d", failedRec.Code)
	}

	sessionCookie := loginSessionCookie(t, router)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	listReq.AddCookie(sessionCookie)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for authenticated list request, got %d body=%s", listRec.Code, listRec.Body.String())
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/logout", nil)
	logoutReq.AddCookie(sessionCookie)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for logout, got %d", logoutRec.Code)
	}

	listAfterLogoutReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	listAfterLogoutReq.AddCookie(sessionCookie)
	listAfterLogoutRec := httptest.NewRecorder()
	router.ServeHTTP(listAfterLogoutRec, listAfterLogoutReq)
	if listAfterLogoutRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d body=%s", listAfterLogoutRec.Code, listAfterLogoutRec.Body.String())
	}
}

func TestConsoleAuthSessionExpires(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, "")
	auth := newTestConsoleAuth(t)
	now := time.Unix(1_700_000_000, 0)
	auth.nowFn = func() time.Time {
		return now
	}
	router := NewRouter(handler, auth, newTestMCPAuth())
	sessionCookie := loginSessionCookie(t, router)

	now = now.Add(dashboardSessionTTL + time.Second)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.AddCookie(sessionCookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired session, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func openTestDashboardDB(t *testing.T) *persistence.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dashboard-auth.db")
	db, err := persistence.Open(context.Background(), persistence.Options{
		Path:             path,
		BusyTimeoutMS:    5000,
		HashKey:          "test-hash-key",
		TaskRetentionDay: 30,
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	return db
}
