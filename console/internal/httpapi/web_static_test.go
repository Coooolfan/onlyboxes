package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

func newWebStaticTestRouter(t *testing.T) http.Handler {
	t.Helper()

	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	return mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)
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

func TestWebStaticRouteServesWorkerStartupScriptFromProvidedWebFS(t *testing.T) {
	router := gin.New()
	registerWebRoutes(router, fstest.MapFS{
		"index.html":               &fstest.MapFile{Data: []byte("<!doctype html>")},
		"static/worker-startup.sh": &fstest.MapFile{Data: []byte("#!/usr/bin/env bash\necho worker-startup\n")},
	})

	req := httptest.NewRequest(http.MethodGet, "/static/worker-startup.sh", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worker-startup") {
		t.Fatalf("expected worker startup script body, got %q", rec.Body.String())
	}
}

func TestEmbeddedWebUnknownRouteReturnsNotFound(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
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

func TestEmbeddedWebFallbackDoesNotInterceptUppercaseAPI(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/API/v1/workers", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmbeddedWebFallbackDoesNotInterceptMCP(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(trustedTokenHeader, "Bearer "+testMCPToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmbeddedWebFallbackDoesNotInterceptUppercaseMCP(t *testing.T) {
	router := newWebStaticTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/MCP", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRouterKeepsRedirectFixedPathDisabled(t *testing.T) {
	handler := NewWorkerHandler(registrytest.NewStore(t), 15*time.Second, nil, nil, nil, "")
	router := mustNewRouter(t, handler, newTestConsoleAuth(t), newTestMCPAuth(t), nil)

	if router.RedirectFixedPath {
		t.Fatalf("expected RedirectFixedPath to remain false")
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
