package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
)

const proxyRouteTokenTTL = 15 * time.Second

const proxyDirectResolveTimeout = 5 * time.Second

var (
	ErrProxySessionNotFound   = errors.New("proxy session not found")
	ErrProxyWorkerUnavailable = errors.New("proxy worker unavailable")
)

type ProxySessionTarget struct {
	WorkerID        string
	ScopedSessionID string
}

type ProxyAuthorization struct {
	Upstream     string
	UpstreamHost string
	Token        string
	TrafficToken string
}

func (s *RegistryService) ConfigureProxy(enabled bool, allowedWorkerCIDRs []netip.Prefix, allowedWorkerPorts []uint16, allowedDirectDomains []string) {
	if s == nil {
		return
	}
	s.proxyEnabled = enabled
	s.proxyAllowedWorkerCIDRs = append([]netip.Prefix(nil), allowedWorkerCIDRs...)
	s.proxyAllowedWorkerPorts = append([]uint16(nil), allowedWorkerPorts...)
	s.proxyAllowedDirectDomains = append([]string(nil), allowedDirectDomains...)
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
	rawDirect := strings.ToLower(strings.TrimSpace(hello.GetLabels()[proxytoken.ProxyDirectLabel]))
	if rawEndpoint != "" && rawDirect != "" {
		return errors.New("worker cannot advertise both proxy endpoint and direct proxy mode")
	}
	if rawDirect != "" {
		if rawDirect != proxytoken.ProxyDirectE2B {
			return errors.New("unsupported direct proxy mode")
		}
		if !session.hasCapability(taskCapabilityTerminalProxy) {
			return errors.New("direct proxy worker must declare terminalProxy capability")
		}
		session.proxyDirect = rawDirect
		return nil
	}
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
	ctx context.Context,
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
	if session.proxyDirect != "" {
		return s.authorizeDirectProxyRoute(ctx, session, scopedSessionID, port)
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
		Upstream: "http://" + session.proxyEndpoint,
		Token:    token,
	}, nil
}

type directProxyResolvePayload struct {
	SessionID string `json:"session_id"`
	Port      int    `json:"port"`
}

type directProxyResolveResult struct {
	URL          string `json:"url"`
	TrafficToken string `json:"traffic_token,omitempty"`
}

func (s *RegistryService) authorizeDirectProxyRoute(ctx context.Context, session *activeSession, scopedSessionID string, port int) (ProxyAuthorization, error) {
	payload, err := json.Marshal(directProxyResolvePayload{SessionID: scopedSessionID, Port: port})
	if err != nil {
		return ProxyAuthorization{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, proxyDirectResolveTimeout)
	defer cancel()

	picked, err := s.pickSessionForNodeAndCapability(session.nodeID, taskCapabilityTerminalProxy)
	if err != nil {
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	attempt, err := s.dispatchCommandAttempt(commandCtx, taskCapabilityTerminalProxy, payload, "", picked, 0, nil)
	if err != nil {
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	if attempt.outcome.err != nil {
		if isSessionNotFoundCommandError(attempt.outcome.err) {
			return ProxyAuthorization{}, ErrProxySessionNotFound
		}
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	var resolved directProxyResolveResult
	if err := json.Unmarshal(attempt.outcome.payloadJSON, &resolved); err != nil {
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	upstream := strings.TrimSpace(resolved.URL)
	parsed, err := validateDirectProxyURL(upstream, s.proxyAllowedDirectDomains)
	if err != nil {
		return ProxyAuthorization{}, ErrProxyWorkerUnavailable
	}
	return ProxyAuthorization{Upstream: upstream, UpstreamHost: parsed.Host, TrafficToken: strings.TrimSpace(resolved.TrafficToken)}, nil
}

func validateDirectProxyURL(raw string, allowedDomains []string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("direct proxy URL must be an HTTPS origin")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("direct proxy URL must not contain a path")
	}
	hostname := strings.ToLower(parsed.Hostname())
	allowed := false
	for _, domain := range allowedDomains {
		domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if domain != "" && strings.HasSuffix(hostname, "."+domain) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("direct proxy URL is outside allowed domains")
	}
	return parsed, nil
}

func (s *RegistryService) proxyTerminalSessionWorker(scopedSessionID string, now time.Time) (string, bool) {
	if s == nil {
		return "", false
	}
	if now.IsZero() {
		now = s.nowFn()
	}
	s.maybePruneTerminalSessionRoutes(now)

	route, ok := s.terminalSessionRouteSnapshot(scopedSessionID, now)
	if !ok || route.ReservationID != 0 || route.RecoveryState != terminalSessionRecoveryReady || strings.TrimSpace(route.NodeID) == "" {
		return "", false
	}
	return route.NodeID, true
}

func sessionSupportsProxy(session *activeSession) bool {
	return session != nil &&
		session.hasCapability(taskCapabilityTerminalExec) &&
		((strings.TrimSpace(session.proxyEndpoint) != "" && len(session.routeTokenKey) > 0) ||
			(strings.TrimSpace(session.proxyDirect) != "" && session.hasCapability(taskCapabilityTerminalProxy)))
}
