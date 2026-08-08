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

// readyTerminalSession builds a session that has already finished container
// creation, so tests can seed the manager without going through Execute.
func readyTerminalSession(sessionID string, containerName string, leaseExpiresAt time.Time, inflight int) *terminalSession {
	ready := make(chan struct{})
	close(ready)
	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	return &terminalSession{
		sessionID:      sessionID,
		containerName:  containerName,
		containerIP:    "172.20.0.2",
		leaseExpiresAt: leaseExpiresAt,
		inflight:       inflight,
		ready:          ready,
		proxyCtx:       proxyCtx,
		proxyCancel:    proxyCancel,
	}
}

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

	tests := []struct {
		name    string
		code    string
		message string
	}{
		{name: "not_found", code: terminalExecCodeSessionNotFound, message: terminalExecNoSessionMessage},
		{name: "capacity", code: terminalExecCodeSessionCapacityExceeded, message: terminalExecCapacityMessage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runTerminalExec = func(_ context.Context, _ terminalExecRequest) (terminalExecRunResult, error) {
				return terminalExecRunResult{}, newTerminalExecError(tc.code, tc.message)
			}

			req := buildCommandResult(&registryv1.CommandDispatch{
				CommandId:   "cmd-term-" + tc.name,
				Capability:  "terminalExec",
				PayloadJson: []byte(`{"command":"echo hello","session_id":"missing"}`),
			})
			result := req.GetCommandResult()
			if result == nil || result.GetError() == nil {
				t.Fatalf("expected error result, got %#v", result)
			}
			if result.GetError().GetCode() != tc.code || result.GetError().GetMessage() != tc.message {
				t.Fatalf("unexpected command error: %#v", result.GetError())
			}
		})
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
	if got := manager.ActiveSessionCount(); got != 0 {
		t.Fatalf("timed out session leaked capacity: %d", got)
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

func TestNewTerminalSessionManagerUsesConfiguredResourceLimits(t *testing.T) {
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024 * 1024,
		DockerImage:      "python:slim",
		MemoryLimit:      "512m",
		CPULimit:         "0.5",
		PidsLimit:        256,
	})
	defer manager.Close()

	if manager.memoryLimit != "512m" {
		t.Fatalf("expected memoryLimit=512m, got %q", manager.memoryLimit)
	}
	if manager.cpuLimit != "0.5" {
		t.Fatalf("expected cpuLimit=0.5, got %q", manager.cpuLimit)
	}
	if manager.pidsLimit != 256 {
		t.Fatalf("expected pidsLimit=256, got %d", manager.pidsLimit)
	}
}

func TestTerminalExecDockerCreateArgs(t *testing.T) {
	containerName := terminalSessionResourceName("session-a")
	got := terminalExecDockerCreateArgs(containerName, "python:slim", "256m", "1.0", 128)
	want := []string{
		"create",
		"--name", containerName,
		"--label", pythonExecManagedLabel,
		"--label", terminalExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--label", terminalExecSessionLabelKey + "=" + terminalSessionIDHash("session-a"),
		"--label", terminalExecSchemaLabelKey + "=" + terminalExecSchemaVersion,
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

func TestTerminalSessionExpiryDuringContainerCreationRemovesLateContainer(t *testing.T) {
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	firstRemove := make(chan struct{})
	var removeOnce sync.Once
	var removeMu sync.Mutex
	removeCalls := 0
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create":
			close(createStarted)
			<-releaseCreate
			return dockerCommandResult{ExitCode: 0}
		case "start":
			return dockerCommandResult{ExitCode: 0}
		case "rm":
			removeMu.Lock()
			removeCalls++
			removeMu.Unlock()
			removeOnce.Do(func() { close(firstRemove) })
			return dockerCommandResult{ExitCode: 0}
		default:
			return dockerCommandResult{ExitCode: 1, Stderr: "unexpected docker operation"}
		}
	})
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	manager.mu.Lock()
	session, err := manager.newSessionLocked("session-expire-during-create", time.Now().Add(50*time.Millisecond))
	manager.mu.Unlock()
	if err != nil {
		t.Fatalf("create pending session: %v", err)
	}
	readyErr := make(chan error, 1)
	go func() { readyErr <- manager.awaitSessionReady(context.Background(), session, true) }()
	<-createStarted
	waiterErr := make(chan error, 1)
	go func() { waiterErr <- manager.awaitSessionReady(context.Background(), session, false) }()
	select {
	case <-firstRemove:
	case <-time.After(time.Second):
		t.Fatalf("lease timer did not attempt cleanup during container creation")
	}
	close(releaseCreate)
	for role, errCh := range map[string]<-chan error{"creator": readyErr, "waiter": waiterErr} {
		if err := <-errCh; err == nil {
			t.Fatalf("%s unexpectedly succeeded after lease expiry", role)
		} else {
			var terminalErr *terminalExecError
			if !errors.As(err, &terminalErr) || terminalErr.code != terminalExecCodeSessionNotFound {
				t.Fatalf("%s expected session_not_found after late create, got %v", role, err)
			}
		}
	}
	removeMu.Lock()
	gotRemoveCalls := removeCalls
	removeMu.Unlock()
	if gotRemoveCalls < 2 {
		t.Fatalf("late-created container was not removed again: rm calls=%d", gotRemoveCalls)
	}
}

func TestTerminalSessionLeaseTimerTracksExtension(t *testing.T) {
	containerRemoved := make(chan struct{}, 1)
	stubDockerConcurrency(t, func(_ context.Context, args ...string) dockerCommandResult {
		if len(args) > 0 && args[0] == "rm" {
			containerRemoved <- struct{}{}
		}
		return dockerCommandResult{ExitCode: 0}
	})
	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
	})
	defer manager.Close()

	oldExpiry := time.Now().Add(500 * time.Millisecond)
	session := readyTerminalSession("session-lease-timer", "container-lease-timer", oldExpiry, 0)
	manager.mu.Lock()
	manager.sessions[session.sessionID] = session
	manager.scheduleSessionLeaseTimerLocked(session)
	manager.mu.Unlock()

	time.Sleep(100 * time.Millisecond)
	newExpiry := time.Now().Add(900 * time.Millisecond)
	manager.mu.Lock()
	session.leaseExpiresAt = newExpiry
	manager.scheduleSessionLeaseTimerLocked(session)
	manager.mu.Unlock()

	waitUntilOldExpiry := time.Until(oldExpiry.Add(150 * time.Millisecond))
	if waitUntilOldExpiry > 0 {
		time.Sleep(waitUntilOldExpiry)
	}
	select {
	case <-session.proxyCtx.Done():
		t.Fatalf("lease timer ignored an extension")
	default:
	}

	select {
	case <-session.proxyCtx.Done():
	case <-time.After(time.Until(newExpiry.Add(time.Second))):
		t.Fatalf("lease timer did not cancel the session at the extended deadline")
	}
	select {
	case <-containerRemoved:
	case <-time.After(time.Second):
		t.Fatalf("lease timer did not remove the expired session container")
	}
	manager.mu.Lock()
	_, stillPresent := manager.sessions[session.sessionID]
	manager.mu.Unlock()
	if stillPresent {
		t.Fatalf("lease timer left the expired session registered")
	}
}

func TestTerminalSessionProxyTargetCachesContainerIP(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
	})

	inspectCalls := 0
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch args[0] {
		case "create", "start", "exec", "rm":
			return dockerCommandResult{ExitCode: 0}
		case "inspect":
			inspectCalls++
			return dockerCommandResult{Stdout: "172.30.0.8\n", ExitCode: 0}
		default:
			return dockerCommandResult{Stderr: "unexpected docker operation", ExitCode: 1}
		}
	}

	manager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      60,
		LeaseMaxSec:      1800,
		LeaseDefaultSec:  60,
		OutputLimitBytes: 1024,
		DockerNetwork:    terminalProxyDockerNetwork,
	})
	defer manager.Close()

	created, err := manager.Execute(context.Background(), terminalExecRequest{Command: "start-service"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if inspectCalls != 1 {
		t.Fatalf("expected one inspect after container start, got %d", inspectCalls)
	}
	for range 20 {
		target, err := manager.ResolveProxyTarget(context.Background(), created.SessionID, time.Now())
		if err != nil {
			t.Fatalf("resolve proxy target: %v", err)
		}
		if target.IP != "172.30.0.8" {
			t.Fatalf("unexpected cached target IP %q", target.IP)
		}
	}
	if inspectCalls != 1 {
		t.Fatalf("proxy requests repeated docker inspect: %d calls", inspectCalls)
	}
}

func TestTerminalExecDockerCreateArgsWithProxyNetwork(t *testing.T) {
	got := terminalExecDockerCreateArgsWithNetwork("container-a", "python:slim", "256m", "1.0", 128, terminalProxyDockerNetwork)
	if network := argValue(got, "--network"); network != terminalProxyDockerNetwork {
		t.Fatalf("expected proxy network %q, got %q in %#v", terminalProxyDockerNetwork, network, got)
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
