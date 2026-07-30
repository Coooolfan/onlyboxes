package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

type fakeE2BBackend struct {
	mu         sync.Mutex
	created    int
	killed     int
	killedIDs  []string
	timeouts   []int
	runStarted chan struct{}
	runRelease chan struct{}
	createFn   func(context.Context, string, int) (*e2b.Sandbox, error)
	timeoutFn  func(context.Context, string, int) error
	runFn      func(context.Context, *e2b.Sandbox, string, int) (e2b.CommandResult, error)
	readFn     func(context.Context, *e2b.Sandbox, string, int64) (e2b.File, error)
	openFn     func(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error)
}

func (f *fakeE2BBackend) Create(ctx context.Context, template string, timeout int) (*e2b.Sandbox, error) {
	if f.createFn != nil {
		return f.createFn(ctx, template, timeout)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	return &e2b.Sandbox{ID: "sandbox-" + strconv.Itoa(f.created), Domain: "test"}, nil
}

func (f *fakeE2BBackend) SetTimeout(ctx context.Context, sandboxID string, timeout int) error {
	if f.timeoutFn != nil {
		return f.timeoutFn(ctx, sandboxID, timeout)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeouts = append(f.timeouts, timeout)
	return nil
}

func (f *fakeE2BBackend) Kill(_ context.Context, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed++
	f.killedIDs = append(f.killedIDs, sandboxID)
	return nil
}

func (f *fakeE2BBackend) Run(ctx context.Context, sandbox *e2b.Sandbox, command string, outputLimit int) (e2b.CommandResult, error) {
	if f.runFn != nil {
		return f.runFn(ctx, sandbox, command, outputLimit)
	}
	if f.runStarted != nil {
		select {
		case f.runStarted <- struct{}{}:
		default:
		}
	}
	if f.runRelease != nil {
		select {
		case <-ctx.Done():
			return e2b.CommandResult{}, ctx.Err()
		case <-f.runRelease:
		}
	}
	return e2b.CommandResult{Stdout: command, ExitCode: 0}, nil
}

func (f *fakeE2BBackend) ReadFile(ctx context.Context, sandbox *e2b.Sandbox, filePath string, limit int64) (e2b.File, error) {
	if f.readFn != nil {
		return f.readFn(ctx, sandbox, filePath, limit)
	}
	return e2b.File{Content: []byte("ok"), MIMEType: "text/plain", Size: 2}, nil
}

func (f *fakeE2BBackend) OpenFile(ctx context.Context, sandbox *e2b.Sandbox, filePath string) (e2b.FileReader, error) {
	if f.openFn != nil {
		return f.openFn(ctx, sandbox, filePath)
	}
	return e2b.FileReader{Body: io.NopCloser(bytes.NewReader([]byte("ok"))), MIMEType: "text/plain", Size: 2}, nil
}

func newTestTerminalManager(backend e2bBackend, maxInflight int) *terminalSessionManager {
	return newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    10,
		OutputLimitBytes:   1024,
		SessionMaxInflight: maxInflight,
	})
}

func TestTerminalSessionCreatesReusesAndExtendsE2BLease(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()

	first, err := manager.Execute(context.Background(), terminalExecRequest{Command: "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.SessionID == "" || first.Stdout != "pwd" {
		t.Fatalf("unexpected first result: %#v", first)
	}
	lease := 20
	second, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:     "ls",
		SessionID:   first.SessionID,
		LeaseTTLSec: &lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.SessionID != first.SessionID || second.Stdout != "ls" {
		t.Fatalf("unexpected reused result: %#v", second)
	}
	if second.LeaseExpiresUnixMS <= first.LeaseExpiresUnixMS {
		t.Fatalf("lease was not extended: first=%d second=%d", first.LeaseExpiresUnixMS, second.LeaseExpiresUnixMS)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.created != 1 {
		t.Fatalf("expected one E2B sandbox, got %d", backend.created)
	}
	if len(backend.timeouts) != 2 || backend.timeouts[1] < 19 {
		t.Fatalf("unexpected E2B timeout updates: %v", backend.timeouts)
	}
}

func TestTerminalSessionRejectsCommandsPastPerSessionLimit(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{
		runStarted: make(chan struct{}, 1),
		runRelease: make(chan struct{}),
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()

	firstDone := make(chan terminalExecRunResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{Command: "first"})
		firstDone <- result
		firstErr <- err
	}()
	<-backend.runStarted

	manager.mu.Lock()
	var sessionID string
	for id := range manager.sessions {
		sessionID = id
	}
	manager.mu.Unlock()
	_, err := manager.Execute(context.Background(), terminalExecRequest{Command: "second", SessionID: sessionID})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionBusy {
		t.Fatalf("expected session_busy, got %v", err)
	}
	close(backend.runRelease)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	if result := <-firstDone; result.SessionID != sessionID {
		t.Fatalf("unexpected first result: %#v", result)
	}
}

func TestTerminalSessionAllowsMultipleConcurrentExecsWithinLimit(t *testing.T) {
	t.Parallel()
	started := make(chan string, 2)
	release := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.runFn = func(ctx context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		if command == "seed" {
			return e2b.CommandResult{}, nil
		}
		started <- command
		select {
		case <-ctx.Done():
			return e2b.CommandResult{}, ctx.Err()
		case <-release:
			return e2b.CommandResult{Stdout: command}, nil
		}
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result terminalExecRunResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, command := range []string{"first", "second"} {
		command := command
		go func() {
			result, err := manager.Execute(context.Background(), terminalExecRequest{
				Command:   command,
				SessionID: seed.SessionID,
			})
			outcomes <- outcome{result: result, err: err}
		}()
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case command := <-started:
			seen[command] = true
		case <-time.After(time.Second):
			t.Fatal("two commands were not concurrently in flight")
		}
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "overflow",
		SessionID: seed.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionBusy {
		t.Fatalf("expected third command to return session_busy, got %v", err)
	}
	close(release)
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.result.SessionID != seed.SessionID || !seen[outcome.result.Stdout] {
			t.Fatalf("unexpected concurrent result: %#v", outcome.result)
		}
	}
}

func TestTerminalSessionJanitorAutomaticallyKillsExpiredSandbox(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    1,
		OutputLimitBytes:   1024,
		SessionMaxInflight: 1,
		JanitorInterval:    10 * time.Millisecond,
	})
	defer manager.Close()
	created, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.sessions[created.SessionID].leaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	waitForCondition(t, time.Second, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.killed == 1
	}, "janitor did not kill expired E2B sandbox")
	if manager.ActiveSessionCount() != 0 {
		t.Fatalf("expired session still counted as active")
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "after-expiry",
		SessionID: created.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found after janitor cleanup, got %v", err)
	}
}

func TestTerminalSessionCreationGateSharesOneSandbox(t *testing.T) {
	t.Parallel()
	createStarted := make(chan struct{})
	createRelease := make(chan struct{})
	var runMu sync.Mutex
	runCount := 0
	backend := &fakeE2BBackend{}
	backend.createFn = func(ctx context.Context, _ string, _ int) (*e2b.Sandbox, error) {
		close(createStarted)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-createRelease:
			return &e2b.Sandbox{ID: "shared-sandbox"}, nil
		}
	}
	backend.runFn = func(context.Context, *e2b.Sandbox, string, int) (e2b.CommandResult, error) {
		runMu.Lock()
		runCount++
		runMu.Unlock()
		return e2b.CommandResult{}, nil
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()

	errs := make(chan error, 2)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "creator",
			SessionID:       "shared-session",
			CreateIfMissing: true,
		})
		errs <- err
	}()
	<-createStarted
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "waiter",
			SessionID:       "shared-session",
			CreateIfMissing: true,
		})
		errs <- err
	}()
	time.Sleep(30 * time.Millisecond)
	runMu.Lock()
	if runCount != 0 {
		runMu.Unlock()
		t.Fatalf("command ran before sandbox creation completed")
	}
	runMu.Unlock()
	close(createRelease)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	runMu.Lock()
	defer runMu.Unlock()
	if runCount != 2 {
		t.Fatalf("expected both commands to use the created sandbox, run_count=%d", runCount)
	}
}

func TestTimedOutCommandDoesNotKillConcurrentSibling(t *testing.T) {
	t.Parallel()
	siblingStarted := make(chan struct{})
	siblingRelease := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.runFn = func(ctx context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		switch command {
		case "seed":
			return e2b.CommandResult{}, nil
		case "sibling":
			close(siblingStarted)
			select {
			case <-ctx.Done():
				return e2b.CommandResult{}, ctx.Err()
			case <-siblingRelease:
				return e2b.CommandResult{Stdout: "sibling-ok"}, nil
			}
		case "timeout":
			<-ctx.Done()
			return e2b.CommandResult{}, ctx.Err()
		default:
			return e2b.CommandResult{Stdout: command}, nil
		}
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	siblingDone := make(chan outcome, 1)
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:   "sibling",
			SessionID: seed.SessionID,
		})
		siblingDone <- outcome{result: result, err: err}
	}()
	<-siblingStarted
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.Execute(timeoutCtx, terminalExecRequest{
		Command:   "timeout",
		SessionID: seed.SessionID,
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	backend.mu.Lock()
	killedWhileSiblingRunning := backend.killed
	backend.mu.Unlock()
	if killedWhileSiblingRunning != 0 {
		t.Fatalf("sandbox was killed while sibling command was running")
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "new-command",
		SessionID: seed.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("destroying session accepted a new command: %v", err)
	}
	close(siblingRelease)
	outcome := <-siblingDone
	if outcome.err != nil || outcome.result.Stdout != "sibling-ok" {
		t.Fatalf("sibling did not finish successfully: %#v err=%v", outcome.result, outcome.err)
	}
	waitForCondition(t, time.Second, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.killed == 1
	}, "sandbox was not killed after sibling drained")
}

func TestTimeoutDuringLeaseSyncAlsoWaitsForSiblingBeforeCleanup(t *testing.T) {
	t.Parallel()
	siblingStarted := make(chan struct{})
	siblingRelease := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.runFn = func(ctx context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		if command == "seed" {
			return e2b.CommandResult{}, nil
		}
		close(siblingStarted)
		select {
		case <-ctx.Done():
			return e2b.CommandResult{}, ctx.Err()
		case <-siblingRelease:
			return e2b.CommandResult{Stdout: "sibling-ok"}, nil
		}
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	siblingDone := make(chan outcome, 1)
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:   "sibling",
			SessionID: seed.SessionID,
		})
		siblingDone <- outcome{result: result, err: err}
	}()
	<-siblingStarted
	backend.timeoutFn = func(ctx context.Context, _ string, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.Execute(timeoutCtx, terminalExecRequest{
		Command:   "never-starts",
		SessionID: seed.SessionID,
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded during lease sync, got %v", err)
	}
	backend.mu.Lock()
	killedBeforeDrain := backend.killed
	backend.mu.Unlock()
	if killedBeforeDrain != 0 {
		t.Fatal("lease sync timeout killed sandbox before sibling drained")
	}
	close(siblingRelease)
	sibling := <-siblingDone
	if sibling.err != nil || sibling.result.Stdout != "sibling-ok" {
		t.Fatalf("sibling was disrupted: %#v err=%v", sibling.result, sibling.err)
	}
	waitForCondition(t, time.Second, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.killed == 1
	}, "sandbox was not cleaned after sibling drained")
}

func TestTerminalResourceValidateReadAndExport(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	backend.runFn = func(_ context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		if command == "seed" {
			return e2b.CommandResult{}, nil
		}
		return e2b.CommandResult{
			Stdout:   `{"mime_type":"text/plain","size_bytes":5}`,
			ExitCode: 0,
		}, nil
	}
	backend.readFn = func(context.Context, *e2b.Sandbox, string, int64) (e2b.File, error) {
		return e2b.File{Content: []byte("hello"), MIMEType: "text/plain", Size: 5}, nil
	}
	backend.openFn = func(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error) {
		return e2b.FileReader{
			Body:     io.NopCloser(bytes.NewReader([]byte("hello"))),
			MIMEType: "text/plain",
			Size:     5,
		}, nil
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	validate, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionValidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if validate.MIMEType != "text/plain" || validate.SizeBytes != 5 || validate.Blob != nil {
		t.Fatalf("unexpected validate result: %#v", validate)
	}
	read, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(read.Blob) != "hello" || read.SizeBytes != 5 {
		t.Fatalf("unexpected read result: %#v", read)
	}

	var uploaded []byte
	var uploadHeader string
	uploadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		uploaded, _ = io.ReadAll(req.Body)
		uploadHeader = req.Header.Get("X-Test-Export")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer uploadServer.Close()
	exported, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionExport,
		SignedURL: uploadServer.URL,
		Headers:   map[string]string{"X-Test-Export": "forwarded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "hello" || uploadHeader != "forwarded" {
		t.Fatalf("unexpected export upload body=%q header=%q", uploaded, uploadHeader)
	}
	if exported.Blob != nil || exported.SizeBytes != 5 {
		t.Fatalf("unexpected export result: %#v", exported)
	}
}

func TestTerminalResourceSharesSessionConcurrencyWithExec(t *testing.T) {
	t.Parallel()
	execStarted := make(chan struct{})
	execRelease := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.runFn = func(ctx context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		switch command {
		case "seed":
			return e2b.CommandResult{}, nil
		case "block":
			close(execStarted)
			select {
			case <-ctx.Done():
				return e2b.CommandResult{}, ctx.Err()
			case <-execRelease:
				return e2b.CommandResult{Stdout: "done"}, nil
			}
		default:
			return e2b.CommandResult{Stdout: `{"mime_type":"text/plain","size_bytes":2}`}, nil
		}
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	execDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:   "block",
			SessionID: seed.SessionID,
		})
		execDone <- err
	}()
	<-execStarted
	resource, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/file.txt",
		Action:    terminalResourceActionRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(resource.Blob) != "ok" {
		t.Fatalf("unexpected resource result: %#v", resource)
	}
	close(execRelease)
	if err := <-execDone; err != nil {
		t.Fatal(err)
	}
}

func TestTerminalResourceReturnsBusyWhenExecUsesOnlySlot(t *testing.T) {
	t.Parallel()
	execStarted := make(chan struct{})
	execRelease := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.runFn = func(ctx context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		if command == "seed" {
			return e2b.CommandResult{}, nil
		}
		close(execStarted)
		select {
		case <-ctx.Done():
			return e2b.CommandResult{}, ctx.Err()
		case <-execRelease:
			return e2b.CommandResult{}, nil
		}
	}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	execDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:   "block",
			SessionID: seed.SessionID,
		})
		execDone <- err
	}()
	<-execStarted
	_, err = manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/file.txt",
		Action:    terminalResourceActionValidate,
	})
	if terminalErrorCode(err) != terminalExecCodeSessionBusy {
		t.Fatalf("expected resource to share session_busy limit, got %v", err)
	}
	close(execRelease)
	if err := <-execDone; err != nil {
		t.Fatal(err)
	}
}

type outcome struct {
	result terminalExecRunResult
	err    error
}

func terminalErrorCode(err error) string {
	var terminalErr *terminalExecError
	if errors.As(err, &terminalErr) {
		return terminalErr.Code()
	}
	return ""
}

func waitForCondition(t *testing.T, timeout time.Duration, predicate func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}
