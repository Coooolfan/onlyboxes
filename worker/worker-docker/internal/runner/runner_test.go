package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRunReturnsContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Run(ctx, testConfig())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestTerminalRecoveryTimesOutBeforeWorkerBecomesReady(t *testing.T) {
	original := recoverTerminalSessionsFn
	t.Cleanup(func() { recoverTerminalSessionsFn = original })
	recoverTerminalSessionsFn = func(ctx context.Context, _ []*registryv1.TerminalSessionRecoveryCandidate) []*registryv1.TerminalSessionRecoveryResult {
		<-ctx.Done()
		return nil
	}
	_, err := recoverTerminalSessionsWithTimeout(context.Background(), 10*time.Millisecond, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected recovery deadline, got %v", err)
	}
}

func TestRunWaitsBeforeReconnectOnSessionFailure(t *testing.T) {
	originalWaitReconnect := waitReconnect
	waitCalls := 0
	waitReconnect = func(context.Context, time.Duration) error {
		waitCalls++
		return context.Canceled
	}
	defer func() {
		waitReconnect = originalWaitReconnect
	}()

	cfg := testConfig()
	cfg.ConsoleGRPCTarget = "127.0.0.1:1"
	cfg.CallTimeout = 5 * time.Millisecond

	err := Run(context.Background(), cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from mocked waitReconnect, got %v", err)
	}
	if waitCalls != 1 {
		t.Fatalf("expected waitReconnect to be called once, got %d", waitCalls)
	}
}

func TestBuildHelloCarriesWorkerSecretAndTerminalCapacity(t *testing.T) {
	originalActiveSessionCountFn := activeSessionCountFn
	activeSessionCountFn = func() int32 { return 7 }
	t.Cleanup(func() { activeSessionCountFn = originalActiveSessionCountFn })

	cfg := testConfig()
	cfg.TerminalMaxActiveSessions = 12
	hello, err := buildHello(cfg)
	if err != nil {
		t.Fatalf("buildHello failed: %v", err)
	}

	if hello.GetNodeId() != cfg.WorkerID {
		t.Fatalf("expected node_id=%s, got %s", cfg.WorkerID, hello.GetNodeId())
	}
	if hello.GetWorkerSecret() != cfg.WorkerSecret {
		t.Fatalf("expected worker_secret to be set")
	}
	capacity := hello.GetTerminalSessionCapacity()
	if capacity == nil {
		t.Fatal("expected terminal_session_capacity to be present")
	}
	if capacity.GetMaxActiveSessions() != 12 || capacity.GetActiveSessionCount() != 7 {
		t.Fatalf("unexpected terminal session capacity: %#v", capacity)
	}
	capabilityByName := make(map[string]int32, len(hello.GetCapabilities()))
	for _, capability := range hello.GetCapabilities() {
		capabilityByName[capability.GetName()] = capability.GetMaxInflight()
	}
	if len(capabilityByName) != 4 {
		t.Fatalf("expected four capabilities, got %#v", hello.GetCapabilities())
	}
	if capabilityByName[echoCapabilityName] != defaultMaxInflight {
		t.Fatalf("expected echo max_inflight=%d, got %d", defaultMaxInflight, capabilityByName[echoCapabilityName])
	}
	if capabilityByName[pythonExecCapabilityDeclared] != defaultMaxInflight {
		t.Fatalf("expected pythonExec max_inflight=%d, got %d", defaultMaxInflight, capabilityByName[pythonExecCapabilityDeclared])
	}
	if capabilityByName[terminalExecCapabilityDeclared] != defaultMaxInflight {
		t.Fatalf("expected terminalExec max_inflight=%d, got %d", defaultMaxInflight, capabilityByName[terminalExecCapabilityDeclared])
	}
	if capabilityByName[terminalResourceCapabilityDeclared] != defaultMaxInflight {
		t.Fatalf("expected terminalResource max_inflight=%d, got %d", defaultMaxInflight, capabilityByName[terminalResourceCapabilityDeclared])
	}
}

func TestBuildHelloAdvertisesExplicitUnlimitedTerminalCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.TerminalMaxActiveSessions = 0

	hello, err := buildHello(cfg)
	if err != nil {
		t.Fatalf("buildHello failed: %v", err)
	}
	capacity := hello.GetTerminalSessionCapacity()
	if capacity == nil || capacity.GetMaxActiveSessions() != 0 {
		t.Fatalf("expected explicit unlimited terminal capacity, got %#v", capacity)
	}
}

func TestValidateTerminalMaxActiveSessionsRejectsProtocolOverflow(t *testing.T) {
	if err := validateTerminalMaxActiveSessions(int(maxProtocolInt32)); err != nil {
		t.Fatalf("expected max protocol value to be valid: %v", err)
	}
	if err := validateTerminalMaxActiveSessions(int(maxProtocolInt32 + 1)); err == nil {
		t.Fatal("expected protocol overflow to be rejected")
	}
}

func TestBuildHelloRejectsNegativeActiveSessionCount(t *testing.T) {
	originalActiveSessionCountFn := activeSessionCountFn
	activeSessionCountFn = func() int32 { return -1 }
	t.Cleanup(func() { activeSessionCountFn = originalActiveSessionCountFn })

	if _, err := buildHello(testConfig()); err == nil {
		t.Fatal("expected negative active session count to be rejected")
	}
}

func TestRunSessionReceivesFailedPreconditionFromServer(t *testing.T) {
	originalActiveSessionCountFn := activeSessionCountFn
	activeSessionCountFn = func() int32 { return 3 }
	t.Cleanup(func() { activeSessionCountFn = originalActiveSessionCountFn })

	server := grpc.NewServer()
	fakeSvc := &fakeRegistryService{
		secretByNodeID: map[string]string{"worker-1": "secret-1"},
	}
	registryv1.RegisterWorkerRegistryServiceServer(server, fakeSvc)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer listener.Close()
	defer server.Stop()
	go func() {
		_ = server.Serve(listener)
	}()

	cfg := testConfig()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	cfg.CallTimeout = 2 * time.Second
	cfg.TerminalMaxActiveSessions = 5

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = runSession(ctx, cfg)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if got := atomic.LoadInt32(&fakeSvc.heartbeatCount); got < 2 {
		t.Fatalf("expected at least two heartbeats before rejection, got %d", got)
	}
	if got := atomic.LoadInt32(&fakeSvc.helloMaxActiveSessions); got != 5 {
		t.Fatalf("expected hello max_active_sessions=5, got %d", got)
	}
	if got := atomic.LoadInt32(&fakeSvc.helloActiveSessionCount); got != 3 {
		t.Fatalf("expected hello active_session_count=3, got %d", got)
	}
}

func TestHeartbeatLoopToleratesSingleAckTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.CallTimeout = 20 * time.Millisecond
	cfg.HeartbeatJitter = 0

	originalActiveSessionCountFn := activeSessionCountFn
	activeSessionCountFn = func() int32 { return 7 }
	defer func() {
		activeSessionCountFn = originalActiveSessionCountFn
	}()

	outbound := make(chan *registryv1.ConnectRequest, 4)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 1)
	sessionErrCh := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- heartbeatLoop(ctx, outbound, heartbeatAckCh, sessionErrCh, cfg, "session-1", 10*time.Millisecond)
	}()

	receivedHeartbeats := 0
	timeout := time.After(2 * time.Second)
	for receivedHeartbeats < 2 {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for second heartbeat")
		case req := <-outbound:
			if req.GetHeartbeat() == nil {
				t.Fatalf("expected heartbeat frame, got %#v", req.GetPayload())
			}
			if req.GetHeartbeat().GetActiveSessionCount() != 7 {
				t.Fatalf("expected active_session_count=7, got %d", req.GetHeartbeat().GetActiveSessionCount())
			}
			receivedHeartbeats++
			if receivedHeartbeats == 2 {
				heartbeatAckCh <- &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1}
				cancel()
			}
		}
	}

	err := <-doneCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled after single ack timeout recovery, got %v", err)
	}
}

func TestHeartbeatLoopFailsAfterTwoConsecutiveAckTimeouts(t *testing.T) {
	cfg := testConfig()
	cfg.CallTimeout = 20 * time.Millisecond
	cfg.HeartbeatJitter = 0

	outbound := make(chan *registryv1.ConnectRequest, 8)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 1)
	sessionErrCh := make(chan error, 1)

	err := heartbeatLoop(context.Background(), outbound, heartbeatAckCh, sessionErrCh, cfg, "session-1", 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded after two consecutive ack timeouts, got %v", err)
	}

	receivedHeartbeats := 0
	for {
		select {
		case req := <-outbound:
			if req.GetHeartbeat() != nil {
				receivedHeartbeats++
			}
		default:
			if receivedHeartbeats < 2 {
				t.Fatalf("expected at least two heartbeat frames before failure, got %d", receivedHeartbeats)
			}
			return
		}
	}
}

func TestRunRejectsMissingWorkerIdentity(t *testing.T) {
	cfg := testConfig()
	cfg.WorkerID = ""

	err := Run(context.Background(), cfg)
	if err == nil || err.Error() != "WORKER_ID is required" {
		t.Fatalf("expected missing WORKER_ID error, got %v", err)
	}
}

func TestRunRejectsTerminalCapacityOutsideProtocolRange(t *testing.T) {
	cfg := testConfig()
	cfg.TerminalMaxActiveSessions = int(maxProtocolInt32 + 1)

	err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "WORKER_TERMINAL_MAX_ACTIVE_SESSIONS") {
		t.Fatalf("expected terminal capacity validation error, got %v", err)
	}
}

func TestBuildCommandResultEcho(t *testing.T) {
	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-1",
		Capability:  "echo",
		PayloadJson: []byte(`{"message":"hello"}`),
	})

	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetCommandId() != "cmd-1" {
		t.Fatalf("expected command_id cmd-1, got %s", result.GetCommandId())
	}
	if string(result.GetPayloadJson()) != `{"message":"hello"}` {
		t.Fatalf("expected payload_json to roundtrip, got %s", string(result.GetPayloadJson()))
	}
	if result.GetCompletedUnixMs() == 0 {
		t.Fatalf("expected completed_unix_ms to be set")
	}
}

func TestBuildCommandResultUnsupportedCapability(t *testing.T) {
	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:  "cmd-2",
		Capability: "build",
	})

	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() == nil || result.GetError().GetCode() != "unsupported_capability" {
		t.Fatalf("expected unsupported_capability error, got %#v", result)
	}
}

func TestBuildCommandResultPythonExecSuccess(t *testing.T) {
	originalRunPythonExec := runPythonExec
	t.Cleanup(func() {
		runPythonExec = originalRunPythonExec
	})

	code := "print(\"123\")\nprint(\"234\")"
	payloadJSON, err := json.Marshal(pythonExecPayload{Code: code})
	if err != nil {
		t.Fatalf("marshal payload failed: %v", err)
	}

	called := false
	runPythonExec = func(_ context.Context, inputCode string) (pythonExecRunResult, error) {
		called = true
		if inputCode != code {
			t.Fatalf("unexpected code: %s", inputCode)
		}
		return pythonExecRunResult{
			Output:   "123\n234\n",
			Stderr:   "",
			ExitCode: 0,
		}, nil
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-py-1",
		Capability:  "pythonExec",
		PayloadJson: payloadJSON,
	})

	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() != nil {
		t.Fatalf("expected success, got error %#v", result.GetError())
	}
	if !called {
		t.Fatalf("expected python executor to be called")
	}

	decoded := pythonExecResult{}
	if err := json.Unmarshal(result.GetPayloadJson(), &decoded); err != nil {
		t.Fatalf("expected valid pythonExec result payload, got %s", string(result.GetPayloadJson()))
	}
	if decoded.Output != "123\n234\n" || decoded.Stderr != "" || decoded.ExitCode != 0 {
		t.Fatalf("unexpected pythonExec result payload: %#v", decoded)
	}
}

func TestBuildCommandResultPythonExecNonZeroExit(t *testing.T) {
	originalRunPythonExec := runPythonExec
	t.Cleanup(func() {
		runPythonExec = originalRunPythonExec
	})

	runPythonExec = func(_ context.Context, _ string) (pythonExecRunResult, error) {
		return pythonExecRunResult{
			Output:   "",
			Stderr:   "Traceback...",
			ExitCode: 1,
		}, nil
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-py-2",
		Capability:  "pythonexec",
		PayloadJson: []byte(`{"code":"raise Exception(\"boom\")"}`),
	})

	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() != nil {
		t.Fatalf("expected success result with non-zero exit, got error %#v", result.GetError())
	}

	decoded := pythonExecResult{}
	if err := json.Unmarshal(result.GetPayloadJson(), &decoded); err != nil {
		t.Fatalf("expected valid pythonExec result payload, got %s", string(result.GetPayloadJson()))
	}
	if decoded.ExitCode != 1 || decoded.Stderr != "Traceback..." {
		t.Fatalf("unexpected pythonExec non-zero result payload: %#v", decoded)
	}
}

func TestBuildCommandResultPythonExecInvalidPayload(t *testing.T) {
	tests := []struct {
		name        string
		payloadJSON []byte
	}{
		{name: "malformed", payloadJSON: []byte(`{"code":`)},
		{name: "missing_code", payloadJSON: []byte(`{}`)},
		{name: "blank_code", payloadJSON: []byte(`{"code":"   "}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := buildCommandResult(&registryv1.CommandDispatch{
				CommandId:   "cmd-py-invalid",
				Capability:  "pythonExec",
				PayloadJson: tc.payloadJSON,
			})

			result := req.GetCommandResult()
			if result == nil {
				t.Fatalf("expected command_result payload")
			}
			if result.GetError() == nil || result.GetError().GetCode() != "invalid_payload" {
				t.Fatalf("expected invalid_payload error, got %#v", result)
			}
		})
	}
}

func TestBuildCommandResultPythonExecExecutionFailed(t *testing.T) {
	originalRunPythonExec := runPythonExec
	t.Cleanup(func() {
		runPythonExec = originalRunPythonExec
	})

	runPythonExec = func(_ context.Context, _ string) (pythonExecRunResult, error) {
		return pythonExecRunResult{}, errors.New("docker is unavailable")
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:   "cmd-py-3",
		Capability:  "pythonExec",
		PayloadJson: []byte(`{"code":"print(1)"}`),
	})
	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() == nil || result.GetError().GetCode() != "execution_failed" {
		t.Fatalf("expected execution_failed error, got %#v", result)
	}
}

func TestBuildCommandResultPythonExecDeadlineExceeded(t *testing.T) {
	originalRunPythonExec := runPythonExec
	t.Cleanup(func() {
		runPythonExec = originalRunPythonExec
	})

	runPythonExec = func(ctx context.Context, _ string) (pythonExecRunResult, error) {
		<-ctx.Done()
		return pythonExecRunResult{}, ctx.Err()
	}

	req := buildCommandResult(&registryv1.CommandDispatch{
		CommandId:      "cmd-py-4",
		Capability:     "pythonExec",
		PayloadJson:    []byte(`{"code":"import time; time.sleep(10)"}`),
		DeadlineUnixMs: time.Now().Add(50 * time.Millisecond).UnixMilli(),
	})
	result := req.GetCommandResult()
	if result == nil {
		t.Fatalf("expected command_result payload")
	}
	if result.GetError() == nil || result.GetError().GetCode() != "deadline_exceeded" {
		t.Fatalf("expected deadline_exceeded error, got %#v", result)
	}
}

func TestPythonExecDockerCreateArgsIncludesResourceLimitsAndLabels(t *testing.T) {
	code := "print('hello')"
	containerName := "onlyboxes-pythonexec-test"
	got := pythonExecDockerCreateArgs(containerName, code)
	want := []string{
		"create",
		"--name", containerName,
		"--label", pythonExecManagedLabel,
		"--label", pythonExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--memory", defaultPythonExecMemoryLimit,
		"--cpus", defaultPythonExecCPULimit,
		"--pids-limit", fmt.Sprint(defaultPythonExecPidsLimit),
		"--entrypoint", "sh",
		defaultPythonExecDockerImage,
		"-c",
		`printf '%s' "$1" > /tmp/script.py && uv run /tmp/script.py`,
		"_",
		code,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected docker args:\nwant=%#v\ngot=%#v", want, got)
	}
}

func TestRunPythonExecInDockerWithImageUsesConfiguredImage(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	originalContainerNameFn := pythonExecContainerNameFn
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
		pythonExecContainerNameFn = originalContainerNameFn
	})

	pythonExecContainerNameFn = func() (string, error) {
		return "container-custom-image", nil
	}

	var gotCalls [][]string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		gotCalls = append(gotCalls, append([]string(nil), args...))
		return dockerCommandResult{
			Stderr:   "invalid image",
			ExitCode: 125,
		}
	}

	_, err := runPythonExecInDockerWithImage(context.Background(), "python:3.12-alpine", "512m", "0.5", 256, "print(1)")
	if err == nil || !strings.Contains(err.Error(), "docker create failed") {
		t.Fatalf("expected docker create failed error, got %v", err)
	}

	wantCalls := [][]string{
		pythonExecDockerCreateArgsWithImage("container-custom-image", "python:3.12-alpine", "512m", "0.5", 256, "print(1)"),
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("unexpected docker call sequence:\nwant=%#v\ngot=%#v", wantCalls, gotCalls)
	}
}

func TestRunPythonExecInDockerReturnsNonZeroExitAsResult(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	originalContainerNameFn := pythonExecContainerNameFn
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
		pythonExecContainerNameFn = originalContainerNameFn
	})

	pythonExecContainerNameFn = func() (string, error) {
		return "container-1", nil
	}

	var gotCalls [][]string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		gotCalls = append(gotCalls, append([]string(nil), args...))
		switch len(gotCalls) {
		case 1:
			return dockerCommandResult{ExitCode: 0}
		case 2:
			return dockerCommandResult{
				Stdout:   "",
				Stderr:   "Traceback (most recent call last)\n",
				ExitCode: 1,
			}
		case 3:
			return dockerCommandResult{Stdout: "exited|1", ExitCode: 0}
		case 4:
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected extra docker call: %#v", args)
			return dockerCommandResult{}
		}
	}

	result, err := runPythonExecInDockerWithImage(context.Background(), defaultPythonExecDockerImage, defaultPythonExecMemoryLimit, defaultPythonExecCPULimit, defaultPythonExecPidsLimit, "raise Exception('boom')")
	if err != nil {
		t.Fatalf("expected non-zero exit to be returned as result, got error: %v", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("expected exit_code=1, got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "Traceback") {
		t.Fatalf("unexpected stderr: %q", result.Stderr)
	}

	wantCalls := [][]string{
		pythonExecDockerCreateArgs("container-1", "raise Exception('boom')"),
		pythonExecDockerStartArgs("container-1"),
		pythonExecDockerInspectArgs("container-1"),
		pythonExecDockerRemoveArgs("container-1"),
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("unexpected docker call sequence:\nwant=%#v\ngot=%#v", wantCalls, gotCalls)
	}
}

func TestRunPythonExecInDockerTimeoutTriggersForceRemove(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	originalContainerNameFn := pythonExecContainerNameFn
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
		pythonExecContainerNameFn = originalContainerNameFn
	})

	pythonExecContainerNameFn = func() (string, error) {
		return "container-timeout", nil
	}

	var gotCalls [][]string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		gotCalls = append(gotCalls, append([]string(nil), args...))
		switch len(gotCalls) {
		case 1:
			return dockerCommandResult{ExitCode: 0}
		case 2:
			return dockerCommandResult{Err: context.DeadlineExceeded}
		case 3:
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected extra docker call: %#v", args)
			return dockerCommandResult{}
		}
	}

	_, err := runPythonExecInDockerWithImage(context.Background(), defaultPythonExecDockerImage, defaultPythonExecMemoryLimit, defaultPythonExecCPULimit, defaultPythonExecPidsLimit, "import time;time.sleep(10)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}

	wantCalls := [][]string{
		pythonExecDockerCreateArgs("container-timeout", "import time;time.sleep(10)"),
		pythonExecDockerStartArgs("container-timeout"),
		pythonExecDockerRemoveArgs("container-timeout"),
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("unexpected docker call sequence:\nwant=%#v\ngot=%#v", wantCalls, gotCalls)
	}
}

func TestRunPythonExecInDockerCreateFailureReturnsErrorWithoutCleanup(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	originalContainerNameFn := pythonExecContainerNameFn
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
		pythonExecContainerNameFn = originalContainerNameFn
	})

	pythonExecContainerNameFn = func() (string, error) {
		return "container-create-fail", nil
	}

	var gotCalls [][]string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		gotCalls = append(gotCalls, append([]string(nil), args...))
		return dockerCommandResult{
			Stderr:   "daemon error",
			ExitCode: 125,
		}
	}

	_, err := runPythonExecInDockerWithImage(context.Background(), defaultPythonExecDockerImage, defaultPythonExecMemoryLimit, defaultPythonExecCPULimit, defaultPythonExecPidsLimit, "print(1)")
	if err == nil || !strings.Contains(err.Error(), "docker create failed") {
		t.Fatalf("expected docker create failed error, got %v", err)
	}

	wantCalls := [][]string{
		pythonExecDockerCreateArgs("container-create-fail", "print(1)"),
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("unexpected docker call sequence:\nwant=%#v\ngot=%#v", wantCalls, gotCalls)
	}
}

func TestRunPythonExecInDockerStartFailureReturnsExecutionError(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	originalContainerNameFn := pythonExecContainerNameFn
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
		pythonExecContainerNameFn = originalContainerNameFn
	})

	pythonExecContainerNameFn = func() (string, error) {
		return "container-start-fail", nil
	}

	var gotCalls [][]string
	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		gotCalls = append(gotCalls, append([]string(nil), args...))
		switch len(gotCalls) {
		case 1:
			return dockerCommandResult{ExitCode: 0}
		case 2:
			return dockerCommandResult{
				Stderr:   "OCI runtime create failed",
				ExitCode: 1,
			}
		case 3:
			return dockerCommandResult{Stdout: "created|0", ExitCode: 0}
		case 4:
			return dockerCommandResult{ExitCode: 0}
		default:
			t.Fatalf("unexpected extra docker call: %#v", args)
			return dockerCommandResult{}
		}
	}

	_, err := runPythonExecInDockerWithImage(context.Background(), defaultPythonExecDockerImage, defaultPythonExecMemoryLimit, defaultPythonExecCPULimit, defaultPythonExecPidsLimit, "print(1)")
	if err == nil || !strings.Contains(err.Error(), "docker start failed") {
		t.Fatalf("expected docker start failed error, got %v", err)
	}

	wantCalls := [][]string{
		pythonExecDockerCreateArgs("container-start-fail", "print(1)"),
		pythonExecDockerStartArgs("container-start-fail"),
		pythonExecDockerInspectArgs("container-start-fail"),
		pythonExecDockerRemoveArgs("container-start-fail"),
	}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Fatalf("unexpected docker call sequence:\nwant=%#v\ngot=%#v", wantCalls, gotCalls)
	}
}

func TestRunPythonExecInDockerCleanupFailureDoesNotOverrideDeadline(t *testing.T) {
	originalRunDockerCommand := runDockerCommand
	originalContainerNameFn := pythonExecContainerNameFn
	t.Cleanup(func() {
		runDockerCommand = originalRunDockerCommand
		pythonExecContainerNameFn = originalContainerNameFn
	})

	pythonExecContainerNameFn = func() (string, error) {
		return "container-cleanup-fail", nil
	}

	runDockerCommand = func(_ context.Context, args ...string) dockerCommandResult {
		switch {
		case reflect.DeepEqual(args, pythonExecDockerCreateArgs("container-cleanup-fail", "import time;time.sleep(10)")):
			return dockerCommandResult{ExitCode: 0}
		case reflect.DeepEqual(args, pythonExecDockerStartArgs("container-cleanup-fail")):
			return dockerCommandResult{Err: context.DeadlineExceeded}
		case reflect.DeepEqual(args, pythonExecDockerRemoveArgs("container-cleanup-fail")):
			return dockerCommandResult{Stderr: "permission denied", ExitCode: 1}
		default:
			t.Fatalf("unexpected docker call: %#v", args)
			return dockerCommandResult{}
		}
	}

	_, err := runPythonExecInDockerWithImage(context.Background(), defaultPythonExecDockerImage, defaultPythonExecMemoryLimit, defaultPythonExecCPULimit, defaultPythonExecPidsLimit, "import time;time.sleep(10)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestRunSessionRespondsToEchoCommandDispatch(t *testing.T) {
	server := grpc.NewServer()
	fakeSvc := &fakeRegistryService{
		secretByNodeID: map[string]string{"worker-1": "secret-1"},
		dispatchEcho:   true,
	}
	registryv1.RegisterWorkerRegistryServiceServer(server, fakeSvc)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer listener.Close()
	defer server.Stop()
	go func() {
		_ = server.Serve(listener)
	}()

	cfg := testConfig()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	cfg.CallTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = runSession(ctx, cfg)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if got := atomic.LoadInt32(&fakeSvc.echoResultCount); got == 0 {
		t.Fatalf("expected at least one echo command result from worker")
	}
}

func TestRunSessionRespondsToPythonExecCommandDispatch(t *testing.T) {
	originalRunPythonExec := runPythonExec
	t.Cleanup(func() {
		runPythonExec = originalRunPythonExec
	})
	runPythonExec = func(_ context.Context, code string) (pythonExecRunResult, error) {
		if code != "print(\"123\")\nprint(\"234\")" {
			t.Fatalf("unexpected python code: %s", code)
		}
		return pythonExecRunResult{
			Output:   "123\n234\n",
			Stderr:   "",
			ExitCode: 0,
		}, nil
	}

	server := grpc.NewServer()
	fakeSvc := &fakeRegistryService{
		secretByNodeID: map[string]string{"worker-1": "secret-1"},
		dispatchPython: true,
	}
	registryv1.RegisterWorkerRegistryServiceServer(server, fakeSvc)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer listener.Close()
	defer server.Stop()
	go func() {
		_ = server.Serve(listener)
	}()

	cfg := testConfig()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	cfg.CallTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = runSession(ctx, cfg)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if got := atomic.LoadInt32(&fakeSvc.pythonResultCount); got == 0 {
		t.Fatalf("expected at least one pythonExec command result from worker")
	}
}

func testConfig() config.Config {
	return config.Config{
		ConsoleGRPCTarget:           "127.0.0.1:65535",
		WorkerID:                    "worker-1",
		WorkerSecret:                "secret-1",
		HeartbeatInterval:           100 * time.Millisecond,
		HeartbeatJitter:             0,
		CallTimeout:                 50 * time.Millisecond,
		NodeName:                    "node-test",
		ExecutorKind:                "docker",
		EchoMaxInflight:             defaultMaxInflight,
		PythonExecMaxInflight:       defaultMaxInflight,
		TerminalExecMaxInflight:     defaultMaxInflight,
		TerminalResourceMaxInflight: defaultMaxInflight,
	}
}

type fakeRegistryService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer

	secretByNodeID          map[string]string
	heartbeatCount          int32
	helloMaxActiveSessions  int32
	helloActiveSessionCount int32
	echoResultCount         int32
	pythonResultCount       int32
	dispatchEcho            bool
	dispatchPython          bool
}

func (s *fakeRegistryService) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := req.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first frame must be hello")
	}

	secret, ok := s.secretByNodeID[hello.GetNodeId()]
	if !ok {
		return status.Error(codes.Unauthenticated, "unknown worker")
	}
	if strings.TrimSpace(hello.GetWorkerSecret()) == "" {
		return status.Error(codes.Unauthenticated, "worker_secret is required")
	}
	if hello.GetWorkerSecret() != secret {
		return status.Error(codes.Unauthenticated, "invalid worker credential")
	}
	capabilityByName := make(map[string]int32, len(hello.GetCapabilities()))
	for _, capability := range hello.GetCapabilities() {
		capabilityByName[capability.GetName()] = capability.GetMaxInflight()
	}
	if capabilityByName[echoCapabilityName] <= 0 {
		return status.Error(codes.InvalidArgument, "missing echo capability")
	}
	if capabilityByName[pythonExecCapabilityDeclared] <= 0 {
		return status.Error(codes.InvalidArgument, "missing pythonExec capability")
	}
	if capabilityByName[terminalExecCapabilityDeclared] <= 0 {
		return status.Error(codes.InvalidArgument, "missing terminalExec capability")
	}
	if capabilityByName[terminalResourceCapabilityDeclared] <= 0 {
		return status.Error(codes.InvalidArgument, "missing terminalResource capability")
	}
	capacity := hello.GetTerminalSessionCapacity()
	if capacity == nil {
		return status.Error(codes.InvalidArgument, "missing terminal session capacity")
	}
	atomic.StoreInt32(&s.helloMaxActiveSessions, capacity.GetMaxActiveSessions())
	atomic.StoreInt32(&s.helloActiveSessionCount, capacity.GetActiveSessionCount())

	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{
				SessionId:            "session-1",
				HeartbeatIntervalSec: 1,
			},
		},
	}); err != nil {
		return err
	}
	recoveryReq, err := stream.Recv()
	if err != nil {
		return err
	}
	if recoveryReq.GetTerminalSessionRecoveryReport() == nil {
		return status.Error(codes.InvalidArgument, "terminal session recovery report is required")
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_TerminalSessionRecoveryAck{
			TerminalSessionRecoveryAck: &registryv1.TerminalSessionRecoveryAck{},
		},
	}); err != nil {
		return err
	}

	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		heartbeat := req.GetHeartbeat()
		commandResult := req.GetCommandResult()
		if commandResult != nil {
			var echoResult struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(commandResult.GetPayloadJson(), &echoResult); err == nil {
				if echoResult.Message == "echo-from-console" {
					atomic.AddInt32(&s.echoResultCount, 1)
					return status.Error(codes.FailedPrecondition, "session replaced after command roundtrip")
				}
			}
			if s.dispatchPython {
				decoded := pythonExecResult{}
				if err := json.Unmarshal(commandResult.GetPayloadJson(), &decoded); err != nil {
					return status.Error(codes.InvalidArgument, "invalid pythonExec result payload")
				}
				if decoded.Output == "123\n234\n" && decoded.ExitCode == 0 {
					atomic.AddInt32(&s.pythonResultCount, 1)
					return status.Error(codes.FailedPrecondition, "session replaced after python command roundtrip")
				}
			}
			return status.Error(codes.InvalidArgument, "unexpected command_result payload")
		}
		if heartbeat == nil {
			return status.Error(codes.InvalidArgument, "heartbeat frame is required")
		}
		if heartbeat.GetNodeId() == "" || heartbeat.GetSessionId() == "" {
			return status.Error(codes.InvalidArgument, "invalid heartbeat frame")
		}
		if heartbeat.GetActiveSessionCount() < 0 {
			return status.Error(codes.InvalidArgument, "invalid active_session_count")
		}

		count := atomic.AddInt32(&s.heartbeatCount, 1)
		if s.dispatchEcho && count == 1 {
			if err := stream.Send(&registryv1.ConnectResponse{
				Payload: &registryv1.ConnectResponse_CommandDispatch{
					CommandDispatch: &registryv1.CommandDispatch{
						CommandId:   "cmd-echo-1",
						Capability:  "echo",
						PayloadJson: []byte(`{"message":"echo-from-console"}`),
					},
				},
			}); err != nil {
				return err
			}
		}
		if s.dispatchPython && count == 1 {
			if err := stream.Send(&registryv1.ConnectResponse{
				Payload: &registryv1.ConnectResponse_CommandDispatch{
					CommandDispatch: &registryv1.CommandDispatch{
						CommandId:   "cmd-python-1",
						Capability:  "pythonExec",
						PayloadJson: []byte(`{"code":"print(\"123\")\nprint(\"234\")"}`),
					},
				},
			}); err != nil {
				return err
			}
		}
		if count >= 2 && !s.dispatchEcho && !s.dispatchPython {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("session outdated after %d heartbeats", count))
		}

		if err := stream.Send(&registryv1.ConnectResponse{
			Payload: &registryv1.ConnectResponse_HeartbeatAck{
				HeartbeatAck: &registryv1.HeartbeatAck{
					HeartbeatIntervalSec: 1,
				},
			},
		}); err != nil {
			return err
		}
	}
}
