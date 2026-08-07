package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
)

func TestSandboxProxyHTTPPreservesApplicationRequestAndStripsInternalHeaders(t *testing.T) {
	body := bytes.Repeat([]byte("payload-"), 128*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		got, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if !bytes.Equal(got, body) {
			t.Errorf("unexpected request body length: want=%d got=%d", len(body), len(got))
		}
		if request.Host != "preview.public-preview.example.com" {
			t.Errorf("unexpected host %q", request.Host)
		}
		if request.Header.Get("Authorization") != "Bearer app-token" {
			t.Errorf("application authorization was not preserved")
		}
		if request.Header.Get("Cookie") != "app_session=abc" {
			t.Errorf("application cookie was not preserved")
		}
		if request.Header.Get(proxytoken.HeaderName) != "" || request.Header.Get(proxyInternalAuthHeader) != "" || request.Header.Get(proxyOriginalHostHeader) != "" || request.Header.Get(proxyUpstreamHeader) != "" {
			t.Errorf("internal headers reached sandbox: %#v", request.Header)
		}
		if request.Header.Get("X-Forwarded-Host") != "preview.public-preview.example.com" {
			t.Errorf("forwarded host was not preserved")
		}
		w.Header().Set(proxytoken.HeaderName, "sandbox-forged-token")
		w.Header().Set(proxyOriginalHostHeader, "sandbox-forged-host")
		w.Header().Set(proxyUpstreamHeader, "sandbox-forged-upstream")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	manager, session := newProxyTestSession(t, upstream.URL, time.Now().Add(time.Minute))
	originalLeaseExpiry := session.leaseExpiresAt
	handler := newProxyTestHandler(t, manager)
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	token := signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(15*time.Second))
	request, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/upload?q=1", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Host = "preview.public-preview.example.com"
	request.Header.Set(proxytoken.HeaderName, token)
	request.Header.Set(proxyInternalAuthHeader, "client-forged")
	request.Header.Set(proxyOriginalHostHeader, "attacker.example")
	request.Header.Set(proxyUpstreamHeader, "client-forged")
	request.Header.Set("Authorization", "Bearer app-token")
	request.Header.Set("Cookie", "app_session=abc")
	request.Header.Set("X-Forwarded-Host", "preview.public-preview.example.com")
	request.Header.Set("X-Forwarded-Proto", "https")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", response.StatusCode)
	}
	if response.Header.Get(proxytoken.HeaderName) != "" || response.Header.Get(proxyOriginalHostHeader) != "" || response.Header.Get(proxyUpstreamHeader) != "" {
		t.Fatalf("sandbox internal response headers were not stripped: %#v", response.Header)
	}
	responseBody, _ := io.ReadAll(response.Body)
	if string(responseBody) != "ok" {
		t.Fatalf("unexpected response body %q", responseBody)
	}
	manager.mu.Lock()
	inflight := session.inflight
	leaseExpiry := session.leaseExpiresAt
	manager.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("proxy traffic changed terminal inflight count: %d", inflight)
	}
	if !leaseExpiry.Equal(originalLeaseExpiry) {
		t.Fatalf("proxy traffic renewed lease: before=%s after=%s", originalLeaseExpiry, leaseExpiry)
	}
}

func TestSandboxProxyRejectsInvalidTokenAndMissingSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatalf("upstream should not be called")
	}))
	defer upstream.Close()
	manager, session := newProxyTestSession(t, upstream.URL, time.Now().Add(time.Minute))
	handler := newProxyTestHandler(t, manager)

	tests := []struct {
		name       string
		token      string
		statusCode int
	}{
		{name: "missing", statusCode: http.StatusUnauthorized},
		{name: "malformed", token: "not-a-token", statusCode: http.StatusUnauthorized},
		{name: "missing_session", token: signProxyTestToken(t, upstream.URL, "unknown-session", time.Now().Add(15*time.Second)), statusCode: http.StatusNotFound},
		{name: "wrong_worker", token: signProxyTestTokenForWorker(t, upstream.URL, session.sessionID, "worker-other", time.Now().Add(15*time.Second)), statusCode: http.StatusUnauthorized},
		{name: "expired", token: signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(-time.Second)), statusCode: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
			if test.token != "" {
				request.Header.Set(proxytoken.HeaderName, test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.statusCode {
				t.Fatalf("expected %d, got %d body=%s", test.statusCode, response.Code, response.Body.String())
			}
		})
	}

	duplicateTokenRequest := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	validToken := signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(15*time.Second))
	duplicateTokenRequest.Header.Add(proxytoken.HeaderName, validToken)
	duplicateTokenRequest.Header.Add(proxytoken.HeaderName, validToken)
	duplicateTokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateTokenResponse, duplicateTokenRequest)
	if duplicateTokenResponse.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate route tokens expected 401, got %d", duplicateTokenResponse.Code)
	}
}

func TestSandboxProxyRejectsExpiredSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatalf("expired session upstream should not be called")
	}))
	defer upstream.Close()
	manager, session := newProxyTestSession(t, upstream.URL, time.Now().Add(-time.Second))
	handler := newProxyTestHandler(t, manager)
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	request.Header.Set(proxytoken.HeaderName, signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(15*time.Second)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expired session expected 404, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestSandboxProxyReturnsBadGatewayWhenPortIsClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve closed port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved port: %v", err)
	}
	upstreamURL := "http://" + address
	manager, session := newProxyTestSession(t, upstreamURL, time.Now().Add(time.Minute))
	handler := newProxyTestHandler(t, manager)
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	request.Header.Set(proxytoken.HeaderName, signProxyTestToken(t, upstreamURL, session.sessionID, time.Now().Add(15*time.Second)))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for closed sandbox port, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestSandboxProxyEstablishedStreamIgnoresTokenExpiry(t *testing.T) {
	releaseSecondEvent := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-releaseSecondEvent
		_, _ = io.WriteString(w, "data: second\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	manager, session := newProxyTestSession(t, upstream.URL, time.Now().Add(time.Minute))
	proxyServer := httptest.NewServer(newProxyTestHandler(t, manager))
	defer proxyServer.Close()
	request, _ := http.NewRequest(http.MethodGet, proxyServer.URL+"/events", nil)
	request.Header.Set(proxytoken.HeaderName, signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(500*time.Millisecond)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("read first event line=%q err=%v", line, err)
	}

	time.Sleep(700 * time.Millisecond)
	close(releaseSecondEvent)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("consume first event separator: %v", err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "data: second\n" {
		t.Fatalf("established stream ended with route token: line=%q err=%v", line, err)
	}
}

func TestSandboxProxyLeaseDeadlineEndsEstablishedStream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-request.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	leaseExpiresAt := time.Now().Add(750 * time.Millisecond)
	manager, session := newProxyTestSession(t, upstream.URL, leaseExpiresAt)
	proxyServer := httptest.NewServer(newProxyTestHandler(t, manager))
	defer proxyServer.Close()
	request, _ := http.NewRequest(http.MethodGet, proxyServer.URL+"/events", nil)
	request.Header.Set(proxytoken.HeaderName, signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(15*time.Second)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("read first event line=%q err=%v", line, err)
	}

	closed := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		closed <- err
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream remained open after lease deadline")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatalf("upstream request was not canceled at lease deadline")
	}
}

func TestSandboxProxyStreamsSSEWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	manager, session := newProxyTestSession(t, upstream.URL, time.Now().Add(time.Minute))
	proxyServer := httptest.NewServer(newProxyTestHandler(t, manager))
	defer proxyServer.Close()
	request, _ := http.NewRequest(http.MethodGet, proxyServer.URL+"/events", nil)
	request.Header.Set(proxytoken.HeaderName, signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(15*time.Second)))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE response: %v", err)
	}
	defer response.Body.Close()
	firstEvent := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(response.Body).ReadString('\n')
		firstEvent <- line
	}()
	select {
	case line := <-firstEvent:
		if line != "data: first\n" {
			t.Fatalf("unexpected SSE line %q", line)
		}
	case <-time.After(time.Second):
		t.Fatalf("SSE first event was buffered")
	}
}

func TestSandboxProxyWebSocketUpgradeAndSessionCancellation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("upstream response writer cannot hijack")
			return
		}
		connection, buffer, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buffer.Flush()
		line, err := buffer.ReadString('\n')
		if err != nil {
			return
		}
		if line == "ping\n" {
			_, _ = buffer.WriteString("pong\n")
			_ = buffer.Flush()
			_, _ = buffer.ReadString('\n')
		}
	}))
	defer upstream.Close()

	manager, session := newProxyTestSession(t, upstream.URL, time.Now().Add(time.Minute))
	proxyServer := httptest.NewServer(newProxyTestHandler(t, manager))
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)
	connection, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer connection.Close()

	token := signProxyTestToken(t, upstream.URL, session.sessionID, time.Now().Add(15*time.Second))
	_, _ = fmt.Fprintf(connection,
		"GET /ws HTTP/1.1\r\nHost: preview.public-preview.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n%s: %s\r\n\r\n",
		proxytoken.HeaderName,
		token,
	)
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("expected 101 response, got %q", statusLine)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = io.WriteString(connection, "ping\n")
	line, err := reader.ReadString('\n')
	if err != nil || line != "pong\n" {
		t.Fatalf("unexpected upgraded response line=%q err=%v", line, err)
	}

	session.proxyCancel()
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatalf("expected upgraded connection to close after session cancellation")
	}
}

func TestRunSandboxProxyServerForceClosesActiveRequests(t *testing.T) {
	originalShutdownTimeout := sandboxProxyShutdownTimeout
	sandboxProxyShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sandboxProxyShutdownTimeout = originalShutdownTimeout })

	requestCanceled := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-request.Context().Done()
		close(requestCanceled)
	})
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- runSandboxProxyServer(ctx, config.Config{ProxyListenAddr: "127.0.0.1:0"}, handler, ready)
	}()
	address := <-ready
	response, err := http.Get("http://" + address.String())
	if err != nil {
		cancel()
		t.Fatalf("open active proxy request: %v", err)
	}
	defer response.Body.Close()

	cancel()
	select {
	case err := <-serverErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled proxy server, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("proxy server shutdown remained blocked by an active request")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatalf("active request context was not canceled by force close")
	}
}

func newProxyTestSession(t *testing.T, upstreamURL string, expiresAt time.Time) (*terminalSessionManager, *terminalSession) {
	t.Helper()
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if len(args) > 0 && args[0] == "rm" {
			return dockerCommandResult{ExitCode: 0}
		}
		return dockerCommandResult{ExitCode: 0}
	})
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("parse upstream host: %v", err)
	}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	t.Cleanup(manager.Close)
	session := readyTerminalSession("session-proxy", "container-proxy", expiresAt, 0)
	session.containerIP = host
	manager.mu.Lock()
	manager.sessions[session.sessionID] = session
	manager.scheduleSessionLeaseTimerLocked(session)
	manager.mu.Unlock()
	return manager, session
}

func newProxyTestHandler(t *testing.T, manager *terminalSessionManager) *sandboxProxyHandler {
	t.Helper()
	handler, err := newSandboxProxyHandler("worker-test", "worker-secret", manager)
	if err != nil {
		t.Fatalf("new proxy handler: %v", err)
	}
	return handler
}

func signProxyTestToken(t *testing.T, upstreamURL string, sessionID string, expiresAt time.Time) string {
	t.Helper()
	return signProxyTestTokenForWorker(t, upstreamURL, sessionID, "worker-test", expiresAt)
}

func signProxyTestTokenForWorker(t *testing.T, upstreamURL string, sessionID string, workerID string, expiresAt time.Time) string {
	t.Helper()
	parsed, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	_, rawPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("parse upstream port number: %v", err)
	}
	key, err := proxytoken.DeriveKey("worker-secret")
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	token, err := proxytoken.Sign(key, proxytoken.Claims{
		WorkerID:        workerID,
		SessionID:       sessionID,
		Port:            port,
		ExpiresAtUnixMs: expiresAt.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}
