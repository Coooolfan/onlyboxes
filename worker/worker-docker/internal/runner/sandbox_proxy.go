package runner

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/logging"
)

const (
	proxyInternalAuthHeader    = "X-Onlyboxes-Internal-Token"
	proxyOriginalHostHeader    = "X-Original-Host"
	proxyUpstreamHeader        = "X-Onlyboxes-Upstream"
	proxyReadHeaderTimeout     = 5 * time.Second
	proxyIdleTimeout           = 2 * time.Minute
	proxyDialTimeout           = 3 * time.Second
	proxyResponseHeaderTimeout = 15 * time.Second
	proxyMaxHeaderBytes        = 1 << 20
)

var sandboxProxyShutdownTimeout = 5 * time.Second

type proxyTargetContextKey struct{}

type sandboxProxyHandler struct {
	workerID string
	key      []byte
	manager  *terminalSessionManager
	nowFn    func() time.Time
	proxy    *httputil.ReverseProxy
}

func newSandboxProxyHandler(workerID string, workerSecret string, manager *terminalSessionManager) (*sandboxProxyHandler, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, errors.New("worker ID is required")
	}
	if manager == nil {
		return nil, errors.New("terminal session manager is required")
	}
	key, err := proxytoken.DeriveKey(workerSecret)
	if err != nil {
		return nil, err
	}

	handler := &sandboxProxyHandler{
		workerID: workerID,
		key:      key,
		manager:  manager,
		nowFn:    time.Now,
	}
	handler.proxy = &httputil.ReverseProxy{
		Rewrite:       rewriteSandboxProxyRequest,
		Transport:     newSandboxProxyTransport(),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "sandbox upstream unavailable", http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del(proxytoken.HeaderName)
			response.Header.Del(proxyInternalAuthHeader)
			response.Header.Del(proxyOriginalHostHeader)
			response.Header.Del(proxyUpstreamHeader)
			return nil
		},
	}
	return handler, nil
}

func (h *sandboxProxyHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tokenValues := request.Header.Values(proxytoken.HeaderName)
	if len(tokenValues) != 1 {
		http.Error(w, "invalid route token", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	if h.nowFn != nil {
		now = h.nowFn()
	}
	claims, err := proxytoken.Verify(h.key, tokenValues[0], now)
	if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(claims.WorkerID)), []byte(h.workerID)) != 1 {
		http.Error(w, "invalid route token", http.StatusUnauthorized)
		return
	}

	target, err := h.manager.ResolveProxyTarget(request.Context(), claims.SessionID, now)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		http.Error(w, "sandbox session not found", http.StatusNotFound)
		return
	}
	targetURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(target.IP, strconv.Itoa(claims.Port)),
	}

	proxyCtx, cancel := context.WithCancel(request.Context())
	stopSessionCancel := context.AfterFunc(target.SessionContext, cancel)
	defer func() {
		stopSessionCancel()
		cancel()
	}()
	proxyCtx = context.WithValue(proxyCtx, proxyTargetContextKey{}, targetURL)
	request = request.WithContext(proxyCtx)
	request.Header.Del(proxytoken.HeaderName)
	request.Header.Del(proxyInternalAuthHeader)
	request.Header.Del(proxyOriginalHostHeader)
	request.Header.Del(proxyUpstreamHeader)
	h.proxy.ServeHTTP(w, request)
}

func rewriteSandboxProxyRequest(request *httputil.ProxyRequest) {
	target, _ := request.In.Context().Value(proxyTargetContextKey{}).(*url.URL)
	if target == nil {
		return
	}
	request.SetURL(target)
	request.Out.Host = request.In.Host
	request.Out.Header.Del(proxytoken.HeaderName)
	request.Out.Header.Del(proxyInternalAuthHeader)
	request.Out.Header.Del(proxyOriginalHostHeader)
	request.Out.Header.Del(proxyUpstreamHeader)
	copyTrustedForwardingHeader(request.Out.Header, request.In.Header, "X-Forwarded-For")
	copyTrustedForwardingHeader(request.Out.Header, request.In.Header, "X-Forwarded-Host")
	copyTrustedForwardingHeader(request.Out.Header, request.In.Header, "X-Forwarded-Proto")
	copyTrustedForwardingHeader(request.Out.Header, request.In.Header, "X-Request-ID")
}

func copyTrustedForwardingHeader(destination http.Header, source http.Header, name string) {
	destination.Del(name)
	for _, value := range source.Values(name) {
		destination.Add(name, value)
	}
}

func newSandboxProxyTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{
		Timeout:   proxyDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ResponseHeaderTimeout = proxyResponseHeaderTimeout
	transport.IdleConnTimeout = 90 * time.Second
	transport.ForceAttemptHTTP2 = false
	return transport
}

func validateProxyConfig(cfg config.Config) error {
	if !cfg.ProxyEnabled {
		return nil
	}
	if strings.TrimSpace(cfg.ProxyListenAddr) == "" {
		return errors.New("WORKER_PROXY_LISTEN_ADDR is required when proxy is enabled")
	}
	listenHost, listenRawPort, err := net.SplitHostPort(strings.TrimSpace(cfg.ProxyListenAddr))
	if err != nil {
		return errors.New("WORKER_PROXY_LISTEN_ADDR must be an IP address with port")
	}
	var listenAddress netip.Addr
	if strings.TrimSpace(listenHost) != "" {
		listenAddress, err = netip.ParseAddr(strings.TrimSpace(listenHost))
		if err != nil || listenAddress.Zone() != "" {
			return errors.New("WORKER_PROXY_LISTEN_ADDR host must be an IP address")
		}
		listenAddress = listenAddress.Unmap()
	}
	listenPort, err := strconv.Atoi(listenRawPort)
	if err != nil || listenPort < 1 || listenPort > 65535 {
		return errors.New("WORKER_PROXY_LISTEN_ADDR port must be between 1 and 65535")
	}

	host, rawPort, err := net.SplitHostPort(strings.TrimSpace(cfg.ProxyAdvertiseAddr))
	if err != nil {
		return errors.New("WORKER_PROXY_ADVERTISE_ADDR must be an IP address with port")
	}
	address, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil || address.Zone() != "" {
		return errors.New("WORKER_PROXY_ADVERTISE_ADDR host must be a unicast IP address")
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return errors.New("WORKER_PROXY_ADVERTISE_ADDR host must be a unicast IP address")
	}
	if listenAddress.IsValid() && !listenAddress.IsUnspecified() && listenAddress != address {
		return errors.New("WORKER_PROXY_LISTEN_ADDR host must be unspecified or match WORKER_PROXY_ADVERTISE_ADDR")
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("WORKER_PROXY_ADVERTISE_ADDR port must be between 1 and 65535")
	}
	if port != listenPort {
		return errors.New("WORKER_PROXY_ADVERTISE_ADDR port must match WORKER_PROXY_LISTEN_ADDR")
	}
	return nil
}

func newSandboxProxyHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ProxyListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyIdleTimeout,
		MaxHeaderBytes:    proxyMaxHeaderBytes,
	}
}

func runSandboxProxyServer(ctx context.Context, cfg config.Config, handler http.Handler, ready chan<- net.Addr) error {
	listener, err := net.Listen("tcp", cfg.ProxyListenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	if ready != nil {
		select {
		case ready <- listener.Addr():
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	server := newSandboxProxyHTTPServer(cfg, handler)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), sandboxProxyShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logging.Warnf("sandbox proxy shutdown failed: %v", err)
			if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
				logging.Warnf("sandbox proxy force close failed: %v", closeErr)
			}
		}
	}()

	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return ctx.Err()
}
