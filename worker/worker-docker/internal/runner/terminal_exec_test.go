package runner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
)

func TestBuildCommandResultTerminalExecSuccess(t *testing.T) {
	originalRunTerminalExec := runTerminalExec
	t.Cleanup(func() {
		runTerminalExec = originalRunTerminalExec
	})

	runTerminalExec = func(_ context.Context, req terminalExecRequest) (terminalExecRunResult, error) {
		if req.Command != "echo hello" {
			t.Fatalf("unexpected command: %s", req.Command)
		}
		return terminalExecRunResult{
			SessionID:          "sess-1",
			Created:            true,
			Stdout:             "hello\n",
			Stderr:             "",
			ExitCode:           0,
			StdoutTruncated:    false,
			StderrTruncated:    false,
			LeaseExpiresUnixMS: 123456789,
		}, nil
	}

	payload := []byte(`{"command":"echo hello"}`)
	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-term-1",
		Capability:  "terminalExec",
		PayloadJson: payload,
	})

	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() != nil {
		t.Fatalf("expected success, got error %#v", result.GetError())
	}

	decoded := terminalExecRunResult{}
	if err := json.Unmarshal(result.GetPayloadJson(), &decoded); err != nil {
		t.Fatalf("expected valid terminalExec result payload, got %s", string(result.GetPayloadJson()))
	}
	if decoded.SessionID != "sess-1" || decoded.ExitCode != 0 || decoded.Stdout != "hello\n" {
		t.Fatalf("unexpected terminalExec result payload: %#v", decoded)
	}
}

func TestBuildCommandResultTerminalExecSessionErrors(t *testing.T) {
	originalRunTerminalExec := runTerminalExec
	t.Cleanup(func() {
		runTerminalExec = originalRunTerminalExec
	})

	runTerminalExec = func(_ context.Context, _ terminalExecRequest) (terminalExecRunResult, error) {
		return terminalExecRunResult{}, newTerminalExecError(terminalExecCodeSessionNotFound, terminalExecNoSessionMessage)
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-term-2",
		Capability:  "terminalExec",
		PayloadJson: []byte(`{"command":"echo hello","session_id":"missing"}`),
	})
	result := req.GetCommandResult()
	if result == nil || result.GetError() == nil {
		t.Fatalf("expected error result, got %#v", result)
	}
	if result.GetError().GetCode() != terminalExecCodeSessionNotFound {
		t.Fatalf("expected code=%s, got %s", terminalExecCodeSessionNotFound, result.GetError().GetCode())
	}
}

func TestTerminalSessionManagerSessionReuse(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	containerState := make(map[string]string)
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			containerName := argValue(args, "--name")
			containerState[containerName] = ""
			return dockerCommandResult{ExitCode: 0}
		case "start":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			containerName := args[1]
			command := args[4]
			switch command {
			case "set-persist-value":
				containerState[containerName] = "persisted\n"
				return dockerCommandResult{ExitCode: 0}
			case "get-persist-value":
				return dockerCommandResult{
					Stdout:   containerState[containerName],
					ExitCode: 0,
				}
			default:
				return dockerCommandResult{Stderr: "unexpected command", ExitCode: 1}
			}
		case "rm":
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	first, err := manager.Execute(context.Background(), terminalExecRequest{
		Command: "set-persist-value",
	})
	if err != nil {
		t.Fatalf("first execute failed: %v", err)
	}
	if !first.Created || first.SessionID == "" {
		t.Fatalf("expected created session, got %#v", first)
	}

	second, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "get-persist-value",
		SessionID: first.SessionID,
	})
	if err != nil {
		t.Fatalf("second execute failed: %v", err)
	}
	if second.Created {
		t.Fatalf("expected reuse session, got created=true")
	}
	if second.Stdout != "persisted\n" {
		t.Fatalf("expected persisted output, got %q", second.Stdout)
	}
}

func TestTerminalSessionManagerBusySession(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	blockCh := make(chan struct{})
	var lock sync.Mutex
	execCalls := 0

	runDockerCommand = func(ctx context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			lock.Lock()
			execCalls++
			lock.Unlock()
			if args[4] == "block-command" {
				select {
				case <-ctx.Done():
					return dockerCommandResult{Err: ctx.Err()}
				case <-blockCh:
					return dockerCommandResult{ExitCode: 0}
				}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	firstDone := make(chan terminalExecRunResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		result, err := manager.Execute(context.Background(), terminalExecRequest{
			Command: "block-command",
		})
		if err != nil {
			firstErr <- err
			return
		}
		firstDone <- result
	}()

	var sessionID string
	deadline := time.Now().Add(2 * time.Second)
	for sessionID == "" {
		manager.mu.Lock()
		for id := range manager.sessions {
			sessionID = id
		}
		manager.mu.Unlock()
		if sessionID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for session creation")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "any-command",
		SessionID: sessionID,
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionBusy {
		t.Fatalf("expected session_busy error, got %v", err)
	}

	close(blockCh)
	select {
	case err := <-firstErr:
		t.Fatalf("first command should succeed, got %v", err)
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting blocked command result")
	}

	lock.Lock()
	defer lock.Unlock()
	if execCalls < 1 {
		t.Fatalf("expected at least one exec call")
	}
}

func TestTerminalSessionManagerTimeoutReleasesSession(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	var calls [][]string
	runDockerCommand = func(ctx context.Context, args ...string) dockerCommandResult {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			<-ctx.Done()
			return dockerCommandResult{Err: ctx.Err()}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sessionID := "timeout-session"
	_, err := manager.Execute(timeoutCtx, terminalExecRequest{
		Command:         "timeout-command",
		SessionID:       sessionID,
		CreateIfMissing: true,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got err=%v", err)
	}

	_, err = manager.Execute(context.Background(), terminalExecRequest{
		Command:   "after-timeout",
		SessionID: sessionID,
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found after timeout cleanup, got %v", err)
	}

	if len(calls) < 3 {
		t.Fatalf("expected create/start/exec/rm docker calls, got %#v", calls)
	}
}

func TestTerminalSessionManagerLeaseNotReducedAndOutputTruncated(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "exec":
			if args[4] == "big-output" {
				return dockerCommandResult{
					Stdout:   "1234567890",
					Stderr:   "abcdefghij",
					ExitCode: 0,
				}
			}
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 4,
	})
	defer manager.Close()

	highLease := 120
	first, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:     "big-output",
		LeaseTTLSec: &highLease,
	})
	if err != nil {
		t.Fatalf("first execute failed: %v", err)
	}
	if !first.StdoutTruncated || !first.StderrTruncated {
		t.Fatalf("expected truncation flags, got %#v", first)
	}
	if first.Stdout != "1234" || first.Stderr != "abcd" {
		t.Fatalf("unexpected truncated output: %#v", first)
	}

	lowLease := 60
	second, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:     "big-output",
		SessionID:   first.SessionID,
		LeaseTTLSec: &lowLease,
	})
	if err != nil {
		t.Fatalf("second execute failed: %v", err)
	}
	if second.LeaseExpiresUnixMS < first.LeaseExpiresUnixMS {
		t.Fatalf("expected lease to be non-decreasing, first=%d second=%d", first.LeaseExpiresUnixMS, second.LeaseExpiresUnixMS)
	}
}

func TestTerminalSessionManagerInvalidLease(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		t.Fatalf("docker command should not be called on invalid lease: %#v", args)
		return dockerCommandResult{}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	invalidLease := 10
	_, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:     "echo hello",
		LeaseTTLSec: &invalidLease,
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeInvalidPayload {
		t.Fatalf("expected invalid_payload error, got %v", err)
	}
}

func TestTerminalSessionManagerSessionNotFoundWithoutCreate(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		t.Fatalf("docker command should not be called when session is missing: %#v", args)
		return dockerCommandResult{}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
	})
	defer manager.Close()

	_, err := manager.Execute(context.Background(), terminalExecRequest{
		Command:   "pwd",
		SessionID: "missing-session",
	})
	var terminalErr *terminalExecError
	if !errors.As(err, &terminalErr) || terminalErr.Code() != terminalExecCodeSessionNotFound {
		t.Fatalf("expected session_not_found, got %v", err)
	}
}

func TestTerminalExecDockerCreateArgs(t *testing.T) {
	got := terminalExecDockerCreateArgs("container-a", "python:slim", "256m", "1.0", 128)
	want := []string{
		"create",
		"--name", "container-a",
		"--label", pythonExecManagedLabel,
		"--label", terminalExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--memory", "256m",
		"--cpus", "1.0",
		"--pids-limit", "128",
		"python:slim",
		"sh",
		"-lc",
		terminalExecIdleCommand,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected create args:\nwant=%#v\ngot=%#v", want, got)
	}
}

func argValue(args []string, key string) string {
	for i := 0; i < len(args)-1; i++ {
		if strings.TrimSpace(args[i]) == key {
			return args[i+1]
		}
	}
	return ""
}
