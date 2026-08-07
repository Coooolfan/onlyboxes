package grpcserver

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
)

func TestProxySessionResolveAndAuthorize(t *testing.T) {
	now := time.UnixMilli(1_730_000_000_000)
	service := NewRegistryService(nil, nil, 5, 15, time.Minute)
	service.nowFn = func() time.Time { return now }
	service.ConfigureProxy(true, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, []uint16{8091})

	hello := &registryv1.ConnectHello{
		NodeId: "worker-1",
		Labels: map[string]string{
			proxytoken.ProxyEndpointLabel: "10.0.2.15:8091",
		},
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec}},
	}
	session := newActiveSessionAt("worker-1", "worker-connection", hello, now)
	if err := service.configureSessionProxy(session, hello, "worker-secret"); err != nil {
		t.Fatalf("configure session proxy: %v", err)
	}
	service.swapSession(session)
	scopedSessionID := scopeTerminalSessionID("owner-a", "session-a")
	service.bindTerminalSessionRoute(scopedSessionID, "worker-1", now)

	target, err := service.ResolveProxySession("owner-a", "session-a", now)
	if err != nil {
		t.Fatalf("resolve proxy session: %v", err)
	}
	if target.WorkerID != "worker-1" || target.ScopedSessionID != scopedSessionID {
		t.Fatalf("unexpected proxy target %#v", target)
	}

	routeExpiresAt := now.Add(time.Hour)
	authorizeAt := now.Add(10 * time.Second)
	authorization, err := service.AuthorizeProxyRoute("worker-1", scopedSessionID, 8080, routeExpiresAt, authorizeAt)
	if err != nil {
		t.Fatalf("authorize proxy route: %v", err)
	}
	if authorization.Upstream != "10.0.2.15:8091" {
		t.Fatalf("unexpected upstream %q", authorization.Upstream)
	}
	key, err := proxytoken.DeriveKey("worker-secret")
	if err != nil {
		t.Fatalf("derive route key: %v", err)
	}
	claims, err := proxytoken.Verify(key, authorization.Token, authorizeAt)
	if err != nil {
		t.Fatalf("verify route token: %v", err)
	}
	if claims.WorkerID != "worker-1" || claims.SessionID != scopedSessionID || claims.Port != 8080 {
		t.Fatalf("unexpected route token claims %#v", claims)
	}
	if claims.ExpiresAtUnixMs != authorizeAt.Add(proxyRouteTokenTTL).UnixMilli() {
		t.Fatalf("unexpected token expiry %d", claims.ExpiresAtUnixMs)
	}

	route := service.terminalSessionToNode[scopedSessionID]
	if route.LastUsedUnixMs != now.UnixMilli() {
		t.Fatalf("proxy authorization must not renew terminal route")
	}
}

func TestProxyAuthorizationIsBoundedByRouteExpiry(t *testing.T) {
	now := time.UnixMilli(1_730_000_000_000)
	service, scopedSessionID := newProxyRegistryServiceForTest(t, now)
	routeExpiresAt := now.Add(3 * time.Second)
	authorization, err := service.AuthorizeProxyRoute("worker-1", scopedSessionID, 3000, routeExpiresAt, now)
	if err != nil {
		t.Fatalf("authorize proxy route: %v", err)
	}
	key, _ := proxytoken.DeriveKey("worker-secret")
	claims, err := proxytoken.Verify(key, authorization.Token, now)
	if err != nil {
		t.Fatalf("verify route token: %v", err)
	}
	if claims.ExpiresAtUnixMs != routeExpiresAt.UnixMilli() {
		t.Fatalf("token exceeded route expiry: claims=%d route=%d", claims.ExpiresAtUnixMs, routeExpiresAt.UnixMilli())
	}
}

func TestProxySessionRejectsOwnerMismatchAndUnavailableWorker(t *testing.T) {
	now := time.UnixMilli(1_730_000_000_000)
	service, scopedSessionID := newProxyRegistryServiceForTest(t, now)

	if _, err := service.ResolveProxySession("owner-b", "session-a", now); !errors.Is(err, ErrProxySessionNotFound) {
		t.Fatalf("expected owner mismatch rejection, got %v", err)
	}
	if _, err := service.AuthorizeProxyRoute("worker-2", scopedSessionID, 8080, now.Add(time.Minute), now); !errors.Is(err, ErrProxySessionNotFound) {
		t.Fatalf("expected worker mismatch rejection, got %v", err)
	}

	service.removeSession(service.getSession("worker-1"))
	if _, err := service.AuthorizeProxyRoute("worker-1", scopedSessionID, 8080, now.Add(time.Minute), now); !errors.Is(err, ErrProxyWorkerUnavailable) && !errors.Is(err, ErrProxySessionNotFound) {
		t.Fatalf("expected offline worker rejection, got %v", err)
	}
}

func TestNormalizeAllowedProxyEndpoint(t *testing.T) {
	allowed := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "10.2.3.4:8091", want: "10.2.3.4:8091", ok: true},
		{input: "[2001:db8::1]:8091", want: "[2001:db8::1]:8091", ok: true},
		{input: "192.168.1.2:8091"},
		{input: "10.2.3.4:8080"},
		{input: "worker.internal:8091"},
		{input: "10.2.3.4:0"},
	}
	for _, test := range tests {
		got, err := normalizeAllowedProxyEndpoint(test.input, allowed, []uint16{8091})
		if test.ok {
			if err != nil || got != test.want {
				t.Fatalf("normalize %q: want=%q got=%q err=%v", test.input, test.want, got, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("expected endpoint %q to be rejected, got %q", test.input, got)
		}
	}
	for _, input := range []string{"0.0.0.0:8091", "127.0.0.1:8091", "[::ffff:127.0.0.1]:8091", "224.0.0.1:8091"} {
		if got, err := normalizeAllowedProxyEndpoint(input, []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, []uint16{8091}); err == nil {
			t.Fatalf("expected non-unicast endpoint %q to be rejected, got %q", input, got)
		}
	}
}

func newProxyRegistryServiceForTest(t *testing.T, now time.Time) (*RegistryService, string) {
	t.Helper()
	service := NewRegistryService(nil, nil, 5, 15, time.Minute)
	service.nowFn = func() time.Time { return now }
	service.ConfigureProxy(true, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, []uint16{8091})
	hello := &registryv1.ConnectHello{
		NodeId:       "worker-1",
		Labels:       map[string]string{proxytoken.ProxyEndpointLabel: "10.0.2.15:8091"},
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec}},
	}
	session := newActiveSessionAt("worker-1", "worker-connection", hello, now)
	if err := service.configureSessionProxy(session, hello, "worker-secret"); err != nil {
		t.Fatalf("configure session proxy: %v", err)
	}
	service.swapSession(session)
	scopedSessionID := scopeTerminalSessionID("owner-a", "session-a")
	service.bindTerminalSessionRoute(scopedSessionID, "worker-1", now)
	return service, scopedSessionID
}
