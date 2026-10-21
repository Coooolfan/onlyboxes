package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/api/proxytoken"
)

func TestSandboxProxyUsesSignedGenerationAcrossHoldAndReactivation(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	m, session := newProxyTestSession(t, upstream.URL, time.Now().Add(time.Minute))
	handler := newProxyTestHandler(t, m)
	parsed, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(parsed.Port())
	key, _ := proxytoken.DeriveKey("worker-secret")
	request := func(generation int64, want int) {
		t.Helper()
		token, err := proxytoken.Sign(key, proxytoken.Claims{WorkerID: "worker-test", SessionID: session.sessionID, Port: port, ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli(), Generation: generation})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
		req.Header.Set(proxytoken.HeaderName, token)
		// Untrusted HTTP metadata cannot replace a stale signed generation.
		req.Header.Set("X-Onlyboxes-Generation", "3")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("generation %d: status=%d want=%d", generation, response.Code, want)
		}
	}
	request(0, http.StatusNoContent)
	request(2, http.StatusNotFound)
	m.mu.Lock()
	m.setPersistenceFenceLocked(session.sessionID, terminalPersistenceFence{operationID: "restore-2", generation: 2, containerID: "root-2", active: true})
	oldContext := session.proxyCtx
	m.mu.Unlock()
	for _, generation := range []int64{0, 1, 3} {
		request(generation, http.StatusNotFound)
	}
	request(2, http.StatusNoContent)
	m.mu.Lock()
	m.setPersistenceFenceLocked(session.sessionID, terminalPersistenceFence{operationID: "suspend-2", generation: 2, containerID: "root-2", held: true})
	m.mu.Unlock()
	if oldContext.Err() == nil {
		t.Fatal("hold left the old proxy context open")
	}
	request(2, http.StatusNotFound)
	m.mu.Lock()
	m.setPersistenceFenceLocked(session.sessionID, terminalPersistenceFence{operationID: "restore-3", generation: 3, containerID: "root-3", active: true})
	m.mu.Unlock()
	request(2, http.StatusNotFound)
	request(3, http.StatusNoContent)
	if hits.Load() != 3 {
		t.Fatalf("stale requests reached upstream: %d", hits.Load())
	}
}

// Done is evaluated after the resolver's first gate and before readiness wait.
type proxyGateContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (c *proxyGateContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	return c.Context.Done()
}

func TestProxyGenerationIsRecheckedAfterReadiness(t *testing.T) {
	m, session := newProxyTestSession(t, "http://127.0.0.1:8080", time.Now().Add(time.Minute))
	m.mu.Lock()
	session.ready = make(chan struct{})
	ready := session.ready
	m.setPersistenceFenceLocked(session.sessionID, terminalPersistenceFence{operationID: "restore-2", generation: 2, containerID: "root-2", active: true})
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	gate := &proxyGateContext{Context: ctx, reached: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := m.ResolveProxyTarget(gate, session.sessionID, 2, time.Now()); done <- err }()
	select {
	case <-gate.reached:
	case <-ctx.Done():
		t.Fatal("proxy never reached readiness gate")
	}
	m.mu.Lock()
	m.setPersistenceFenceLocked(session.sessionID, terminalPersistenceFence{operationID: "restore-3", generation: 3, containerID: "root-3", active: true})
	close(ready)
	m.mu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, errTerminalProxySessionNotFound) {
			t.Fatalf("old generation passed readiness: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("proxy readiness did not finish")
	}
}
