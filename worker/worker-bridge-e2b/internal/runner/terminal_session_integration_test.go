package runner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

type recordingE2BBackend struct {
	e2bBackend
	mu      sync.Mutex
	killIDs []string
}

func (b *recordingE2BBackend) Kill(ctx context.Context, sandboxID string) error {
	err := b.e2bBackend.Kill(ctx, sandboxID)
	if err == nil {
		b.mu.Lock()
		b.killIDs = append(b.killIDs, sandboxID)
		b.mu.Unlock()
	}
	return err
}

func (b *recordingE2BBackend) killCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.killIDs)
}

func TestIntegrationTerminalSessionConcurrentExecAndResources(t *testing.T) {
	backend, template := liveTerminalBackend(t)
	manager := newLiveTerminalManager(backend, template, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{
		Command: `printf 'resource-live' > /tmp/onlyboxes-resource-live.txt`,
	})
	if err != nil {
		t.Fatal(err)
	}

	startGate := make(chan struct{})
	results := make(chan outcome, 2)
	startedAt := time.Now()
	for _, command := range []string{
		`sleep 2; printf 'concurrent-a'`,
		`sleep 2; printf 'concurrent-b'`,
	} {
		command := command
		go func() {
			<-startGate
			result, err := manager.Execute(context.Background(), terminalExecRequest{
				Command:   command,
				SessionID: seed.SessionID,
			})
			results <- outcome{result: result, err: err}
		}()
	}
	close(startGate)
	waitForSessionInflight(t, manager, seed.SessionID, 2, 3*time.Second)
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "printf overflow",
		SessionID: seed.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionBusy {
		t.Fatalf("expected third concurrent terminalExec to return session_busy, got %v", err)
	}
	outputs := map[string]bool{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.result.SessionID != seed.SessionID {
			t.Fatalf("concurrent command switched sessions: %#v", result.result)
		}
		outputs[result.result.Stdout] = true
	}
	if !outputs["concurrent-a"] || !outputs["concurrent-b"] {
		t.Fatalf("unexpected concurrent outputs: %v", outputs)
	}
	if elapsed := time.Since(startedAt); elapsed >= 3500*time.Millisecond {
		t.Fatalf("commands appear serial, elapsed=%s", elapsed)
	}

	validate, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/onlyboxes-resource-live.txt",
		Action:    terminalResourceActionValidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if validate.SizeBytes != int64(len("resource-live")) {
		t.Fatalf("unexpected validate result: %#v", validate)
	}
	read, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/onlyboxes-resource-live.txt",
		Action:    terminalResourceActionRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(read.Blob) != "resource-live" {
		t.Fatalf("unexpected read result: %#v", read)
	}

	var uploaded []byte
	uploadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		uploaded, _ = io.ReadAll(req.Body)
		if req.Header.Get("X-Onlyboxes-Test") != "export" {
			t.Errorf("export header was not forwarded")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer uploadServer.Close()
	if _, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/onlyboxes-resource-live.txt",
		Action:    terminalResourceActionExport,
		SignedURL: uploadServer.URL,
		Headers:   map[string]string{"X-Onlyboxes-Test": "export"},
	}); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "resource-live" {
		t.Fatalf("unexpected exported content %q", uploaded)
	}

	execDone := make(chan outcome, 1)
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:   `sleep 4; printf 'mixed-exec-ok'`,
			SessionID: seed.SessionID,
		})
		execDone <- outcome{result: result, err: err}
	}()
	waitForSessionInflight(t, manager, seed.SessionID, 1, 3*time.Second)
	mixedRead, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/onlyboxes-resource-live.txt",
		Action:    terminalResourceActionRead,
	})
	if err != nil || string(mixedRead.Blob) != "resource-live" {
		t.Fatalf("mixed terminalResource failed: result=%#v err=%v", mixedRead, err)
	}
	select {
	case early := <-execDone:
		t.Fatalf("terminalExec ended before concurrent resource completed: %#v err=%v", early.result, early.err)
	default:
	}
	mixedExec := <-execDone
	if mixedExec.err != nil || mixedExec.result.Stdout != "mixed-exec-ok" {
		t.Fatalf("mixed terminalExec failed: %#v err=%v", mixedExec.result, mixedExec.err)
	}

	manager.Close()
	if backend.killCount() != 1 {
		t.Fatalf("expected one E2B sandbox cleanup, got %d", backend.killCount())
	}
}

func TestIntegrationTimedOutCommandDoesNotKillSibling(t *testing.T) {
	backend, template := liveTerminalBackend(t)
	manager := newLiveTerminalManager(backend, template, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "printf seed"})
	if err != nil {
		t.Fatal(err)
	}
	siblingDone := make(chan outcome, 1)
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:   `sleep 2; printf 'sibling-live-ok'`,
			SessionID: seed.SessionID,
		})
		siblingDone <- outcome{result: result, err: err}
	}()
	waitForSessionInflight(t, manager, seed.SessionID, 1, 3*time.Second)
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = manager.Execute(timeoutCtx, terminalExecRequest{
		Command:   `sleep 5; printf should-not-complete`,
		SessionID: seed.SessionID,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	manager.mu.Lock()
	sessionAfterTimeout := manager.sessions[seed.SessionID]
	if sessionAfterTimeout == nil || !sessionAfterTimeout.destroying || sessionAfterTimeout.inflight != 1 {
		manager.mu.Unlock()
		t.Fatalf("unexpected session state after timeout: %#v", sessionAfterTimeout)
	}
	manager.mu.Unlock()
	if backend.killCount() != 0 {
		t.Fatal("sandbox was killed before sibling drained")
	}
	sibling := <-siblingDone
	if sibling.err != nil || sibling.result.Stdout != "sibling-live-ok" {
		t.Fatalf("sibling was disrupted: %#v err=%v", sibling.result, sibling.err)
	}
	manager.mu.Lock()
	_, sessionStillPresent := manager.sessions[seed.SessionID]
	manager.mu.Unlock()
	if sessionStillPresent {
		t.Fatal("destroying session remained registered after sibling drained")
	}
	waitForCondition(t, 5*time.Second, func() bool { return backend.killCount() == 1 }, "sandbox was not killed after sibling drained")
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "printf after",
		SessionID: seed.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("timed-out session still accepted commands: %v", err)
	}
}

func TestIntegrationJanitorExpiresTerminalSession(t *testing.T) {
	backend, template := liveTerminalBackend(t)
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           template,
		LeaseMinSec:        1,
		LeaseMaxSec:        30,
		LeaseDefaultSec:    1,
		OutputLimitBytes:   1024,
		SessionMaxInflight: 1,
		JanitorInterval:    100 * time.Millisecond,
	})
	defer manager.Close()
	session, err := manager.Execute(context.Background(), terminalExecRequest{Command: "printf expiring"})
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		return manager.ActiveSessionCount() == 0 && backend.killCount() == 1
	}, "janitor did not expire and clean up the live E2B sandbox")
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "printf after",
		SessionID: session.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("expired session still accepted commands: %v", err)
	}
}

func liveTerminalBackend(t *testing.T) (*recordingE2BBackend, string) {
	t.Helper()
	if os.Getenv("E2B_INTEGRATION") != "1" {
		t.Skip("set E2B_INTEGRATION=1 to run against E2B")
	}
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	template := strings.TrimSpace(os.Getenv("E2B_TERMINAL_TEMPLATE"))
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and E2B_TERMINAL_TEMPLATE are required")
	}
	client, err := e2b.NewClient(e2b.Config{
		APIKey:         apiKey,
		APIURL:         strings.TrimSpace(os.Getenv("E2B_API_URL")),
		Domain:         strings.TrimSpace(os.Getenv("E2B_DOMAIN")),
		RequestTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &recordingE2BBackend{e2bBackend: client}, template
}

func newLiveTerminalManager(backend e2bBackend, template string, maxInflight int) *terminalSessionManager {
	return newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           template,
		LeaseMinSec:        1,
		LeaseMaxSec:        120,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024 * 1024,
		ExportMaxBytes:     1024 * 1024,
		ExportMode:         terminalExportModeWorker,
		SessionMaxInflight: maxInflight,
	})
}

func waitForSessionInflight(t *testing.T, manager *terminalSessionManager, sessionID string, want int, timeout time.Duration) {
	t.Helper()
	waitForCondition(t, timeout, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		session := manager.sessions[sessionID]
		return session != nil && session.inflight == want
	}, "session did not reach expected inflight count")
}
