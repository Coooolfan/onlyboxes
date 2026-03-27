package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/buildinfo"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
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

func TestNewConsoleAuthNilQueriesReturnsError(t *testing.T) {
	auth, err := NewConsoleAuth(nil, false)
	if !errors.Is(err, ErrConsoleQueriesRequired) {
		t.Fatalf("expected ErrConsoleQueriesRequired, got %v", err)
	}
	if auth != nil {
		t.Fatalf("expected nil auth when queries are nil")
	}
}

func TestInitializeAdminAccountPersistsOnFirstRun(t *testing.T) {
	ctx := context.Background()
	db := openTestAuthDB(t)
	defer func() {
		_ = db.Close()
	}()

	result, err := InitializeAdminAccount(ctx, db.Queries, "", "")
	if err != nil {
		t.Fatalf("initialize admin account: %v", err)
	}
	if !result.InitializedNow {
		t.Fatalf("expected initialized now")
	}
	if result.EnvIgnored {
		t.Fatalf("expected env ignored=false")
	}
	if !strings.HasPrefix(result.Username, dashboardUsernamePrefix) {
		t.Fatalf("expected generated username prefix %q, got %q", dashboardUsernamePrefix, result.Username)
	}
	if strings.TrimSpace(result.PasswordPlaintext) == "" {
		t.Fatalf("expected plaintext password in first init result")
	}
	if strings.TrimSpace(result.AccountID) == "" {
		t.Fatalf("expected account_id in init result")
	}

	stored, err := db.Queries.GetAccountByID(ctx, result.AccountID)
	if err != nil {
		t.Fatalf("load persisted account: %v", err)
	}
	if stored.Username != result.Username {
		t.Fatalf("unexpected username: %q", stored.Username)
	}
	if stored.IsAdmin != 1 {
		t.Fatalf("expected is_admin=1, got %d", stored.IsAdmin)
	}
	if !strings.EqualFold(stored.HashAlgo, dashboardPasswordHashAlgo) {
		t.Fatalf("unexpected hash algo: %q", stored.HashAlgo)
	}
	if !compareDashboardPassword(stored.PasswordHash, result.PasswordPlaintext) {
		t.Fatalf("expected stored hash to match initialized password")
	}
}

func TestInitializeAdminAccountLoadsPersistedAndIgnoresEnv(t *testing.T) {
	ctx := context.Background()
	db := openTestAuthDB(t)
	defer func() {
		_ = db.Close()
	}()

	first, err := InitializeAdminAccount(ctx, db.Queries, "admin-first", "password-first")
	if err != nil {
		t.Fatalf("first initialize admin account: %v", err)
	}
	if !first.InitializedNow {
		t.Fatalf("expected first initialization")
	}

	second, err := InitializeAdminAccount(ctx, db.Queries, "admin-second", "password-second")
	if err != nil {
		t.Fatalf("second initialize admin account: %v", err)
	}
	if second.InitializedNow {
		t.Fatalf("expected loading persisted admin account")
	}
	if !second.EnvIgnored {
		t.Fatalf("expected env ignored=true when admin account exists")
	}
	if second.AccountID != first.AccountID {
		t.Fatalf("expected persisted account_id %q, got %q", first.AccountID, second.AccountID)
	}
	if second.Username != first.Username {
		t.Fatalf("expected persisted username %q, got %q", first.Username, second.Username)
	}
	if second.PasswordPlaintext != "" {
		t.Fatalf("expected empty plaintext password for loaded account")
	}
}

func TestInitializeAdminAccountRetriesOnAccountIDConflict(t *testing.T) {
	ctx := context.Background()
	db := openTestAuthDB(t)
	defer func() {
		_ = db.Close()
	}()

	seedTestAccount(t, db.Queries, "acc-conflict", "seed-user", "seed-pass", false)

	previousGenerator := accountIDGenerator
	sequence := []string{"acc-conflict", "acc-retry-success"}
	generateIdx := 0
	accountIDGenerator = func() (string, error) {
		if generateIdx >= len(sequence) {
			return "", errors.New("account id sequence exhausted")
		}
		value := sequence[generateIdx]
		generateIdx++
		return value, nil
	}
	t.Cleanup(func() {
		accountIDGenerator = previousGenerator
	})

	result, err := InitializeAdminAccount(ctx, db.Queries, "admin-retry", "password-retry")
	if err != nil {
		t.Fatalf("initialize admin account with account_id conflict retry: %v", err)
	}
	if !result.InitializedNow {
		t.Fatalf("expected initialized now")
	}
	if result.AccountID != "acc-retry-success" {
		t.Fatalf("expected retried account_id acc-retry-success, got %q", result.AccountID)
	}
}

func TestInitializeAdminAccountReturnsConflictOnUsernameKeyCollision(t *testing.T) {
	ctx := context.Background()
	db := openTestAuthDB(t)
	defer func() {
		_ = db.Close()
	}()

	seedTestAccount(t, db.Queries, "acc-existing", "admin-dup", "seed-pass", false)

	_, err := InitializeAdminAccount(ctx, db.Queries, "ADMIN-dup", "password-new")
	if !errors.Is(err, errAccountRegistrationConflict) {
		t.Fatalf("expected errAccountRegistrationConflict, got %v", err)
	}
}

func TestConsoleAuthLoginLogoutLifecycle(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuth(t)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)

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
		t.Fatalf("expected 200 for authenticated admin list request, got %d body=%s", listRec.Code, listRec.Body.String())
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

func TestConsoleAuthSessionEndpoint(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuthWithRegistration(t, true)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/session", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	payload := accountSessionResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode session payload: %v", err)
	}
	if !payload.Authenticated {
		t.Fatalf("expected authenticated=true")
	}
	if payload.Account.AccountID == "" || payload.Account.Username != testDashboardUsername || !payload.Account.IsAdmin {
		t.Fatalf("unexpected session account payload: %#v", payload.Account)
	}
	if !payload.RegistrationEnabled {
		t.Fatalf("expected registration_enabled=true")
	}
	if payload.ConsoleVersion != consoleVersion() {
		t.Fatalf("expected console_version=%q, got %q", consoleVersion(), payload.ConsoleVersion)
	}
	if payload.ConsoleRepoURL != buildinfo.RepoURL {
		t.Fatalf("expected console_repo_url=%q, got %q", buildinfo.RepoURL, payload.ConsoleRepoURL)
	}
}

func TestConsoleAuthSessionExpires(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuth(t)
	now := time.Unix(1_700_000_000, 0)
	auth.nowFn = func() time.Time {
		return now
	}
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
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

func TestConsoleAuthRegisterAndAdminGuard(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuthWithRegistration(t, true)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	adminCookie := loginSessionCookie(t, router)

	registerBody := []byte(`{"username":"member-a","password":"member-a-pass"}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerReq.AddCookie(adminCookie)
	registerRec := httptest.NewRecorder()
	router.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", registerRec.Code, registerRec.Body.String())
	}

	nonAdminCookie := loginSessionCookieFor(t, router, "member-a", "member-a-pass")

	workersReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	workersReq.AddCookie(nonAdminCookie)
	workersRec := httptest.NewRecorder()
	router.ServeHTTP(workersRec, workersReq)
	if workersRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-admin workers access, got %d body=%s", workersRec.Code, workersRec.Body.String())
	}

	nonAdminRegisterReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", bytes.NewReader(registerBody))
	nonAdminRegisterReq.Header.Set("Content-Type", "application/json")
	nonAdminRegisterReq.AddCookie(nonAdminCookie)
	nonAdminRegisterRec := httptest.NewRecorder()
	router.ServeHTTP(nonAdminRegisterRec, nonAdminRegisterReq)
	if nonAdminRegisterRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin register, got %d body=%s", nonAdminRegisterRec.Code, nonAdminRegisterRec.Body.String())
	}
}

func TestConsoleAuthRegisterDuplicateUsernameConflict(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuthWithRegistration(t, true)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	adminCookie := loginSessionCookie(t, router)

	registerReqA := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", strings.NewReader(`{"username":"member-dup","password":"member-pass"}`))
	registerReqA.Header.Set("Content-Type", "application/json")
	registerReqA.AddCookie(adminCookie)
	registerRecA := httptest.NewRecorder()
	router.ServeHTTP(registerRecA, registerReqA)
	if registerRecA.Code != http.StatusCreated {
		t.Fatalf("expected first register 201, got %d body=%s", registerRecA.Code, registerRecA.Body.String())
	}

	registerReqB := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", strings.NewReader(`{"username":"MEMBER-dup","password":"member-pass-2"}`))
	registerReqB.Header.Set("Content-Type", "application/json")
	registerReqB.AddCookie(adminCookie)
	registerRecB := httptest.NewRecorder()
	router.ServeHTTP(registerRecB, registerReqB)
	if registerRecB.Code != http.StatusConflict {
		t.Fatalf("expected duplicate register 409, got %d body=%s", registerRecB.Code, registerRecB.Body.String())
	}
	if !strings.Contains(registerRecB.Body.String(), errAccountRegistrationConflict.Error()) {
		t.Fatalf("expected duplicate register error message %q, got %s", errAccountRegistrationConflict.Error(), registerRecB.Body.String())
	}
}

func TestConsoleAuthRegisterDisabled(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuthWithRegistration(t, false)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	adminCookie := loginSessionCookie(t, router)

	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", strings.NewReader(`{"username":"member-x","password":"pass"}`))
	registerReq.Header.Set("Content-Type", "application/json")
	registerReq.AddCookie(adminCookie)
	registerRec := httptest.NewRecorder()
	router.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when registration is disabled, got %d body=%s", registerRec.Code, registerRec.Body.String())
	}
}

func TestConsoleAuthChangePasswordLifecycle(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuth(t)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	originalCookie := loginSessionCookie(t, router)

	changeReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/password", strings.NewReader(`{"current_password":"password-test","new_password":"password-next"}`))
	changeReq.Header.Set("Content-Type", "application/json")
	changeReq.AddCookie(originalCookie)
	changeRec := httptest.NewRecorder()
	router.ServeHTTP(changeRec, changeReq)
	if changeRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for password change, got %d body=%s", changeRec.Code, changeRec.Body.String())
	}

	var renewedCookie *http.Cookie
	for _, cookie := range changeRec.Result().Cookies() {
		if cookie.Name == dashboardSessionCookieName {
			renewedCookie = cookie
			break
		}
	}
	if renewedCookie == nil {
		t.Fatalf("expected renewed session cookie after password change")
	}
	if renewedCookie.Value == originalCookie.Value {
		t.Fatalf("expected renewed session id to differ from original")
	}

	oldSessionReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	oldSessionReq.AddCookie(originalCookie)
	oldSessionRec := httptest.NewRecorder()
	router.ServeHTTP(oldSessionRec, oldSessionReq)
	if oldSessionRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected old session to be invalidated, got %d body=%s", oldSessionRec.Code, oldSessionRec.Body.String())
	}

	oldPasswordReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/login", strings.NewReader(`{"username":"admin-test","password":"password-test"}`))
	oldPasswordReq.Header.Set("Content-Type", "application/json")
	oldPasswordRec := httptest.NewRecorder()
	router.ServeHTTP(oldPasswordRec, oldPasswordReq)
	if oldPasswordRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected old password login to fail, got %d body=%s", oldPasswordRec.Code, oldPasswordRec.Body.String())
	}

	newPasswordCookie := loginSessionCookieFor(t, router, "admin-test", "password-next")
	newSessionReq := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	newSessionReq.AddCookie(newPasswordCookie)
	newSessionRec := httptest.NewRecorder()
	router.ServeHTTP(newSessionRec, newSessionReq)
	if newSessionRec.Code != http.StatusOK {
		t.Fatalf("expected new password session to work, got %d body=%s", newSessionRec.Code, newSessionRec.Body.String())
	}
}

func TestConsoleAuthChangePasswordValidationAndCurrentPassword(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuth(t)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	cookie := loginSessionCookie(t, router)

	missingCurrentReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/password", strings.NewReader(`{"new_password":"password-next"}`))
	missingCurrentReq.Header.Set("Content-Type", "application/json")
	missingCurrentReq.AddCookie(cookie)
	missingCurrentRec := httptest.NewRecorder()
	router.ServeHTTP(missingCurrentRec, missingCurrentReq)
	if missingCurrentRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing current password, got %d body=%s", missingCurrentRec.Code, missingCurrentRec.Body.String())
	}
	if !strings.Contains(missingCurrentRec.Body.String(), errAccountCurrentPasswordRequired.Error()) {
		t.Fatalf("expected current password required message, got %s", missingCurrentRec.Body.String())
	}

	invalidCurrentReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/password", strings.NewReader(`{"current_password":"wrong-pass","new_password":"password-next"}`))
	invalidCurrentReq.Header.Set("Content-Type", "application/json")
	invalidCurrentReq.AddCookie(cookie)
	invalidCurrentRec := httptest.NewRecorder()
	router.ServeHTTP(invalidCurrentRec, invalidCurrentReq)
	if invalidCurrentRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid current password, got %d body=%s", invalidCurrentRec.Code, invalidCurrentRec.Body.String())
	}
	if !strings.Contains(invalidCurrentRec.Body.String(), errAccountCurrentPasswordInvalid.Error()) {
		t.Fatalf("expected invalid current password message, got %s", invalidCurrentRec.Body.String())
	}
}

func TestConsoleAuthListAccountsAdminOnlyAndPagination(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth := newTestConsoleAuthWithRegistration(t, true)
	router := mustNewRouter(t, handler, auth, newTestMCPAuth(t), nil)
	adminCookie := loginSessionCookie(t, router)

	for _, username := range []string{"member-a", "member-b"} {
		registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", strings.NewReader(`{"username":"`+username+`","password":"member-pass"}`))
		registerReq.Header.Set("Content-Type", "application/json")
		registerReq.AddCookie(adminCookie)
		registerRec := httptest.NewRecorder()
		router.ServeHTTP(registerRec, registerReq)
		if registerRec.Code != http.StatusCreated {
			t.Fatalf("expected register success for %q, got %d body=%s", username, registerRec.Code, registerRec.Body.String())
		}
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/accounts?page=1&page_size=2", nil)
	listReq.AddCookie(adminCookie)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for account list, got %d body=%s", listRec.Code, listRec.Body.String())
	}

	var payload accountListResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode account list payload: %v", err)
	}
	if payload.Total != 3 {
		t.Fatalf("expected total=3 accounts, got %d", payload.Total)
	}
	if payload.Page != 1 || payload.PageSize != 2 {
		t.Fatalf("unexpected pagination payload: page=%d page_size=%d", payload.Page, payload.PageSize)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("expected 2 items on first page, got %d", len(payload.Items))
	}

	nonAdminCookie := loginSessionCookieFor(t, router, "member-a", "member-pass")
	nonAdminListReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/accounts", nil)
	nonAdminListReq.AddCookie(nonAdminCookie)
	nonAdminListRec := httptest.NewRecorder()
	router.ServeHTTP(nonAdminListRec, nonAdminListReq)
	if nonAdminListRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin account list, got %d body=%s", nonAdminListRec.Code, nonAdminListRec.Body.String())
	}
}

func TestConsoleAuthDeleteAccountGuardsAndCascade(t *testing.T) {
	ctx := context.Background()
	db := openTestAuthDB(t)
	defer func() {
		_ = db.Close()
	}()
	seedTestAccount(t, db.Queries, testDashboardAccountID, testDashboardUsername, testDashboardPassword, true)
	seedTestAccount(t, db.Queries, "acc-admin-second", "admin-second", "admin-second-pass", true)

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	auth, err := NewConsoleAuth(db.Queries, true)
	if err != nil {
		t.Fatalf("new console auth: %v", err)
	}
	mcpAuth, err := NewMCPAuthWithPersistence(db)
	if err != nil {
		t.Fatalf("new mcp auth: %v", err)
	}
	router := mustNewRouter(t, handler, auth, mcpAuth, nil)
	adminCookie := loginSessionCookie(t, router)

	registerReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/register", strings.NewReader(`{"username":"member-to-delete","password":"member-pass"}`))
	registerReq.Header.Set("Content-Type", "application/json")
	registerReq.AddCookie(adminCookie)
	registerRec := httptest.NewRecorder()
	router.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("expected register success, got %d body=%s", registerRec.Code, registerRec.Body.String())
	}
	var registerPayload registerAccountResponse
	if err := json.Unmarshal(registerRec.Body.Bytes(), &registerPayload); err != nil {
		t.Fatalf("decode register payload: %v", err)
	}
	memberID := registerPayload.Account.AccountID
	if strings.TrimSpace(memberID) == "" {
		t.Fatalf("expected member account_id")
	}

	memberToken := "member-token-delete-case"
	if _, _, err := mcpAuth.createToken(ctx, memberID, "member-token", &memberToken); err != nil {
		t.Fatalf("seed member token failed: %v", err)
	}
	memberCookie := loginSessionCookieFor(t, router, "member-to-delete", "member-pass")

	deleteSelfReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/accounts/"+testDashboardAccountID, nil)
	deleteSelfReq.AddCookie(adminCookie)
	deleteSelfRec := httptest.NewRecorder()
	router.ServeHTTP(deleteSelfRec, deleteSelfReq)
	if deleteSelfRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for deleting self, got %d body=%s", deleteSelfRec.Code, deleteSelfRec.Body.String())
	}

	deleteAdminReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/accounts/acc-admin-second", nil)
	deleteAdminReq.AddCookie(adminCookie)
	deleteAdminRec := httptest.NewRecorder()
	router.ServeHTTP(deleteAdminRec, deleteAdminReq)
	if deleteAdminRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for deleting admin account, got %d body=%s", deleteAdminRec.Code, deleteAdminRec.Body.String())
	}

	deleteMissingReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/accounts/acc-missing", nil)
	deleteMissingReq.AddCookie(adminCookie)
	deleteMissingRec := httptest.NewRecorder()
	router.ServeHTTP(deleteMissingRec, deleteMissingReq)
	if deleteMissingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for deleting missing account, got %d body=%s", deleteMissingRec.Code, deleteMissingRec.Body.String())
	}

	deleteMemberReq := httptest.NewRequest(http.MethodDelete, "/api/v1/console/accounts/"+memberID, nil)
	deleteMemberReq.AddCookie(adminCookie)
	deleteMemberRec := httptest.NewRecorder()
	router.ServeHTTP(deleteMemberRec, deleteMemberReq)
	if deleteMemberRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for deleting member account, got %d body=%s", deleteMemberRec.Code, deleteMemberRec.Body.String())
	}

	memberSessionReq := httptest.NewRequest(http.MethodGet, "/api/v1/console/tokens", nil)
	memberSessionReq.AddCookie(memberCookie)
	memberSessionRec := httptest.NewRecorder()
	router.ServeHTTP(memberSessionRec, memberSessionReq)
	if memberSessionRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected deleted member session to be invalidated, got %d body=%s", memberSessionRec.Code, memberSessionRec.Body.String())
	}

	memberLoginReq := httptest.NewRequest(http.MethodPost, "/api/v1/console/login", strings.NewReader(`{"username":"member-to-delete","password":"member-pass"}`))
	memberLoginReq.Header.Set("Content-Type", "application/json")
	memberLoginRec := httptest.NewRecorder()
	router.ServeHTTP(memberLoginRec, memberLoginReq)
	if memberLoginRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected deleted member login to fail, got %d body=%s", memberLoginRec.Code, memberLoginRec.Body.String())
	}

	remainingTokens, err := db.Queries.ListTrustedTokensByAccount(ctx, memberID)
	if err != nil {
		t.Fatalf("list member tokens after delete: %v", err)
	}
	if len(remainingTokens) != 0 {
		t.Fatalf("expected member tokens to be deleted by cascade, got %d", len(remainingTokens))
	}
}

func loginSessionCookieFor(t *testing.T, router http.Handler, username string, password string) *http.Cookie {
	t.Helper()
	body, err := json.Marshal(loginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("marshal login payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected login success for %q, got %d body=%s", username, rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == dashboardSessionCookieName {
			return cookie
		}
	}
	t.Fatalf("expected %s cookie in login response", dashboardSessionCookieName)
	return nil
}

func openTestAuthDB(t *testing.T) *persistence.DB {
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
