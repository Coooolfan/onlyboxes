package grpcserver

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
)

const proxyRouteTokenTTL = 15 * time.Second

var (
	ErrProxySessionNotFound   = errors.New("proxy session not found")
	ErrProxyWorkerUnavailable = errors.New("proxy worker unavailable")
)

type ProxySessionTarget struct {
	WorkerID        string
	ScopedSessionID string
}

type ProxyAuthorization struct {
	Upstream string
	Token    string
}

func (s *RegistryService) ConfigureProxy(enabled bool, allowedWorkerCIDRs []netip.Prefix, allowedWorkerPorts []uint16) {
	if s == nil {
		return
	}
	s.proxyEnabled = enabled
	s.proxyAllowedWorkerCIDRs = append([]netip.Prefix(nil), allowedWorkerCIDRs...)
	s.proxyAllowedWorkerPorts = append([]uint16(nil), allowedWorkerPorts...)
}

func (s *RegistryService) configureSessionProxy(
	session *activeSession,
	hello *registryv1.ConnectHello,
	workerSecret string,
) error {
	if s == nil || session == nil || hello == nil || !s.proxyEnabled {
		return nil
	}

	rawEndpoint := strings.TrimSpace(hello.GetLabels()[proxytoken.ProxyEndpointLabel])
	if rawEndpoint == "" {
		return nil
	}
	endpoint, err := normalizeAllowedProxyEndpoint(rawEndpoint, s.proxyAllowedWorkerCIDRs, s.proxyAllowedWorkerPorts)
	if err != nil {
		return err
	}
	key, err := proxytoken.DeriveKey(workerSecret)
	if err != nil {
		return fmt.Errorf("derive proxy route token key: %w", err)
	}
	session.proxyEndpoint = endpoint
	session.routeTokenKey = append([]byte(nil), key...)
	return nil
}

func normalizeAllowedProxyEndpoint(raw string, allowedCIDRs []netip.Prefix, allowedPorts []uint16) (string, error) {
	host, rawPort, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("proxy endpoint must be an IP address with port")
	}
	address, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil || address.Zone() != "" {
		return "", errors.New("proxy endpoint host must be a unicast IP address")
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return "", errors.New("proxy endpoint host must be a unicast IP address")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("proxy endpoint port must be between 1 and 65535")
	}
	portAllowed := false
	for _, allowedPort := range allowedPorts {
		if int(allowedPort) == port {
			portAllowed = true
			break
		}
	}
	if !portAllowed {
		return "", errors.New("proxy endpoint port is outside the allowed worker ports")
	}

	allowed := false
	for _, prefix := range allowedCIDRs {
		if prefix.Contains(address) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errors.New("proxy endpoint is outside allowed worker CIDRs")
	}
	return net.JoinHostPort(address.String(), strconv.Itoa(port)), nil
}

func (s *RegistryService) ResolveProxySession(ownerID string, externalSessionID string, now time.Time) (ProxySessionTarget, error) {
	if s == nil || !s.proxyEnabled {
		return ProxySessionTarget{}, ErrProxyWorkerUnavailable
	}
	scopedSessionID := scopeTerminalSessionID(ownerID, externalSessionID)
	if strings.TrimSpace(scopedSessionID) == "" {
		return ProxySessionTarget{}, ErrProxySessionNotFound
	}
	workerID, ok := s.proxyTerminalSessionWorker(scopedSessionID, now)
	if !ok {
		return ProxySessionTarget{}, ErrProxySessionNotFound
	}
	session := s.getSession(workerID)
	if !sessionSupportsProxy(session) {
		return ProxySessionTarget{}, ErrProxyWorkerUnavailable
	}
	return ProxySessionTarget{
		WorkerID:        workerID,
		ScopedSessionID: scopedSessionID,
	}, nil
}

func (s *RegistryService) AuthorizeProxyRoute(
	workerID string,
	scopedSessionID string,
	port int,
	routeExpiresAt time.Time,
	now time.Time,
) (ProxyAuthorization, error) {
	if s == nil || !s.proxyEnabled {
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	workerID = strings.TrimSpace(workerID)
	scopedSessionID = strings.TrimSpace(scopedSessionID)
	if workerID == "" || scopedSessionID == "" || port < 1 || port > 65535 {
		return ProxyAuthorization{}, ErrProxySessionNotFound
	}
	if now.IsZero() {
		now = s.nowFn()
	}
	if !routeExpiresAt.After(now) {
		return ProxyAuthorization{}, ErrProxySessionNotFound
	}
	mappedWorkerID, ok := s.proxyTerminalSessionWorker(scopedSessionID, now)
	if !ok || mappedWorkerID != workerID {
		return ProxyAuthorization{}, ErrProxySessionNotFound
	}

	session := s.getSession(workerID)
	if !sessionSupportsProxy(session) {
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	expiresAt := now.Add(proxyRouteTokenTTL)
	if routeExpiresAt.Before(expiresAt) {
		expiresAt = routeExpiresAt
	}
	token, err := proxytoken.Sign(session.routeTokenKey, proxytoken.Claims{
		WorkerID:        workerID,
		SessionID:       scopedSessionID,
		Port:            port,
		ExpiresAtUnixMs: expiresAt.UnixMilli(),
	})
	if err != nil {
		return ProxyAuthorization{}, fmt.Errorf("sign proxy route token: %w", err)
	}
	return ProxyAuthorization{
		Upstream: session.proxyEndpoint,
		Token:    token,
	}, nil
}

func (s *RegistryService) proxyTerminalSessionWorker(scopedSessionID string, now time.Time) (string, bool) {
	if s == nil {
		return "", false
	}
	if now.IsZero() {
		now = s.nowFn()
	}
	s.maybePruneTerminalSessionRoutes(now)

	s.terminalRoutesMu.RLock()
	route, ok := s.terminalSessionToNode[strings.TrimSpace(scopedSessionID)]
	s.terminalRoutesMu.RUnlock()
	if !ok || route.ReservationID != 0 || strings.TrimSpace(route.NodeID) == "" {
		return "", false
	}
	if s.terminalRouteTTL > 0 && now.UnixMilli()-route.LastUsedUnixMs >= s.terminalRouteTTL.Milliseconds() {
		return "", false
	}
	return route.NodeID, true
}

func sessionSupportsProxy(session *activeSession) bool {
	return session != nil &&
		session.hasCapability(taskCapabilityTerminalExec) &&
		strings.TrimSpace(session.proxyEndpoint) != "" &&
		len(session.routeTokenKey) > 0
}
