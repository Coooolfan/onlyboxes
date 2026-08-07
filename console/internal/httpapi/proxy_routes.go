package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
)

const (
	proxyInternalAuthHeader  = "X-Onlyboxes-Internal-Token"
	proxyOriginalHostHeader  = "X-Original-Host"
	proxyUpstreamHeader      = "X-Onlyboxes-Upstream"
	proxyRouteKeyBytes       = 16
	proxyRouteKeyLength      = 26
	proxyBaseDomainMaxBytes  = 253 - 1 - proxyRouteKeyLength
	proxyRouteCreateAttempts = 8
	proxyRouteMaxPerOwner    = 100
	proxyRouteMaxTTL         = 7 * 24 * time.Hour
)

type ProxyRouteResolver interface {
	ResolveProxySession(ownerID string, externalSessionID string, now time.Time) (grpcserver.ProxySessionTarget, error)
	AuthorizeProxyRoute(workerID string, scopedSessionID string, port int, routeExpiresAt time.Time, now time.Time) (grpcserver.ProxyAuthorization, error)
}

type proxyRouteRecord struct {
	RouteKey        string
	OwnerID         string
	SessionID       string
	ScopedSessionID string
	Port            int
	WorkerID        string
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

type ProxyRouteHandler struct {
	resolver      ProxyRouteResolver
	baseDomain    string
	internalToken string
	routeTTL      time.Duration
	nowFn         func() time.Time
	randomReader  io.Reader

	mu     sync.RWMutex
	routes map[string]proxyRouteRecord
}

type createProxyRouteRequest struct {
	SessionID string `json:"session_id"`
	Port      int    `json:"port"`
}

type proxyRouteResponse struct {
	RouteKey  string    `json:"route_key"`
	SessionID string    `json:"session_id"`
	Port      int       `json:"port"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type listProxyRoutesResponse struct {
	Items []proxyRouteResponse `json:"items"`
	Total int                  `json:"total"`
}

func NewProxyRouteHandler(
	resolver ProxyRouteResolver,
	baseDomain string,
	internalToken string,
	routeTTL time.Duration,
) (*ProxyRouteHandler, error) {
	if resolver == nil {
		return nil, errors.New("proxy route resolver is required")
	}
	normalizedDomain, err := normalizeProxyBaseDomain(baseDomain)
	if err != nil {
		return nil, err
	}
	internalToken = strings.TrimSpace(internalToken)
	if internalToken == "" {
		return nil, errors.New("proxy internal auth token is required")
	}
	if routeTTL <= 0 || routeTTL > proxyRouteMaxTTL {
		return nil, errors.New("proxy route TTL must be between 1 second and 7 days")
	}
	return &ProxyRouteHandler{
		resolver:      resolver,
		baseDomain:    normalizedDomain,
		internalToken: internalToken,
		routeTTL:      routeTTL,
		nowFn:         time.Now,
		randomReader:  rand.Reader,
		routes:        make(map[string]proxyRouteRecord),
	}, nil
}

func (h *ProxyRouteHandler) Create(c *gin.Context) {
	ownerID, ok := proxyRouteOwnerID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	req := createProxyRouteRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
		return
	}
	if len(sessionID) > 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is too long"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "port must be between 1 and 65535"})
		return
	}

	now := h.now()
	target, err := h.resolver.ResolveProxySession(ownerID, sessionID, now)
	if err != nil {
		switch {
		case errors.Is(err, grpcserver.ErrProxySessionNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		case errors.Is(err, grpcserver.ErrProxyWorkerUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session worker proxy is unavailable"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve session"})
		}
		return
	}

	record, err := h.createRoute(ownerID, sessionID, req.Port, target, now)
	if err != nil {
		if errors.Is(err, errProxyRouteLimitReached) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "proxy route limit reached"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create proxy route"})
		return
	}
	c.JSON(http.StatusCreated, h.routeResponse(record))
}

func (h *ProxyRouteHandler) List(c *gin.Context) {
	ownerID, ok := proxyRouteOwnerID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	now := h.now()
	records := h.listRoutes(ownerID, now)
	items := make([]proxyRouteResponse, 0, len(records))
	for _, record := range records {
		items = append(items, h.routeResponse(record))
	}
	c.JSON(http.StatusOK, listProxyRoutesResponse{Items: items, Total: len(items)})
}

func (h *ProxyRouteHandler) Delete(c *gin.Context) {
	ownerID, ok := proxyRouteOwnerID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	routeKey := strings.ToLower(strings.TrimSpace(c.Param("route_key")))
	if !validProxyRouteKey(routeKey) || !h.deleteRoute(ownerID, routeKey, h.now()) {
		c.JSON(http.StatusNotFound, gin.H{"error": "proxy route not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ProxyRouteHandler) Resolve(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !secureStringEqual(c.GetHeader(proxyInternalAuthHeader), h.internalToken) {
		c.Status(http.StatusUnauthorized)
		return
	}
	routeKey, ok := h.routeKeyFromHost(c.GetHeader(proxyOriginalHostHeader))
	if !ok {
		c.Status(http.StatusForbidden)
		return
	}
	record, ok := h.getRoute(routeKey, h.now())
	if !ok {
		c.Status(http.StatusForbidden)
		return
	}
	authorization, err := h.resolver.AuthorizeProxyRoute(
		record.WorkerID,
		record.ScopedSessionID,
		record.Port,
		record.ExpiresAt,
		h.now(),
	)
	if err != nil {
		if errors.Is(err, grpcserver.ErrProxySessionNotFound) || errors.Is(err, grpcserver.ErrProxyWorkerUnavailable) {
			c.Status(http.StatusForbidden)
			return
		}
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header(proxyUpstreamHeader, authorization.Upstream)
	c.Header(proxytoken.HeaderName, authorization.Token)
	c.Status(http.StatusNoContent)
}

var errProxyRouteLimitReached = errors.New("proxy route limit reached")

func (h *ProxyRouteHandler) createRoute(
	ownerID string,
	sessionID string,
	port int,
	target grpcserver.ProxySessionTarget,
	now time.Time,
) (proxyRouteRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)

	count := 0
	for _, record := range h.routes {
		if record.OwnerID == ownerID {
			count++
		}
	}
	if count >= proxyRouteMaxPerOwner {
		return proxyRouteRecord{}, errProxyRouteLimitReached
	}

	for attempt := 0; attempt < proxyRouteCreateAttempts; attempt++ {
		routeKey, err := generateProxyRouteKey(h.randomReader)
		if err != nil {
			return proxyRouteRecord{}, err
		}
		if _, exists := h.routes[routeKey]; exists {
			continue
		}
		record := proxyRouteRecord{
			RouteKey:        routeKey,
			OwnerID:         ownerID,
			SessionID:       sessionID,
			ScopedSessionID: target.ScopedSessionID,
			Port:            port,
			WorkerID:        target.WorkerID,
			CreatedAt:       now,
			ExpiresAt:       now.Add(h.routeTTL),
		}
		h.routes[routeKey] = record
		return record, nil
	}
	return proxyRouteRecord{}, errors.New("failed to allocate unique proxy route key")
}

func (h *ProxyRouteHandler) listRoutes(ownerID string, now time.Time) []proxyRouteRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)

	records := make([]proxyRouteRecord, 0)
	for _, record := range h.routes {
		if record.OwnerID == ownerID {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
	return records
}

func (h *ProxyRouteHandler) getRoute(routeKey string, now time.Time) (proxyRouteRecord, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	record, ok := h.routes[routeKey]
	return record, ok
}

func (h *ProxyRouteHandler) deleteRoute(ownerID string, routeKey string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	record, ok := h.routes[routeKey]
	if !ok || record.OwnerID != ownerID {
		return false
	}
	delete(h.routes, routeKey)
	return true
}

func (h *ProxyRouteHandler) pruneLocked(now time.Time) {
	for routeKey, record := range h.routes {
		if !record.ExpiresAt.After(now) {
			delete(h.routes, routeKey)
		}
	}
}

func (h *ProxyRouteHandler) routeResponse(record proxyRouteRecord) proxyRouteResponse {
	return proxyRouteResponse{
		RouteKey:  record.RouteKey,
		SessionID: record.SessionID,
		Port:      record.Port,
		URL:       "https://" + record.RouteKey + "." + h.baseDomain,
		CreatedAt: record.CreatedAt,
		ExpiresAt: record.ExpiresAt,
	}
}

func (h *ProxyRouteHandler) routeKeyFromHost(rawHost string) (string, bool) {
	host := strings.ToLower(strings.TrimSpace(rawHost))
	if host == "" {
		return "", false
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	suffix := "." + h.baseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	routeKey := strings.TrimSuffix(host, suffix)
	if !validProxyRouteKey(routeKey) {
		return "", false
	}
	return routeKey, true
}

func (h *ProxyRouteHandler) now() time.Time {
	if h != nil && h.nowFn != nil {
		return h.nowFn()
	}
	return time.Now()
}

func proxyRouteOwnerID(c *gin.Context) (string, bool) {
	account, ok := requestSessionAccountFromGin(c)
	if ok && strings.TrimSpace(account.AccountID) != "" {
		return strings.TrimSpace(account.AccountID), true
	}
	return "", false
}

func generateProxyRouteKey(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("route key random source is required")
	}
	raw := make([]byte, proxyRouteKeyBytes)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}

func validProxyRouteKey(value string) bool {
	if len(value) != proxyRouteKeyLength {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func normalizeProxyBaseDomain(value string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(value))
	if domain == "" {
		return "", errors.New("proxy public base domain is required")
	}
	if len(domain) > proxyBaseDomainMaxBytes || strings.ContainsAny(domain, "/:") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", errors.New("proxy public base domain is invalid")
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", errors.New("proxy public base domain must contain at least two labels")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("proxy public base domain is invalid")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", errors.New("proxy public base domain is invalid")
			}
		}
	}
	return domain, nil
}

func secureStringEqual(provided string, expected string) bool {
	providedHash := sha256.Sum256([]byte(strings.TrimSpace(provided)))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}
