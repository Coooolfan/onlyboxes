package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/registry"
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

func TestConsoleAuthLoginLogoutLifecycle(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil)
	auth := newTestConsoleAuth(t)
	router := NewRouter(handler, auth)

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
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil)
	auth := newTestConsoleAuth(t)
	now := time.Unix(1_700_000_000, 0)
	auth.nowFn = func() time.Time {
		return now
	}
	router := NewRouter(handler, auth)
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
