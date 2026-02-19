package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func newWebStaticTestRouter(t *testing.T) http.Handler {
	t.Helper()

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, "")
	return NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
}

func TestEmbeddedWebRootServesIndex(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("expected text/html content type, got %q", contentType)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "<!doctype html") {
		t.Fatalf("expected embedded index html body, got %q", rec.Body.String())
	}
}

func TestEmbeddedWebSPAFallbackServesIndex(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "<!doctype html") {
		t.Fatalf("expected fallback index html body, got %q", rec.Body.String())
	}
}

func TestEmbeddedWebFallbackDoesNotInterceptAPI(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmbeddedWebFallbackDoesNotInterceptMCP(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(trustedTokenHeader, testMCPToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmbeddedWebFallbackRejectsNonGETMethods(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}
