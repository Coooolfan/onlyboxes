package httpapi

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
)

const (
	testProxyRouteMaxPerAccount = 16
	testProxyRouteMaxPerSession = 2
	testProxyRouteKeyLength     = 26
)

type proxyRouteResolverStub struct {
	target              grpcserver.ProxySessionTarget
	resolveErr          error
	authorization       grpcserver.ProxyAuthorization
	authorizeErr        error
	resolvedOwnerID     string
	authorizedWorkerID  string
	authorizedSessionID string
	authorizedPort      int
}

func (s *proxyRouteResolverStub) ResolveProxySession(ownerID string, _ string, _ time.Time) (grpcserver.ProxySessionTarget, error) {
	s.resolvedOwnerID = ownerID
	return s.target, s.resolveErr
}

func (s *proxyRouteResolverStub) AuthorizeProxyRoute(_ context.Context, workerID string, scopedSessionID string, port int, _ time.Time, _ time.Time) (grpcserver.ProxyAuthorization, error) {
	s.authorizedWorkerID = workerID
	s.authorizedSessionID = scopedSessionID
	s.authorizedPort = port
	return s.authorization, s.authorizeErr
}

func TestProxyRouteManagementOwnerIsolation(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{
			WorkerID:        "worker-1",
			ScopedSessionID: "obx:owner-a:session-a",
		},
	}
	handler := newProxyRouteHandlerForTest(t, resolver, now, time.Hour)
	router := newProxyRouteTestRouter(handler)

	created := createProxyRouteForTest(t, router, "owner-a", `{"session_id":"session-a","port":8080}`)
	if resolver.resolvedOwnerID != "owner-a" {
		t.Fatalf("resolver received wrong owner %q", resolver.resolvedOwnerID)
	}
	if created.RouteKey == "" || created.SessionID != "session-a" || created.Port != 8080 {
		t.Fatalf("unexpected create response %#v", created)
	}
	if created.URL != "https://"+created.RouteKey+".public-preview.example.com" {
		t.Fatalf("unexpected public URL %q", created.URL)
	}

	ownerAList := listProxyRoutesForTest(t, router, "owner-a")
	if ownerAList.Total != 1 || len(ownerAList.Items) != 1 {
		t.Fatalf("owner A expected one route, got %#v", ownerAList)
	}
	ownerBList := listProxyRoutesForTest(t, router, "owner-b")
	if ownerBList.Total != 0 || len(ownerBList.Items) != 0 {
		t.Fatalf("owner B saw another owner's route: %#v", ownerBList)
	}

	otherDelete := httptest.NewRequest(http.MethodDelete, "/api/v1/proxy-routes/"+created.RouteKey, nil)
	otherDelete.Header.Set("X-Test-Owner", "owner-b")
	otherDeleteResponse := httptest.NewRecorder()
	router.ServeHTTP(otherDeleteResponse, otherDelete)
	if otherDeleteResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-owner delete expected 404, got %d", otherDeleteResponse.Code)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/proxy-routes/"+created.RouteKey, nil)
	deleteRequest.Header.Set("X-Test-Owner", "owner-a")
	deleteResponse := httptest.NewRecorder()
	router.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("owner delete expected 204, got %d", deleteResponse.Code)
	}

	resolveDeleted := httptest.NewRequest(http.MethodGet, "/internal/v1/proxy/resolve", nil)
	resolveDeleted.Header.Set(proxyInternalAuthHeader, "nginx-secret")
	resolveDeleted.Header.Set(proxyOriginalHostHeader, created.RouteKey+".public-preview.example.com")
	resolveDeletedResponse := httptest.NewRecorder()
	router.ServeHTTP(resolveDeletedResponse, resolveDeleted)
	if resolveDeletedResponse.Code != http.StatusForbidden {
		t.Fatalf("deleted route resolve expected 403, got %d", resolveDeletedResponse.Code)
	}
}

func TestProxyRouteConfiguredHTTPURL(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{
			WorkerID:        "worker-1",
			ScopedSessionID: "obx:owner-a:session-a",
		},
	}
	handler, err := NewProxyRouteHandler(resolver, registrytest.NewStore(t), "public-preview.localhost", "http", "nginx-secret", time.Hour, proxyRouteKeyMinLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession)
	if err != nil {
		t.Fatalf("new localhost proxy route handler: %v", err)
	}
	handler.nowFn = func() time.Time { return now }
	handler.randomReader = bytes.NewReader(bytes.Repeat([]byte{0x11}, proxyRouteKeyBytes*proxyRouteCreateAttempts))

	created := createProxyRouteForTest(t, newProxyRouteTestRouter(handler), "owner-a", `{"session_id":"session-a","port":8080}`)
	if len(created.RouteKey) != proxyRouteKeyMinLength {
		t.Fatalf("unexpected configured route key length %d", len(created.RouteKey))
	}
	wantURL := "http://" + created.RouteKey + ".public-preview.localhost"
	if created.URL != wantURL {
		t.Fatalf("unexpected localhost public URL %q, want %q", created.URL, wantURL)
	}
}

func TestProxyRouteRestorePreservesURLAndDelete(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := registrytest.NewStore(t)
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{
			WorkerID:        "worker-1",
			ScopedSessionID: "obx:owner-a:session-a",
		},
		authorization: grpcserver.ProxyAuthorization{Upstream: "http://10.0.0.2:8091", Token: "route-token"},
	}
	first := newProxyRouteHandlerWithStoreForTest(t, resolver, store, now, time.Hour)
	created := createProxyRouteForTest(t, newProxyRouteTestRouter(first), "owner-a", `{"session_id":"session-a","port":8080}`)

	restoredResolver := &proxyRouteResolverStub{
		authorization: grpcserver.ProxyAuthorization{Upstream: "http://10.0.0.2:8091", Token: "new-route-token"},
	}
	restored := newProxyRouteHandlerWithStoreForTest(t, restoredResolver, store, now.Add(time.Minute), time.Hour)
	restoredRouter := newProxyRouteTestRouter(restored)
	listed := listProxyRoutesForTest(t, restoredRouter, "owner-a")
	if listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].RouteKey != created.RouteKey || listed.Items[0].URL != created.URL {
		t.Fatalf("restored route changed: created=%#v listed=%#v", created, listed)
	}

	resolve := httptest.NewRequest(http.MethodGet, "/internal/v1/proxy/resolve", nil)
	resolve.Header.Set(proxyInternalAuthHeader, "nginx-secret")
	resolve.Header.Set(proxyOriginalHostHeader, created.RouteKey+".public-preview.example.com")
	resolveResponse := httptest.NewRecorder()
	restoredRouter.ServeHTTP(resolveResponse, resolve)
	if resolveResponse.Code != http.StatusNoContent || restoredResolver.authorizedWorkerID != "worker-1" || restoredResolver.authorizedSessionID != "obx:owner-a:session-a" {
		t.Fatalf("restored route did not resolve: code=%d worker=%q session=%q", resolveResponse.Code, restoredResolver.authorizedWorkerID, restoredResolver.authorizedSessionID)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/proxy-routes/"+created.RouteKey, nil)
	deleteRequest.Header.Set("X-Test-Owner", "owner-a")
	deleteResponse := httptest.NewRecorder()
	restoredRouter.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete restored route expected 204, got %d", deleteResponse.Code)
	}

	afterDelete := newProxyRouteHandlerWithStoreForTest(t, restoredResolver, store, now.Add(2*time.Minute), time.Hour)
	if listed := listProxyRoutesForTest(t, newProxyRouteTestRouter(afterDelete), "owner-a"); listed.Total != 0 {
		t.Fatalf("deleted route was restored: %#v", listed)
	}
}

func TestProxyRouteRestoreDeletesExpiredRows(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := registrytest.NewStore(t)
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{
			WorkerID:        "worker-1",
			ScopedSessionID: "obx:owner-a:session-a",
		},
	}
	first := newProxyRouteHandlerWithStoreForTest(t, resolver, store, now, time.Hour)
	createProxyRouteForTest(t, newProxyRouteTestRouter(first), "owner-a", `{"session_id":"session-a","port":8080}`)

	restored := newProxyRouteHandlerWithStoreForTest(t, resolver, store, now.Add(2*time.Hour), time.Hour)
	if listed := listProxyRoutesForTest(t, newProxyRouteTestRouter(restored), "owner-a"); listed.Total != 0 {
		t.Fatalf("expired route was restored: %#v", listed)
	}
	persisted, err := store.LoadActiveProxyRoutes(context.Background(), 0)
	if err != nil {
		t.Fatalf("load persisted proxy routes: %v", err)
	}
	if len(persisted) != 0 {
		t.Fatalf("expired proxy route row was not deleted: %#v", persisted)
	}
}

func TestProxyRoutePruneDeletesExpiredRowsAndMemory(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := registrytest.NewStore(t)
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{
			WorkerID:        "worker-1",
			ScopedSessionID: "obx:owner-a:session-a",
		},
	}
	handler := newProxyRouteHandlerWithStoreForTest(t, resolver, store, now, time.Hour)
	created := createProxyRouteForTest(t, newProxyRouteTestRouter(handler), "owner-a", `{"session_id":"session-a","port":8080}`)

	removed, err := handler.PruneExpired(context.Background(), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("prune expired proxy routes: %v", err)
	}
	if removed != 1 {
		t.Fatalf("pruned routes=%d, want 1", removed)
	}
	if _, exists := handler.routes[created.RouteKey]; exists {
		t.Fatal("expired route remained in memory")
	}
	persisted, err := store.LoadActiveProxyRoutes(context.Background(), 0)
	if err != nil {
		t.Fatalf("load persisted proxy routes: %v", err)
	}
	if len(persisted) != 0 {
		t.Fatalf("expired proxy route row was not pruned: %#v", persisted)
	}
}

func TestProxyRoutePersistenceFailuresDoNotChangeMemory(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	newResolver := func() *proxyRouteResolverStub {
		return &proxyRouteResolverStub{
			target: grpcserver.ProxySessionTarget{
				WorkerID:        "worker-1",
				ScopedSessionID: "obx:owner-a:session-a",
			},
		}
	}

	t.Run("create", func(t *testing.T) {
		store := registrytest.NewStore(t)
		handler := newProxyRouteHandlerWithStoreForTest(t, newResolver(), store, now, time.Hour)
		if err := store.Persistence().Close(); err != nil {
			t.Fatalf("close persistence: %v", err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(`{"session_id":"session-a","port":8080}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Test-Owner", "owner-a")
		response := httptest.NewRecorder()
		newProxyRouteTestRouter(handler).ServeHTTP(response, request)
		if response.Code != http.StatusInternalServerError || len(handler.routes) != 0 {
			t.Fatalf("failed persistence changed memory: code=%d routes=%#v", response.Code, handler.routes)
		}
	})

	t.Run("delete", func(t *testing.T) {
		store := registrytest.NewStore(t)
		handler := newProxyRouteHandlerWithStoreForTest(t, newResolver(), store, now, time.Hour)
		router := newProxyRouteTestRouter(handler)
		created := createProxyRouteForTest(t, router, "owner-a", `{"session_id":"session-a","port":8080}`)
		if err := store.Persistence().Close(); err != nil {
			t.Fatalf("close persistence: %v", err)
		}
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/proxy-routes/"+created.RouteKey, nil)
		request.Header.Set("X-Test-Owner", "owner-a")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("delete with failed persistence expected 500, got %d", response.Code)
		}
		if _, exists := handler.routes[created.RouteKey]; !exists {
			t.Fatal("failed persistence removed route from memory")
		}
	})
}

func TestProxyRouteManagementRequiresAuthentication(t *testing.T) {
	handler := newProxyRouteHandlerForTest(t, &proxyRouteResolverStub{}, time.Now(), time.Hour)
	router := newProxyRouteTestRouter(handler)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(`{"session_id":"session","port":8080}`)),
		httptest.NewRequest(http.MethodGet, "/api/v1/proxy-routes", nil),
		httptest.NewRequest(http.MethodDelete, "/api/v1/proxy-routes/ceirceirceirceirceirceirce", nil),
	} {
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s expected 401, got %d", request.Method, request.URL.Path, response.Code)
		}
	}
}

func TestProxyRoutePerOwnerLimit(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	resolver := &proxyRouteResolverStub{target: grpcserver.ProxySessionTarget{WorkerID: "worker-1", ScopedSessionID: "scoped-session"}}
	handler := newProxyRouteHandlerForTest(t, resolver, now, time.Hour)
	for index := 0; index < testProxyRouteMaxPerAccount; index++ {
		routeKey := "route-" + strconv.Itoa(index)
		handler.routes[routeKey] = proxyRouteRecord{RouteKey: routeKey, OwnerID: "owner-a", ExpiresAt: now.Add(time.Hour)}
	}
	router := newProxyRouteTestRouter(handler)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(`{"session_id":"session","port":8080}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Owner", "owner-a")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("route limit expected 429, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestProxyRoutePerSessionLimit(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	resolver := &proxyRouteResolverStub{target: grpcserver.ProxySessionTarget{WorkerID: "worker-1", ScopedSessionID: "scoped-session"}}
	handler := newProxyRouteHandlerForTest(t, resolver, now, time.Hour)
	for index := 0; index < testProxyRouteMaxPerSession; index++ {
		routeKey := "route-" + strconv.Itoa(index)
		handler.routes[routeKey] = proxyRouteRecord{
			RouteKey:  routeKey,
			OwnerID:   "owner-a",
			SessionID: "session-a",
			ExpiresAt: now.Add(time.Hour),
		}
	}

	router := newProxyRouteTestRouter(handler)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(`{"session_id":"session-a","port":8080}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Owner", "owner-a")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "session proxy route limit reached") {
		t.Fatalf("session route limit expected 429, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestGenerateProxyRouteKeyUsesConfiguredLength(t *testing.T) {
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	routeKey, err := generateProxyRouteKey(bytes.NewReader(raw), proxyRouteKeyMaxLength)
	if err != nil {
		t.Fatalf("generate route key: %v", err)
	}
	if !validProxyRouteKey(routeKey) || routeKey != strings.ToLower(routeKey) {
		t.Fatalf("invalid DNS-safe route key %q", routeKey)
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(routeKey))
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("route key did not preserve 128 random bits: decoded=%x err=%v", decoded, err)
	}
	shortRouteKey, err := generateProxyRouteKey(bytes.NewReader(raw), proxyRouteKeyMinLength)
	if err != nil {
		t.Fatalf("generate short route key: %v", err)
	}
	if len(shortRouteKey) != proxyRouteKeyMinLength || shortRouteKey != routeKey[:proxyRouteKeyMinLength] || !validProxyRouteKey(shortRouteKey) {
		t.Fatalf("unexpected short route key %q", shortRouteKey)
	}
	for _, length := range []int{proxyRouteKeyMinLength - 1, proxyRouteKeyMaxLength + 1} {
		if _, err := generateProxyRouteKey(bytes.NewReader(raw), length); err == nil {
			t.Fatalf("expected route key length %d to fail", length)
		}
	}
}

func TestProxyRouteCreateValidationAndResolverErrors(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{WorkerID: "worker-1", ScopedSessionID: "scoped-session"},
	}
	handler := newProxyRouteHandlerForTest(t, resolver, now, time.Hour)
	router := newProxyRouteTestRouter(handler)

	for _, test := range []struct {
		body string
		want int
	}{
		{body: `{}`, want: http.StatusBadRequest},
		{body: `{"session_id":"session","port":0}`, want: http.StatusBadRequest},
		{body: `{"session_id":"session","port":65536}`, want: http.StatusBadRequest},
		{body: `{`, want: http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Test-Owner", "owner-a")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("body %q: expected %d, got %d", test.body, test.want, response.Code)
		}
	}

	resolver.resolveErr = grpcserver.ErrProxySessionNotFound
	request := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(`{"session_id":"missing","port":8080}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Owner", "owner-a")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing session expected 404, got %d", response.Code)
	}

	resolver.resolveErr = grpcserver.ErrProxyWorkerUnavailable
	unavailable := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(`{"session_id":"session","port":8080}`))
	unavailable.Header.Set("Content-Type", "application/json")
	unavailable.Header.Set("X-Test-Owner", "owner-a")
	unavailableResponse := httptest.NewRecorder()
	router.ServeHTTP(unavailableResponse, unavailable)
	if unavailableResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable worker expected 503, got %d", unavailableResponse.Code)
	}
}

func TestProxyRouteInternalResolve(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	currentTime := now
	resolver := &proxyRouteResolverStub{
		target: grpcserver.ProxySessionTarget{
			WorkerID:        "worker-1",
			ScopedSessionID: "obx:owner-a:session-a",
		},
		authorization: grpcserver.ProxyAuthorization{
			Upstream:     "https://3000-sandbox.e2b.app",
			UpstreamHost: "3000-sandbox.e2b.app",
			TrafficToken: "traffic-secret",
		},
	}
	handler := newProxyRouteHandlerForTest(t, resolver, now, time.Minute)
	handler.nowFn = func() time.Time { return currentTime }
	router := newProxyRouteTestRouter(handler)
	created := createProxyRouteForTest(t, router, "owner-a", `{"session_id":"session-a","port":3000}`)
	host := created.RouteKey + ".public-preview.example.com"

	missingAuth := httptest.NewRequest(http.MethodGet, "/internal/v1/proxy/resolve", nil)
	missingAuth.Header.Set(proxyOriginalHostHeader, host)
	missingAuthResponse := httptest.NewRecorder()
	router.ServeHTTP(missingAuthResponse, missingAuth)
	if missingAuthResponse.Code != http.StatusUnauthorized {
		t.Fatalf("missing internal auth expected 401, got %d", missingAuthResponse.Code)
	}

	spoofedHost := httptest.NewRequest(http.MethodGet, "/internal/v1/proxy/resolve", nil)
	spoofedHost.Header.Set(proxyInternalAuthHeader, "nginx-secret")
	spoofedHost.Header.Set(proxyOriginalHostHeader, created.RouteKey+".attacker.example")
	spoofedHostResponse := httptest.NewRecorder()
	router.ServeHTTP(spoofedHostResponse, spoofedHost)
	if spoofedHostResponse.Code != http.StatusForbidden {
		t.Fatalf("spoofed host expected 403, got %d", spoofedHostResponse.Code)
	}

	resolve := httptest.NewRequest(http.MethodGet, "/internal/v1/proxy/resolve", nil)
	resolve.Header.Set(proxyInternalAuthHeader, "nginx-secret")
	resolve.Header.Set(proxyOriginalHostHeader, host)
	resolveResponse := httptest.NewRecorder()
	router.ServeHTTP(resolveResponse, resolve)
	if resolveResponse.Code != http.StatusNoContent {
		t.Fatalf("resolve expected 204, got %d body=%s", resolveResponse.Code, resolveResponse.Body.String())
	}
	if resolveResponse.Header().Get(proxyUpstreamHeader) != resolver.authorization.Upstream ||
		resolveResponse.Header().Get(proxyUpstreamHostHeader) != resolver.authorization.UpstreamHost ||
		resolveResponse.Header().Get(proxyUpstreamTrafficTokenHeader) != resolver.authorization.TrafficToken ||
		resolveResponse.Header().Get(proxytoken.HeaderName) != "" {
		t.Fatalf("unexpected resolve headers %#v", resolveResponse.Header())
	}
	if resolver.authorizedWorkerID != "worker-1" || resolver.authorizedSessionID != "obx:owner-a:session-a" || resolver.authorizedPort != 3000 {
		t.Fatalf("unexpected authorization input: %#v", resolver)
	}

	currentTime = now.Add(time.Minute)
	expired := httptest.NewRequest(http.MethodGet, "/internal/v1/proxy/resolve", nil)
	expired.Header.Set(proxyInternalAuthHeader, "nginx-secret")
	expired.Header.Set(proxyOriginalHostHeader, host)
	expiredResponse := httptest.NewRecorder()
	router.ServeHTTP(expiredResponse, expired)
	if expiredResponse.Code != http.StatusForbidden {
		t.Fatalf("expired route expected 403, got %d", expiredResponse.Code)
	}
}

func TestNewProxyRouteHandlerRejectsInvalidConfiguration(t *testing.T) {
	resolver := &proxyRouteResolverStub{}
	store := registrytest.NewStore(t)
	if _, err := NewProxyRouteHandler(resolver, nil, "preview.example.com", "https", "secret", time.Hour, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
		t.Fatalf("expected missing proxy route store to fail")
	}
	tooLongDomain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 35)
	for _, domain := range []string{"", "localhost", "https://preview.example.com", "*.preview.example.com", "-bad.example.com", tooLongDomain} {
		if _, err := NewProxyRouteHandler(resolver, store, domain, "https", "secret", time.Hour, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
			t.Fatalf("expected invalid domain %q to fail", domain)
		}
	}
	for _, scheme := range []string{"", "ftp"} {
		if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", scheme, "secret", time.Hour, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
			t.Fatalf("expected invalid public scheme %q to fail", scheme)
		}
	}
	if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", "https", "", time.Hour, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
		t.Fatalf("expected missing internal token to fail")
	}
	if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", "https", "secret", 0, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
		t.Fatalf("expected invalid route TTL to fail")
	}
	if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", "https", "secret", proxyRouteMaxTTL+time.Second, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
		t.Fatalf("expected route TTL above maximum to fail")
	}
	if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", "https", "secret", time.Hour, testProxyRouteKeyLength, 0, testProxyRouteMaxPerSession); err == nil {
		t.Fatalf("expected invalid account route limit to fail")
	}
	if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", "https", "secret", time.Hour, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, 0); err == nil {
		t.Fatalf("expected invalid session route limit to fail")
	}
	for _, length := range []int{proxyRouteKeyMinLength - 1, proxyRouteKeyMaxLength + 1} {
		if _, err := NewProxyRouteHandler(resolver, store, "preview.example.com", "https", "secret", time.Hour, length, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession); err == nil {
			t.Fatalf("expected invalid route key length %d to fail", length)
		}
	}
}

func newProxyRouteHandlerForTest(t *testing.T, resolver ProxyRouteResolver, now time.Time, ttl time.Duration) *ProxyRouteHandler {
	t.Helper()
	return newProxyRouteHandlerWithStoreForTest(t, resolver, registrytest.NewStore(t), now, ttl)
}

func newProxyRouteHandlerWithStoreForTest(t *testing.T, resolver ProxyRouteResolver, store proxyRouteStore, now time.Time, ttl time.Duration) *ProxyRouteHandler {
	t.Helper()
	handler, err := NewProxyRouteHandler(resolver, store, "public-preview.example.com", "https", "nginx-secret", ttl, testProxyRouteKeyLength, testProxyRouteMaxPerAccount, testProxyRouteMaxPerSession)
	if err != nil {
		t.Fatalf("new proxy route handler: %v", err)
	}
	handler.nowFn = func() time.Time { return now }
	handler.randomReader = bytes.NewReader(bytes.Repeat([]byte{0x11}, proxyRouteKeyBytes*proxyRouteCreateAttempts))
	if err := handler.Restore(context.Background(), now); err != nil {
		t.Fatalf("restore proxy routes: %v", err)
	}
	return handler
}

func newProxyRouteTestRouter(handler *ProxyRouteHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if ownerID := strings.TrimSpace(c.GetHeader("X-Test-Owner")); ownerID != "" {
			setRequestSessionAccount(c, SessionAccount{AccountID: ownerID, Username: ownerID})
		}
		c.Next()
	})
	router.POST("/api/v1/proxy-routes", handler.Create)
	router.GET("/api/v1/proxy-routes", handler.List)
	router.DELETE("/api/v1/proxy-routes/:route_key", handler.Delete)
	router.GET("/internal/v1/proxy/resolve", handler.Resolve)
	return router
}

func createProxyRouteForTest(t *testing.T, router http.Handler, ownerID string, body string) proxyRouteResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/proxy-routes", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Owner", ownerID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create route expected 201, got %d body=%s", response.Code, response.Body.String())
	}
	created := proxyRouteResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return created
}

func listProxyRoutesForTest(t *testing.T, router http.Handler, ownerID string) listProxyRoutesResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/proxy-routes", nil)
	request.Header.Set("X-Test-Owner", ownerID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list routes expected 200, got %d body=%s", response.Code, response.Body.String())
	}
	listed := listProxyRoutesResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	return listed
}
