package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConsoleSessionContract(t *testing.T) {
	originalActiveSessionCount := activeSessionCountFn
	activeSessionCountFn = func() int32 { return 3 }
	t.Cleanup(func() { activeSessionCountFn = originalActiveSessionCount })

	service := &consoleContractService{}
	server := grpc.NewServer()
	registryv1.RegisterWorkerRegistryServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()

	cfg := consoleTestConfig()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.TerminalMaxActiveSessions = 9
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connected, err := runSessionWithStatus(ctx, cfg)
	if !connected {
		t.Fatal("expected the session handshake to complete")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if atomic.LoadInt32(&service.commandResults) != 1 {
		t.Fatalf("expected one command result")
	}
}

func TestBuildHelloAdvertisesExplicitUnlimitedTerminalCapacity(t *testing.T) {
	hello, err := buildHello(consoleTestConfig())
	if err != nil {
		t.Fatalf("buildHello failed: %v", err)
	}
	capacity := hello.GetTerminalSessionCapacity()
	if capacity == nil || capacity.GetMaxActiveSessions() != 0 {
		t.Fatalf("expected explicit unlimited terminal capacity, got %#v", capacity)
	}
}

func TestBuildHelloAdvertisesE2BDirectProxy(t *testing.T) {
	cfg := consoleTestConfig()
	cfg.ProxyEnabled = true
	hello, err := buildHello(cfg)
	if err != nil {
		t.Fatalf("buildHello failed: %v", err)
	}
	if hello.GetLabels()[proxytoken.ProxyDirectLabel] != proxytoken.ProxyDirectE2B {
		t.Fatalf("missing E2B direct proxy label: %#v", hello.GetLabels())
	}
	found := false
	for _, capability := range hello.GetCapabilities() {
		if capability.GetName() == terminalProxyCapabilityDeclared {
			found = true
		}
	}
	if !found {
		t.Fatal("missing terminalProxy capability")
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

func TestRunRejectsTerminalCapacityOutsideProtocolRange(t *testing.T) {
	cfg := consoleTestConfig()
	cfg.E2BAPIKey = "api-key"
	cfg.E2BPythonTemplate = "python-template"
	cfg.E2BTerminalTemplate = "terminal-template"
	cfg.TerminalMaxActiveSessions = int(maxProtocolInt32 + 1)

	err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "WORKER_TERMINAL_MAX_ACTIVE_SESSIONS") {
		t.Fatalf("expected terminal capacity validation error, got %v", err)
	}
}

func TestBuildHelloRejectsNegativeActiveSessionCount(t *testing.T) {
	originalActiveSessionCount := activeSessionCountFn
	activeSessionCountFn = func() int32 { return -1 }
	t.Cleanup(func() { activeSessionCountFn = originalActiveSessionCount })

	if _, err := buildHello(consoleTestConfig()); err == nil {
		t.Fatal("expected negative active session count to be rejected")
	}
}

func TestHeartbeatToleratesOneMissAndFailsAfterTwo(t *testing.T) {
	cfg := consoleTestConfig()
	cfg.CallTimeout = 20 * time.Millisecond

	outbound := make(chan *registryv1.ConnectRequest, 8)
	acks := make(chan *registryv1.HeartbeatAck, 1)
	sessionErrors := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- heartbeatLoop(ctx, outbound, acks, sessionErrors, cfg, "session-1", 5*time.Millisecond)
	}()
	<-outbound // First heartbeat times out.
	second := <-outbound
	if second.GetHeartbeat() == nil {
		t.Fatalf("expected second heartbeat")
	}
	acks <- &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected recovery after one missed ack, got %v", err)
	}

	outbound = make(chan *registryv1.ConnectRequest, 8)
	err := heartbeatLoop(context.Background(), outbound, make(chan *registryv1.HeartbeatAck), make(chan error), cfg, "session-2", 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected failure after two missed acks, got %v", err)
	}
}

func TestConsoleSessionDispatchesAllCapabilities(t *testing.T) {
	originalPython := runPythonExec
	originalTerminal := runTerminalExec
	originalResource := runTerminalResource
	t.Cleanup(func() {
		runPythonExec = originalPython
		runTerminalExec = originalTerminal
		runTerminalResource = originalResource
	})
	runPythonExec = func(context.Context, string) (pythonExecRunResult, error) {
		return pythonExecRunResult{Output: "python-ok", ExitCode: 0}, nil
	}
	runTerminalExec = func(context.Context, terminalExecRequest) (terminalExecRunResult, error) {
		return terminalExecRunResult{SessionID: "terminal-session", Stdout: "terminal-ok"}, nil
	}
	runTerminalResource = func(context.Context, terminalResourceRequest) (terminalResourceRunResult, error) {
		return terminalResourceRunResult{
			SessionID: "terminal-session",
			FilePath:  "/tmp/file.txt",
			MIMEType:  "text/plain",
			SizeBytes: 2,
			Blob:      []byte("ok"),
		}, nil
	}

	service := &allCapabilityContractService{}
	server := grpc.NewServer()
	registryv1.RegisterWorkerRegistryServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()
	cfg := consoleTestConfig()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runSession(ctx, cfg); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition after all results, got %v", err)
	}
	if got := atomic.LoadInt32(&service.results); got != 4 {
		t.Fatalf("expected all four capability results, got %d", got)
	}
}

func consoleTestConfig() config.Config {
	return config.Config{
		ConsoleGRPCTarget:           "127.0.0.1:1",
		ConsoleTLS:                  false,
		WorkerID:                    "worker-e2b-1",
		WorkerSecret:                "worker-secret",
		HeartbeatInterval:           10 * time.Millisecond,
		HeartbeatJitter:             0,
		CallTimeout:                 time.Second,
		NodeName:                    "e2b-test",
		ExecutorKind:                "e2b",
		EchoMaxInflight:             4,
		PythonExecMaxInflight:       4,
		TerminalExecMaxInflight:     4,
		TerminalResourceMaxInflight: 4,
	}
}

type consoleContractService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer
	commandResults int32
}

type allCapabilityContractService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer
	results int32
}

func (s *allCapabilityContractService) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetHello() == nil {
		return status.Error(codes.InvalidArgument, "hello required")
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{SessionId: "all-capabilities", HeartbeatIntervalSec: 1},
		},
	}); err != nil {
		return err
	}
	firstHeartbeat, err := stream.Recv()
	if err != nil || firstHeartbeat.GetHeartbeat() == nil {
		return status.Error(codes.InvalidArgument, "heartbeat required")
	}
	dispatches := []*registryv1.CommandDispatch{
		{CommandId: "echo", Capability: "echo", PayloadJson: []byte(`{"message":"echo-ok"}`)},
		{CommandId: "python", Capability: "pythonExec", PayloadJson: []byte(`{"code":"print(1)"}`)},
		{CommandId: "terminal", Capability: "terminalExec", PayloadJson: []byte(`{"command":"pwd"}`)},
		{CommandId: "resource", Capability: "terminalResource", PayloadJson: []byte(`{"session_id":"terminal-session","file_path":"/tmp/file.txt","action":"read"}`)},
	}
	for _, dispatch := range dispatches {
		if err := stream.Send(&registryv1.ConnectResponse{
			Payload: &registryv1.ConnectResponse_CommandDispatch{CommandDispatch: dispatch},
		}); err != nil {
			return err
		}
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_HeartbeatAck{
			HeartbeatAck: &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1},
		},
	}); err != nil {
		return err
	}
	seen := map[string]bool{}
	for len(seen) < len(dispatches) {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		result := frame.GetCommandResult()
		if result == nil {
			continue
		}
		if result.GetError() != nil || len(result.GetPayloadJson()) == 0 {
			return status.Errorf(codes.InvalidArgument, "capability %s failed", result.GetCommandId())
		}
		seen[result.GetCommandId()] = true
	}
	atomic.StoreInt32(&s.results, int32(len(seen)))
	return status.Error(codes.FailedPrecondition, "contract complete")
}

func (s *consoleContractService) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetNodeId() != "worker-e2b-1" || hello.GetWorkerSecret() != "worker-secret" {
		return status.Error(codes.Unauthenticated, "invalid hello identity")
	}
	if hello.GetExecutorKind() != "e2b" || !strings.HasPrefix(hello.GetNodeName(), "e2b-") {
		return status.Error(codes.InvalidArgument, "invalid hello metadata")
	}
	capabilities := map[string]int32{}
	for _, declaration := range hello.GetCapabilities() {
		capabilities[declaration.GetName()] = declaration.GetMaxInflight()
	}
	for _, name := range []string{"echo", "pythonExec", "terminalExec", "terminalResource"} {
		if capabilities[name] != 4 {
			return status.Errorf(codes.InvalidArgument, "invalid capability %s", name)
		}
	}
	capacity := hello.GetTerminalSessionCapacity()
	if capacity == nil || capacity.GetMaxActiveSessions() != 9 || capacity.GetActiveSessionCount() != 3 {
		return status.Errorf(codes.InvalidArgument, "invalid terminal session capacity: %#v", capacity)
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{
				SessionId:            "console-session",
				HeartbeatIntervalSec: 1,
			},
		},
	}); err != nil {
		return err
	}

	heartbeatFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	heartbeat := heartbeatFrame.GetHeartbeat()
	if heartbeat == nil ||
		heartbeat.GetNodeId() != hello.GetNodeId() ||
		heartbeat.GetSessionId() != "console-session" ||
		heartbeat.GetActiveSessionCount() != 3 {
		return status.Error(codes.InvalidArgument, "invalid heartbeat")
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_CommandDispatch{
			CommandDispatch: &registryv1.CommandDispatch{
				CommandId:   "echo-command",
				Capability:  "echo",
				PayloadJson: []byte(`{"message":"from-console"}`),
			},
		},
	}); err != nil {
		return err
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_HeartbeatAck{
			HeartbeatAck: &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1},
		},
	}); err != nil {
		return err
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if result := frame.GetCommandResult(); result != nil {
			var payload struct {
				Message string `json:"message"`
			}
			if result.GetCommandId() != "echo-command" ||
				json.Unmarshal(result.GetPayloadJson(), &payload) != nil ||
				payload.Message != "from-console" {
				return status.Error(codes.InvalidArgument, "invalid command result")
			}
			atomic.AddInt32(&s.commandResults, 1)
			return status.Error(codes.FailedPrecondition, "session replaced")
		}
	}
}
