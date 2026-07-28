package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubDockerConcurrency installs a runDockerCommand stub and restores the
// original when the test finishes.
func stubDockerConcurrency(t *testing.T, fn func(ctx context.Context, args ...string) dockerCommandResult) {
	t.Helper()
	original := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = original
	})
	runDockerCommand = fn
}

func TestTerminalSessionConcurrentExecWithinLimit(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			if args[4] == "block-command" {
				started <- struct{}{}
				select {
				case <-ctx.Done():
					return dockerCommandResult{Err: ctx.Err()}
				case <-release:
					return dockerCommandResult{Stdout: "released", ExitCode: 0}
				}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:        60,
		LeaseMaxSec:        1800,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024 * 1024,
		SessionMaxInflight: 2,
	})
	defer manager.Close()

	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatalf("seed execute failed: %v", err)
	}

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, execErr := manager.Execute(context.Background(), terminalExecRequest{
				Command:   "block-command",
				SessionID: seed.SessionID,
			})
			results <- execErr
		}()
	}

	// Both commands must be genuinely in flight at the same time.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for concurrent command %d to start", i+1)
		}
	}

	// A third command exceeds the per-session limit.
	_, err = manager.Execute(context.Background(), terminalExecRequest{
		Command:   "overflow",
		SessionID: seed.SessionID,
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionBusy {
		t.Fatalf("expected session_busy beyond limit, got %v", err)
	}

	close(release)
	for i := 0; i < 2; i++ {
		if execErr := <-results; execErr != nil {
			t.Fatalf("concurrent command failed: %v", execErr)
		}
	}
}

func TestTerminalSessionDefaultLimitStaysSerial(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			if args[4] == "block-command" {
				started <- struct{}{}
				select {
				case <-ctx.Done():
					return dockerCommandResult{Err: ctx.Err()}
				case <-release:
					return dockerCommandResult{ExitCode: 0}
				}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	// No SessionMaxInflight configured: must behave exactly as before.
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatalf("seed execute failed: %v", err)
	}

	go func() {
		_, _ = manager.Execute(context.Background(), terminalExecRequest{
			Command:   "block-command",
			SessionID: seed.SessionID,
		})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for blocking command")
	}

	_, err = manager.Execute(context.Background(), terminalExecRequest{
		Command:   "second",
		SessionID: seed.SessionID,
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionBusy {
		t.Fatalf("expected session_busy at default limit, got %v", err)
	}
	close(release)
}

func TestTerminalSessionReadinessGateBlocksUntilContainerExists(t *testing.T) {
	createGate := make(chan struct{})

	var mu sync.Mutex
	events := make([]string, 0, 8)
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			record("create-begin")
			<-createGate
			record("create-end")
			return dockerCommandResult{ExitCode: 0}
		case "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			record("exec")
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:        60,
		LeaseMaxSec:        1800,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024 * 1024,
		SessionMaxInflight: 2,
	})
	defer manager.Close()

	creatorDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "creator",
			SessionID:       "sess-gate",
			CreateIfMissing: true,
		})
		creatorDone <- err
	}()

	// Wait until the creator is inside container creation.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0 && events[0] == "create-begin"
	}, "creator did not reach container create")

	waiterDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "waiter",
			SessionID:       "sess-gate",
			CreateIfMissing: true,
		})
		waiterDone <- err
	}()

	// Give the waiter a chance to (incorrectly) run against a missing container.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	for _, event := range events {
		if event == "exec" {
			mu.Unlock()
			t.Fatalf("command executed before container was ready: %v", events)
		}
	}
	mu.Unlock()

	close(createGate)
	if err := <-creatorDone; err != nil {
		t.Fatalf("creator failed: %v", err)
	}
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[0] != "create-begin" || events[1] != "create-end" {
		t.Fatalf("expected creation to complete before any exec, got %v", events)
	}
}

func TestTerminalSessionCreateFailureReachesAllWaiters(t *testing.T) {
	createGate := make(chan struct{})

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			<-createGate
			return dockerCommandResult{Stderr: "image pull failed", ExitCode: 125}
		case "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			t.Errorf("exec must not run when container creation failed")
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:        60,
		LeaseMaxSec:        1800,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024 * 1024,
		SessionMaxInflight: 3,
	})
	defer manager.Close()

	errCh := make(chan error, 2)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "creator",
			SessionID:       "sess-fail",
			CreateIfMissing: true,
		})
		errCh <- err
	}()

	waitFor(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.sessions["sess-fail"] != nil
	}, "session was never registered")

	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "waiter",
			SessionID:       "sess-fail",
			CreateIfMissing: true,
		})
		errCh <- err
	}()

	// Let the waiter park on the readiness gate before creation fails.
	time.Sleep(100 * time.Millisecond)
	close(createGate)

	for i := 0; i < 2; i++ {
		err := <-errCh
		if err == nil {
			t.Fatalf("expected creation failure to propagate to caller %d", i+1)
		}
		if !strings.Contains(err.Error(), "docker create failed") {
			t.Fatalf("expected docker create failure, got %v", err)
		}
	}

	waitFor(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.sessions) == 0
	}, "failed session was not cleaned up")
}

func TestTerminalSessionDeferredDestroyKeepsSiblingAlive(t *testing.T) {
	siblingRelease := make(chan struct{})
	siblingStarted := make(chan struct{})

	var mu sync.Mutex
	removeCalls := 0

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start":
			return dockerCommandResult{ExitCode: 0}
		case "rm":
			mu.Lock()
			removeCalls++
			mu.Unlock()
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			switch args[4] {
			case "sibling":
				close(siblingStarted)
				select {
				case <-ctx.Done():
					return dockerCommandResult{Err: ctx.Err()}
				case <-siblingRelease:
					return dockerCommandResult{Stdout: "sibling-ok", ExitCode: 0}
				}
			case "doomed":
				<-ctx.Done()
				return dockerCommandResult{Err: ctx.Err()}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:        60,
		LeaseMaxSec:        1800,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024 * 1024,
		SessionMaxInflight: 2,
	})
	defer manager.Close()

	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatalf("seed execute failed: %v", err)
	}

	siblingResult := make(chan terminalExecRunResult, 1)
	siblingErr := make(chan error, 1)
	go func() {
		result, execErr := manager.Execute(context.Background(), terminalExecRequest{
			Command:   "sibling",
			SessionID: seed.SessionID,
		})
		if execErr != nil {
			siblingErr <- execErr
			return
		}
		siblingResult <- result
	}()

	select {
	case <-siblingStarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("sibling command never started")
	}

	// This command times out and asks for the session to be destroyed.
	doomedCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = manager.Execute(doomedCtx, terminalExecRequest{
		Command:   "doomed",
		SessionID: seed.SessionID,
	})
	if err == nil {
		t.Fatalf("expected doomed command to fail")
	}

	// The container must survive while the sibling is still running.
	mu.Lock()
	if removeCalls != 0 {
		mu.Unlock()
		t.Fatalf("container removed while a sibling command was still in flight")
	}
	mu.Unlock()

	close(siblingRelease)
	select {
	case execErr := <-siblingErr:
		t.Fatalf("sibling command should have survived, got %v", execErr)
	case result := <-siblingResult:
		if result.Stdout != "sibling-ok" {
			t.Fatalf("unexpected sibling output %q", result.Stdout)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("sibling command did not finish")
	}

	// Once drained, the session is gone and the container is reclaimed.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return removeCalls > 0
	}, "container was never removed after inflight drained")

	manager.mu.Lock()
	_, stillPresent := manager.sessions[seed.SessionID]
	manager.mu.Unlock()
	if stillPresent {
		t.Fatalf("destroyed session should have been dropped")
	}
}

func TestCleanupExpiredSessionsSkipsInflight(t *testing.T) {
	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		return dockerCommandResult{ExitCode: 0}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	expired := time.Now().Add(-time.Minute)
	manager.mu.Lock()
	manager.sessions["busy-session"] = readyTerminalSession("busy-session", "container-busy", expired, 1)
	manager.sessions["idle-session"] = readyTerminalSession("idle-session", "container-idle", expired, 0)
	manager.mu.Unlock()

	manager.cleanupExpiredSessions()

	manager.mu.Lock()
	_, busyPresent := manager.sessions["busy-session"]
	_, idlePresent := manager.sessions["idle-session"]
	manager.mu.Unlock()

	if !busyPresent {
		t.Fatalf("session with inflight commands must not be reclaimed")
	}
	if idlePresent {
		t.Fatalf("idle expired session should have been reclaimed")
	}

	// Draining the last command makes it reclaimable.
	manager.mu.Lock()
	manager.sessions["busy-session"].inflight = 0
	manager.mu.Unlock()

	manager.cleanupExpiredSessions()

	manager.mu.Lock()
	_, stillPresent := manager.sessions["busy-session"]
	manager.mu.Unlock()
	if stillPresent {
		t.Fatalf("drained expired session should have been reclaimed")
	}
}

func TestTerminalExecAndResourceRunConcurrently(t *testing.T) {
	execRelease := make(chan struct{})
	execStarted := make(chan struct{})
	probeStarted := make(chan struct{})

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			// terminalResource probes run python3; terminalExec runs sh.
			if args[2] == "python3" {
				close(probeStarted)
				return dockerCommandResult{
					Stdout:   `{"mime_type":"text/plain","size_bytes":5}`,
					ExitCode: 0,
				}
			}
			if args[4] == "block-command" {
				close(execStarted)
				select {
				case <-ctx.Done():
					return dockerCommandResult{Err: ctx.Err()}
				case <-execRelease:
					return dockerCommandResult{ExitCode: 0}
				}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:        60,
		LeaseMaxSec:        1800,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024 * 1024,
		SessionMaxInflight: 2,
	})
	defer manager.Close()

	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatalf("seed execute failed: %v", err)
	}

	go func() {
		_, _ = manager.Execute(context.Background(), terminalExecRequest{
			Command:   "block-command",
			SessionID: seed.SessionID,
		})
	}()

	select {
	case <-execStarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("terminalExec command never started")
	}

	// terminalResource must not be blocked by the in-flight terminalExec.
	result, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionValidate,
	})
	if err != nil {
		t.Fatalf("concurrent terminalResource failed: %v", err)
	}
	if result.MIMEType != "text/plain" || result.SizeBytes != 5 {
		t.Fatalf("unexpected resource result %#v", result)
	}

	select {
	case <-probeStarted:
	default:
		t.Fatalf("resource probe did not run")
	}

	close(execRelease)
}

func TestResolveResourceRejectsBeyondSessionLimit(t *testing.T) {
	execRelease := make(chan struct{})
	execStarted := make(chan struct{})

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			if args[4] == "block-command" {
				close(execStarted)
				select {
				case <-ctx.Done():
					return dockerCommandResult{Err: ctx.Err()}
				case <-execRelease:
					return dockerCommandResult{ExitCode: 0}
				}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	seed, err := manager.Execute(context.Background(), terminalExecRequest{Command: "seed"})
	if err != nil {
		t.Fatalf("seed execute failed: %v", err)
	}

	go func() {
		_, _ = manager.Execute(context.Background(), terminalExecRequest{
			Command:   "block-command",
			SessionID: seed.SessionID,
		})
	}()

	select {
	case <-execStarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("terminalExec command never started")
	}

	_, err = manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: seed.SessionID,
		FilePath:  "/tmp/hello.txt",
		Action:    terminalResourceActionValidate,
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionBusy {
		t.Fatalf("expected session_busy at default limit, got %v", err)
	}
	close(execRelease)
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s", message)
}
