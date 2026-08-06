package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTerminalSessionCapacityRejectsNewSessionsButAllowsExisting(t *testing.T) {
	var mu sync.Mutex
	createCalls := 0
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			mu.Lock()
			createCalls++
			mu.Unlock()
			return dockerCommandResult{ExitCode: 0}
		case "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			if len(args) > 2 && args[2] == "python3" {
				return dockerCommandResult{Stdout: `{"mime_type":"text/plain","size_bytes":5}`, ExitCode: 0}
			}
			return dockerCommandResult{Stdout: "ok", ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:        60,
		LeaseMaxSec:        1800,
		LeaseDefaultSec:    60,
		OutputLimitBytes:   1024,
		SessionMaxInflight: 2,
		MaxActiveSessions:  2,
	})
	defer manager.Close()

	for _, sessionID := range []string{"session-a", "session-b"} {
		if _, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "seed",
			SessionID:       sessionID,
			CreateIfMissing: true,
		}); err != nil {
			t.Fatalf("create %s: %v", sessionID, err)
		}
	}
	if got := manager.ActiveSessionCount(); got != 2 {
		t.Fatalf("expected active session count 2, got %d", got)
	}

	_, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "overflow",
		SessionID:       "session-c",
		CreateIfMissing: true,
	})
	assertTerminalSessionCapacityError(t, err)

	mu.Lock()
	gotCreateCalls := createCalls
	mu.Unlock()
	if gotCreateCalls != 2 {
		t.Fatalf("capacity rejection must happen before docker create: got %d create calls", gotCreateCalls)
	}

	_, err = manager.Execute(context.Background(), terminalExecRequest{
		Command:   "missing",
		SessionID: "session-missing",
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found before capacity check, got %v", err)
	}

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "reuse",
		SessionID: "session-a",
	}); err != nil {
		t.Fatalf("existing session exec at capacity failed: %v", err)
	}
	resource, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "session-a",
		FilePath:  "/workspace/existing.txt",
		Action:    terminalResourceActionValidate,
	})
	if err != nil {
		t.Fatalf("existing session resource at capacity failed: %v", err)
	}
	if resource.SizeBytes != 5 {
		t.Fatalf("unexpected resource result: %#v", resource)
	}
}

func TestTerminalSessionCapacityCountsCreatingSession(t *testing.T) {
	createStarted := make(chan struct{})
	createRelease := make(chan struct{})
	var startOnce sync.Once

	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			startOnce.Do(func() { close(createStarted) })
			select {
			case <-ctx.Done():
				return dockerCommandResult{Err: ctx.Err()}
			case <-createRelease:
				return dockerCommandResult{ExitCode: 0}
			}
		case "start", "exec", "rm":
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})
	defer manager.Close()

	creatorDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "seed",
			SessionID:       "creating",
			CreateIfMissing: true,
		})
		creatorDone <- err
	}()

	select {
	case <-createStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for docker create")
	}
	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("creating session should consume capacity, got %d", got)
	}

	_, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "overflow",
		SessionID:       "second",
		CreateIfMissing: true,
	})
	assertTerminalSessionCapacityError(t, err)

	close(createRelease)
	if err := <-creatorDone; err != nil {
		t.Fatalf("creator failed: %v", err)
	}
}

func TestTerminalSessionCapacityReleasesAfterCreateFailure(t *testing.T) {
	var mu sync.Mutex
	createCalls := 0
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			mu.Lock()
			createCalls++
			call := createCalls
			mu.Unlock()
			if call == 1 {
				return dockerCommandResult{Stderr: "image pull failed", ExitCode: 125}
			}
			return dockerCommandResult{ExitCode: 0}
		case "start", "exec", "rm":
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})
	defer manager.Close()

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "fail",
		SessionID:       "failed",
		CreateIfMissing: true,
	}); err == nil {
		t.Fatal("expected first create to fail")
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("failed create leaked capacity: %d", got)
	}

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "succeed",
		SessionID:       "replacement",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("capacity was not reusable after create failure: %v", err)
	}
}

func TestTerminalSessionCapacityReleasesAfterContainerStartFailure(t *testing.T) {
	var mu sync.Mutex
	startCalls := 0
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "exec", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "start":
			mu.Lock()
			startCalls++
			call := startCalls
			mu.Unlock()
			if call == 1 {
				return dockerCommandResult{Stderr: "OCI runtime create failed", ExitCode: 1}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})
	defer manager.Close()

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "fail",
		SessionID:       "start-failure",
		CreateIfMissing: true,
	}); err == nil || !strings.Contains(err.Error(), "docker start failed") {
		t.Fatalf("expected docker start failure, got %v", err)
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("start failure leaked capacity: %d", got)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "replacement",
		SessionID:       "replacement",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("capacity was not reusable after start failure: %v", err)
	}
}

func TestTerminalSessionCapacityHeldUntilContainerCleanupReturns(t *testing.T) {
	rmStarted := make(chan struct{}, 1)
	rmRelease := make(chan struct{})
	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "exec":
			return dockerCommandResult{ExitCode: 0}
		case "rm":
			select {
			case rmStarted <- struct{}{}:
			default:
			}
			select {
			case <-ctx.Done():
				return dockerCommandResult{Err: ctx.Err()}
			case <-rmRelease:
				return dockerCommandResult{ExitCode: 0}
			}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "expiring",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	manager.mu.Lock()
	manager.sessions["expiring"].leaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	cleanupDone := make(chan struct{})
	go func() {
		manager.cleanupExpiredSessions()
		close(cleanupDone)
	}()
	select {
	case <-rmStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for container cleanup")
	}

	if got := manager.ActiveSessionCount(); got != 1 {
		t.Fatalf("cleanup-in-progress session should consume capacity, got %d", got)
	}
	_, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "overflow",
		SessionID:       "replacement",
		CreateIfMissing: true,
	})
	assertTerminalSessionCapacityError(t, err)

	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("close returned while container cleanup was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(rmRelease)
	select {
	case <-cleanupDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cleanup to finish")
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

func TestTerminalSessionCapacityReleasedAfterContainerCleanupFailure(t *testing.T) {
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "exec":
			return dockerCommandResult{ExitCode: 0}
		case "rm":
			return dockerCommandResult{Err: errors.New("docker daemon unavailable")}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})
	defer manager.Close()

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "cleanup-failure",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	manager.mu.Lock()
	manager.sessions["cleanup-failure"].leaseExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	manager.cleanupExpiredSessions()

	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("cleanup failure retained capacity: %d", got)
	}
	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "replacement",
		SessionID:       "replacement",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("capacity was not reusable after cleanup failure: %v", err)
	}
}

func TestTerminalSessionResourceTimeoutReleasesCapacity(t *testing.T) {
	resourceProbe := false
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			if resourceProbe {
				return dockerCommandResult{Err: context.DeadlineExceeded}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})
	defer manager.Close()

	if _, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:         "seed",
		SessionID:       "resource-timeout",
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	resourceProbe = true
	if _, err := manager.ResolveResource(context.Background(), terminalResourceRequest{
		SessionID: "resource-timeout",
		FilePath:  "/workspace/file.txt",
		Action:    terminalResourceActionRead,
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected resource deadline exceeded, got %v", err)
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("resource timeout leaked capacity: %d", got)
	}
}

func TestTerminalSessionCapacityZeroIsUnlimitedAndCloseReleasesAll(t *testing.T) {
	var mu sync.Mutex
	removeCalls := 0
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "exec":
			return dockerCommandResult{ExitCode: 0}
		case "rm":
			mu.Lock()
			removeCalls++
			mu.Unlock()
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 0,
	})
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

	manager.Close()
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("close leaked capacity: %d", got)
	}
	mu.Lock()
	gotRemoveCalls := removeCalls
	mu.Unlock()
	if gotRemoveCalls != 3 {
		t.Fatalf("expected three cleanup calls, got %d", gotRemoveCalls)
	}
}

func TestTerminalSessionCloseWaitsForPendingContainerCreation(t *testing.T) {
	createStarted := make(chan struct{})
	createRelease := make(chan struct{})
	var startOnce sync.Once
	var mu sync.Mutex
	removeCalls := 0
	stubDockerConcurrency(t, func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			startOnce.Do(func() { close(createStarted) })
			select {
			case <-ctx.Done():
				return dockerCommandResult{Err: ctx.Err()}
			case <-createRelease:
				return dockerCommandResult{ExitCode: 0}
			}
		case "start", "exec":
			return dockerCommandResult{ExitCode: 0}
		case "rm":
			mu.Lock()
			removeCalls++
			mu.Unlock()
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	})

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:       60,
		LeaseMaxSec:       1800,
		LeaseDefaultSec:   60,
		OutputLimitBytes:  1024,
		MaxActiveSessions: 1,
	})
	creatorDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), terminalExecRequest{
			Command:         "seed",
			SessionID:       "creating-on-close",
			CreateIfMissing: true,
		})
		creatorDone <- err
	}()
	select {
	case <-createStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for container create")
	}

	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("close returned before pending creation finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(createRelease)
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for close")
	}
	select {
	case <-creatorDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for creator")
	}
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("close leaked capacity: %d", got)
	}
	mu.Lock()
	gotRemoveCalls := removeCalls
	mu.Unlock()
	if gotRemoveCalls != 1 {
		t.Fatalf("expected one container cleanup, got %d", gotRemoveCalls)
	}
}

func assertTerminalSessionCapacityError(t *testing.T, err error) {
	t.Helper()
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionCapacityExceeded {
		t.Fatalf("expected session_capacity_exceeded, got %v", err)
	}
}
