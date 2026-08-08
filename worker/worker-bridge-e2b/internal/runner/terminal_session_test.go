package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

type fakeE2BBackend struct {
	mu                   sync.Mutex
	created              int
	killed               int
	killedIDs            []string
	timeouts             []int
	runStarted           chan struct{}
	runRelease           chan struct{}
	createFn             func(context.Context, string, int) (*e2b.Sandbox, error)
	createWithMetadataFn func(context.Context, string, int, map[string]string) (*e2b.Sandbox, error)
	listFn               func(context.Context, map[string]string) ([]e2b.SandboxInfo, error)
	connectFn            func(context.Context, string, int) (*e2b.Sandbox, error)
	timeoutFn            func(context.Context, string, int) error
	runFn                func(context.Context, *e2b.Sandbox, string, int) (e2b.CommandResult, error)
	readFn               func(context.Context, *e2b.Sandbox, string, int64) (e2b.File, error)
	openFn               func(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error)
	killFn               func(context.Context, string) error
}

func (f *fakeE2BBackend) CreateWithMetadata(ctx context.Context, template string, timeout int, metadata map[string]string) (*e2b.Sandbox, error) {
	if f.createWithMetadataFn != nil {
		return f.createWithMetadataFn(ctx, template, timeout, metadata)
	}
	return f.Create(ctx, template, timeout)
}

func (f *fakeE2BBackend) List(ctx context.Context, metadata map[string]string) ([]e2b.SandboxInfo, error) {
	if f.listFn != nil {
		return f.listFn(ctx, metadata)
	}
	return nil, nil
}

func (f *fakeE2BBackend) Connect(ctx context.Context, sandboxID string, timeout int) (*e2b.Sandbox, error) {
	if f.connectFn != nil {
		return f.connectFn(ctx, sandboxID, timeout)
	}
	return &e2b.Sandbox{ID: sandboxID, Domain: "test", AccessToken: "fresh-token"}, nil
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

func (f *fakeE2BBackend) Kill(ctx context.Context, sandboxID string) error {
	if f.killFn != nil {
		return f.killFn(ctx, sandboxID)
	}
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
		ExportMode:         terminalExportModeWorker,
		SessionMaxInflight: maxInflight,
	})
}

func TestResolveProxyReturnsActiveSandboxOrigin(t *testing.T) {
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	now := time.Now()
	manager.mu.Lock()
	manager.sessions["session-a"] = &terminalSession{
		sessionID:               "session-a",
		sandbox:                 &e2b.Sandbox{ID: "sandbox-a", Domain: "e2b.app", TrafficAccessToken: "traffic-secret"},
		confirmedLeaseExpiresAt: now.Add(time.Minute),
		ready:                   make(chan struct{}),
		capacityReserved:        true,
	}
	close(manager.sessions["session-a"].ready)
	manager.activeSessionReservations = 1
	manager.mu.Unlock()

	resolved, err := manager.ResolveProxy(context.Background(), "session-a", 3000, now)
	if err != nil {
		t.Fatalf("resolve proxy: %v", err)
	}
	if resolved.URL != "https://3000-sandbox-a.e2b.app" || resolved.TrafficToken != "traffic-secret" {
		t.Fatalf("unexpected proxy resolution: %#v", resolved)
	}
	if _, err := manager.ResolveProxy(context.Background(), "session-a", 3000, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expected expired session rejection")
	}
}

func newTestTerminalManagerWithCapacity(backend e2bBackend, maxInflight, maxActiveSessions int) *terminalSessionManager {
	return newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    10,
		OutputLimitBytes:   1024,
		ExportMode:         terminalExportModeWorker,
		SessionMaxInflight: maxInflight,
		MaxActiveSessions:  maxActiveSessions,
	})
}

func TestTerminalSessionCapacityRejectsNewButAllowsExisting(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	backend.runFn = func(_ context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		if strings.Contains(command, "mimetypes.guess_type") {
			return e2b.CommandResult{Stdout: `{"mime_type":"text/plain","size_bytes":5}`}, nil
		}
		return e2b.CommandResult{Stdout: command, ExitCode: 0}, nil
	}
	manager := newTestTerminalManagerWithCapacity(backend, 2, 2)
	defer manager.Close()

	for _, sessionID := range []string{"session-a", "session-b"} {
		if _, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "seed",
			SessionID:       sessionID,
			CreateIfMissing: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.ActiveSessionCount(); got != 2 {
		t.Fatalf("expected active session count 2, got %d", got)
	}

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "overflow",
		SessionID:       "session-c",
		CreateIfMissing: true,
	}); terminalErrorCode(err) != terminalExecCodeSessionCapacityExceeded {
		t.Fatalf("expected session_capacity_exceeded, got %v", err)
	}
	backend.mu.Lock()
	created := backend.created
	backend.mu.Unlock()
	if created != 2 {
		t.Fatalf("capacity rejection reached E2B Create: got %d create calls", created)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "missing",
		SessionID: "missing",
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found for non-creating lookup, got %v", err)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "reuse",
		SessionID: "session-a",
	}); err != nil {
		t.Fatalf("existing session should remain usable at capacity: %v", err)
	}
	resource, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "session-a",
		FilePath:  "/workspace/a.txt",
		Action:    terminalResourceActionValidate,
	})
	if err != nil {
		t.Fatalf("existing session resource should remain usable at capacity: %v", err)
	}
	if resource.SizeBytes != 5 {
		t.Fatalf("unexpected resource result: %#v", resource)
	}
}

func TestTerminalSessionCapacityZeroIsUnlimited(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManagerWithCapacity(backend, 1, 0)

	for _, sessionID := range []string{"one", "two", "three"} {
		if _, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "seed",
			SessionID:       sessionID,
			CreateIfMissing: true,
		}); err != nil {
			t.Fatalf("unlimited create %s: %v", sessionID, err)
		}
	}
	if got := manager.ActiveSessionCount(); got != 3 {
		t.Fatalf("expected three active sessions, got %d", got)
	}
	backend.mu.Lock()
	created := backend.created
	backend.mu.Unlock()
	if created != 3 {
		t.Fatalf("expected three E2B Create calls, got %d", created)
	}

	manager.Close()
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("close leaked capacity: %d", got)
	}
}

func TestTerminalSessionCapacityCountsCreatingAndReleasesCreateFailure(t *testing.T) {
	t.Parallel()
	createStarted := make(chan struct{}, 1)
	createRelease := make(chan struct{})
	var createMu sync.Mutex
	createCalls := 0
	failNext := false
	backend := &fakeE2BBackend{}
	backend.createFn = func(ctx context.Context, _ string, _ int) (*e2b.Sandbox, error) {
		createMu.Lock()
		createCalls++
		call := createCalls
		shouldFail := failNext
		failNext = false
		createMu.Unlock()
		if call == 1 {
			createStarted <- struct{}{}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-createRelease:
			}
		}
		if shouldFail {
			return nil, errors.New("create failed")
		}
		return &e2b.Sandbox{ID: "sandbox-custom-" + strconv.Itoa(call), Domain: "test"}, nil
	}
	manager := newTestTerminalManagerWithCapacity(backend, 1, 1)

	creatorDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "create",
			SessionID:       "creating",
			CreateIfMissing: true,
		})
		creatorDone <- err
	}()
	select {
	case <-createStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for E2B create")
	}
	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("creating session should consume capacity, got %d", got)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "overflow",
		SessionID:       "second",
		CreateIfMissing: true,
	}); terminalErrorCode(err) != terminalExecCodeSessionCapacityExceeded {
		t.Fatalf("expected capacity error while create is pending, got %v", err)
	}
	close(createRelease)
	if err := <-creatorDone; err != nil {
		t.Fatalf("creator failed: %v", err)
	}
	manager.releaseAndDestroySession("creating")
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("destroyed session should release capacity, got %d", got)
	}

	createMu.Lock()
	failNext = true
	createMu.Unlock()
	if _, err := manager.Execute(context.Background(), terminalExecRequest{Command: "failed"}); err == nil {
		t.Fatal("expected E2B create failure")
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("failed create leaked capacity, got %d", got)
	}
	manager.Close()
}

func TestTerminalSessionCapacityHeldUntilE2BKillReturns(t *testing.T) {
	t.Parallel()
	killStarted := make(chan struct{}, 1)
	killRelease := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.killFn = func(ctx context.Context, _ string) error {
		select {
		case killStarted <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-killRelease:
			return nil
		}
	}
	manager := newTestTerminalManagerWithCapacity(backend, 1, 1)

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "expired",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.sessions["expired"].confirmedLeaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	cleanupDone := make(chan struct{})
	go func() {
		manager.cleanupExpiredSessions()
		close(cleanupDone)
	}()
	select {
	case <-killStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for E2B Kill")
	}
	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("cleanup-in-progress session should consume capacity, got %d", got)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "overflow",
		SessionID:       "replacement",
		CreateIfMissing: true,
	}); terminalErrorCode(err) != terminalExecCodeSessionCapacityExceeded {
		t.Fatalf("expected capacity error during Kill, got %v", err)
	}

	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("close returned while E2B Kill was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(killRelease)
	select {
	case <-cleanupDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for E2B cleanup")
	}
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for close")
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("cleanup should release capacity, got %d", got)
	}
}

func TestTerminalSessionCapacityReleasedAfterE2BKillFailure(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	backend.killFn = func(context.Context, string) error {
		return errors.New("E2B Kill unavailable")
	}
	manager := newTestTerminalManagerWithCapacity(backend, 1, 1)
	defer manager.Close()

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "cleanup-failure",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.sessions["cleanup-failure"].confirmedLeaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	manager.cleanupExpiredSessions()

	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("Kill failure retained capacity: %d", got)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "replacement",
		SessionID:       "replacement",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("capacity was not reusable after Kill failure: %v", err)
	}
}

func TestTerminalSessionExpiredReplacementTransfersCapacitySlot(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManagerWithCapacity(backend, 1, 1)
	defer manager.Close()

	first, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "reused",
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.sessions[first.SessionID].confirmedLeaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	replacement, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "replacement",
		SessionID:       first.SessionID,
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("expired replacement should reuse its capacity slot: %v", err)
	}
	if !replacement.Created {
		t.Fatalf("expected replacement to create a new sandbox: %#v", replacement)
	}
	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("expected one transferred slot, got %d", got)
	}
	backend.mu.Lock()
	created, killed := backend.created, backend.killed
	backend.mu.Unlock()
	if created != 2 || killed != 1 {
		t.Fatalf("expected old sandbox killed before replacement create, created=%d killed=%d", created, killed)
	}
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
	if len(backend.timeouts) != 1 || backend.timeouts[0] < 19 {
		t.Fatalf("unexpected E2B timeout updates: %v", backend.timeouts)
	}
}

func TestConcurrentLeaseUpdatesAreAppliedInIncreasingOrder(t *testing.T) {
	t.Parallel()
	firstSyncStarted := make(chan struct{})
	firstSyncRelease := make(chan struct{})
	var timeoutMu sync.Mutex
	var timeouts []int
	activeSyncs := 0
	maxActiveSyncs := 0
	backend := &fakeE2BBackend{}
	backend.timeoutFn = func(ctx context.Context, _ string, timeout int) error {
		timeoutMu.Lock()
		timeouts = append(timeouts, timeout)
		activeSyncs++
		if activeSyncs > maxActiveSyncs {
			maxActiveSyncs = activeSyncs
		}
		call := len(timeouts)
		timeoutMu.Unlock()
		if call == 1 {
			close(firstSyncStarted)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-firstSyncRelease:
			}
		}
		timeoutMu.Lock()
		activeSyncs--
		timeoutMu.Unlock()
		return nil
	}
	manager := newTestTerminalManager(backend, 2)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}

	type leaseOutcome struct {
		result terminalExecRunResult
		err    error
	}
	outcomes := make(chan leaseOutcome, 2)
	shortLease := 20
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:     "short",
			SessionID:   seed.SessionID,
			LeaseTTLSec: &shortLease,
		})
		outcomes <- leaseOutcome{result: result, err: err}
	}()
	<-firstSyncStarted

	longLease := 40
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:     "long",
			SessionID:   seed.SessionID,
			LeaseTTLSec: &longLease,
		})
		outcomes <- leaseOutcome{result: result, err: err}
	}()
	waitForCondition(t, time.Second, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		session := manager.sessions[seed.SessionID]
		return session != nil && time.Until(session.desiredLeaseExpiresAt) > 35*time.Second
	}, "longer lease was not recorded while the first timeout update was in flight")
	close(firstSyncRelease)

	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
	}
	timeoutMu.Lock()
	defer timeoutMu.Unlock()
	if len(timeouts) != 2 || timeouts[1] <= timeouts[0] {
		t.Fatalf("lease updates were not increasing: %v", timeouts)
	}
	if maxActiveSyncs != 1 {
		t.Fatalf("SetTimeout calls overlapped: max_active=%d", maxActiveSyncs)
	}
}

func TestFailedLeaseUpdateDoesNotAdvanceConfirmedExpiry(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}

	backend.timeoutFn = func(context.Context, string, int) error {
		return errors.New("timeout update failed")
	}
	longLease := 20
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:     "fails",
		SessionID:   seed.SessionID,
		LeaseTTLSec: &longLease,
	}); err == nil {
		t.Fatal("expected lease update failure")
	}
	manager.mu.Lock()
	session := manager.sessions[seed.SessionID]
	confirmedAfterFailure := session.confirmedLeaseExpiresAt
	desiredAfterFailure := session.desiredLeaseExpiresAt
	manager.mu.Unlock()
	if confirmedAfterFailure.UnixMilli() != seed.LeaseExpiresUnixMS {
		t.Fatalf("failed update advanced confirmed lease: before=%d after=%d", seed.LeaseExpiresUnixMS, confirmedAfterFailure.UnixMilli())
	}
	if !desiredAfterFailure.After(confirmedAfterFailure) {
		t.Fatalf("expected the requested lease to remain pending: desired=%s confirmed=%s", desiredAfterFailure, confirmedAfterFailure)
	}

	backend.timeoutFn = nil
	retried, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:     "retry",
		SessionID:   seed.SessionID,
		LeaseTTLSec: &longLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.LeaseExpiresUnixMS <= seed.LeaseExpiresUnixMS {
		t.Fatalf("retry did not confirm the pending lease: before=%d after=%d", seed.LeaseExpiresUnixMS, retried.LeaseExpiresUnixMS)
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
	manager.sessions[created.SessionID].confirmedLeaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	waitForCondition(t, time.Second, func() bool {
		backend.mu.Lock()
		killed := backend.killed
		backend.mu.Unlock()
		return killed == 1 && manager.ActiveSessionCount() == 0
	}, "janitor did not kill expired E2B sandbox and release its capacity")
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "after-expiry",
		SessionID: created.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found after janitor cleanup, got %v", err)
	}
}

func TestTerminalSessionRejectsExpiredIdleSessionBeforeJanitorCleanup(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    10,
		OutputLimitBytes:   1024,
		SessionMaxInflight: 1,
		JanitorInterval:    time.Hour,
	})
	defer manager.Close()

	created, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	session := manager.sessions[created.SessionID]
	session.desiredLeaseExpiresAt = time.Now().Add(-time.Second)
	session.confirmedLeaseExpiresAt = session.desiredLeaseExpiresAt
	manager.mu.Unlock()

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "after-expiry",
		SessionID: created.SessionID,
	}); terminalErrorCode(err) != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found before janitor cleanup, got %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.killed != 1 || len(backend.killedIDs) != 1 || backend.killedIDs[0] != "sandbox-1" {
		t.Fatalf("expired sandbox was not cleaned up: killed=%d ids=%v", backend.killed, backend.killedIDs)
	}
}

func TestTerminalSessionReplacesExpiredIdleSessionWhenRequested(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    10,
		OutputLimitBytes:   1024,
		SessionMaxInflight: 1,
		JanitorInterval:    time.Hour,
	})
	defer manager.Close()

	created, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	session := manager.sessions[created.SessionID]
	session.desiredLeaseExpiresAt = time.Now().Add(-time.Second)
	session.confirmedLeaseExpiresAt = session.desiredLeaseExpiresAt
	manager.mu.Unlock()

	replacement, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "after-expiry",
		SessionID:       created.SessionID,
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replacement.Created || replacement.SessionID != created.SessionID {
		t.Fatalf("unexpected replacement result: %#v", replacement)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.created != 2 || backend.killed != 1 || len(backend.killedIDs) != 1 || backend.killedIDs[0] != "sandbox-1" {
		t.Fatalf(
			"expired sandbox was not replaced: created=%d killed=%d ids=%v",
			backend.created,
			backend.killed,
			backend.killedIDs,
		)
	}
}

func TestTerminalManagerCloseWaitsForSandboxCreation(t *testing.T) {
	t.Parallel()
	createStarted := make(chan struct{})
	createRelease := make(chan struct{})
	backend := &fakeE2BBackend{}
	backend.createFn = func(context.Context, string, int) (*e2b.Sandbox, error) {
		close(createStarted)
		<-createRelease
		return &e2b.Sandbox{ID: "created-during-close"}, nil
	}
	manager := newTestTerminalManager(backend, 1)
	execDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
		execDone <- err
	}()
	<-createStarted

	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("manager closed before sandbox creation completed")
	case <-time.After(30 * time.Millisecond):
	}
	close(createRelease)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("manager did not close after sandbox creation completed")
	}
	<-execDone
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.killed != 1 || len(backend.killedIDs) != 1 || backend.killedIDs[0] != "created-during-close" {
		t.Fatalf("created sandbox was not cleaned up: killed=%d ids=%v", backend.killed, backend.killedIDs)
	}
}

func TestCommandDeadlineExtendsRemoteTimeoutWithoutExtendingLease(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManager(backend, 1)
	defer manager.Close()

	lease := 2
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := manager.Execute(ctx, terminalExecRequest{
		Command:     "long-command",
		LeaseTTLSec: &lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	timeouts := append([]int(nil), backend.timeouts...)
	backend.mu.Unlock()
	if len(timeouts) != 1 || timeouts[0] < 19 {
		t.Fatalf("command deadline did not protect the remote sandbox: %v", timeouts)
	}
	leaseDuration := time.UnixMilli(result.LeaseExpiresUnixMS).Sub(startedAt)
	if leaseDuration < time.Second || leaseDuration > 4*time.Second {
		t.Fatalf("command deadline changed the user lease: %s", leaseDuration)
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
	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("concurrent creation should use one reservation, got %d", got)
	}
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
	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("destroying session should keep its reservation, got %d", got)
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
		killed := backend.killed
		backend.mu.Unlock()
		return killed == 1 && manager.ActiveSessionCount() == 0
	}, "sandbox cleanup did not finish after sibling drained")
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

func TestTerminalResourceTimeoutReleasesCapacity(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	manager := newTestTerminalManagerWithCapacity(backend, 1, 1)
	defer manager.Close()

	seed, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "resource-timeout",
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.runFn = func(context.Context, *e2b.Sandbox, string, int) (e2b.CommandResult, error) {
		return e2b.CommandResult{}, context.DeadlineExceeded
	}
	if _, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/workspace/file.txt",
		Action:    terminalResourceActionRead,
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected resource deadline exceeded, got %v", err)
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("resource timeout leaked capacity: %d", got)
	}
	backend.mu.Lock()
	killed := backend.killed
	backend.mu.Unlock()
	if killed != 1 {
		t.Fatalf("resource timeout should kill one sandbox, got %d", killed)
	}
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

func TestTerminalResourceSandboxExportUsesSandboxCommand(t *testing.T) {
	t.Parallel()
	var exportCommand string
	backend := &fakeE2BBackend{}
	backend.runFn = func(_ context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		switch {
		case command == "seed":
			return e2b.CommandResult{}, nil
		case strings.Contains(command, "mimetypes.guess_type"):
			return e2b.CommandResult{Stdout: `{"mime_type":"text/plain","size_bytes":5}`}, nil
		default:
			exportCommand = command
			return e2b.CommandResult{}, nil
		}
	}
	backend.openFn = func(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error) {
		t.Fatal("sandbox export must not open the file through the worker")
		return e2b.FileReader{}, nil
	}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    10,
		OutputLimitBytes:   1024,
		ExportMode:         terminalExportModeSandbox,
		SessionMaxInflight: 1,
	})
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionExport,
		SignedURL: "https://uploads.example.com/object?signature=secret",
		Headers:   map[string]string{"X-Test-Export": "forwarded"},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(exportCommand, "python3 -c ") {
		t.Fatalf("expected sandbox Python upload command, got %q", exportCommand)
	}
}

func TestWorkerExportEnforcesLimitOnUnknownLengthStream(t *testing.T) {
	t.Parallel()
	backend := &fakeE2BBackend{}
	backend.runFn = func(_ context.Context, _ *e2b.Sandbox, command string, _ int) (e2b.CommandResult, error) {
		if command == "seed" {
			return e2b.CommandResult{}, nil
		}
		return e2b.CommandResult{Stdout: `{"mime_type":"text/plain","size_bytes":5}`}, nil
	}
	backend.openFn = func(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error) {
		return e2b.FileReader{
			Body:     io.NopCloser(strings.NewReader("123456")),
			MIMEType: "text/plain",
			Size:     -1,
		}, nil
	}
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		Backend:            backend,
		Template:           "terminal-template",
		LeaseMinSec:        1,
		LeaseMaxSec:        60,
		LeaseDefaultSec:    10,
		OutputLimitBytes:   1024,
		ExportMaxBytes:     5,
		ExportMode:         terminalExportModeWorker,
		SessionMaxInflight: 1,
	})
	defer manager.Close()
	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	uploadedCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		uploaded, _ := io.ReadAll(req.Body)
		uploadedCh <- uploaded
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, err = manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/growing.txt",
		Action:    terminalResourceActionExport,
		SignedURL: server.URL,
	})
	if terminalErrorCode(err) != terminalResourceCodeFileTooLarge {
		t.Fatalf("expected file_too_large, got %v", err)
	}
	var uploaded []byte
	select {
	case uploaded = <-uploadedCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for export upload")
	}
	if len(uploaded) > 5 {
		t.Fatalf("export uploaded %d bytes past the configured limit", len(uploaded))
	}
}

func TestSandboxExportCommandStreamsFile(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	filePath := filepath.Join(t.TempDir(), "export.txt")
	if err := os.WriteFile(filePath, []byte("sandbox-direct"), 0o600); err != nil {
		t.Fatal(err)
	}
	var uploaded []byte
	var receivedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		uploaded, _ = io.ReadAll(req.Body)
		receivedHeader = req.Header.Get("X-Test-Export")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	command, err := buildSandboxExportCommand(
		filePath,
		server.URL+"/upload?signature=secret",
		map[string]string{"X-Test-Export": "forwarded"},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("/bin/bash", "-l", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox export command failed: %v: %s", err, output)
	}
	if string(uploaded) != "sandbox-direct" || receivedHeader != "forwarded" {
		t.Fatalf("unexpected upload body=%q header=%q", uploaded, receivedHeader)
	}
}

func TestSandboxExportCommandEnforcesLimit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	filePath := filepath.Join(t.TempDir(), "export-too-large.txt")
	if err := os.WriteFile(filePath, []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		uploaded, _ = io.ReadAll(req.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	command, err := buildSandboxExportCommand(filePath, server.URL, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("/bin/bash", "-l", "-c", command).CombinedOutput()
	if err == nil || !strings.Contains(string(output), terminalExportLimitMarker) {
		t.Fatalf("expected sandbox export limit failure, err=%v output=%s", err, output)
	}
	if len(uploaded) > 5 {
		t.Fatalf("sandbox export uploaded %d bytes past the configured limit", len(uploaded))
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
