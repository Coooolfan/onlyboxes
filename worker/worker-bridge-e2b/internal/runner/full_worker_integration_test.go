package runner

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIntegrationConsoleDispatchesAllCapabilitiesToE2B(t *testing.T) {
	if os.Getenv("E2B_INTEGRATION") != "1" {
		t.Skip("set E2B_INTEGRATION=1 to run against E2B")
	}
	apiKey := strings.TrimSpace(os.Getenv("E2B_API_KEY"))
	pythonTemplate := strings.TrimSpace(os.Getenv("E2B_PYTHON_TEMPLATE"))
	terminalTemplate := strings.TrimSpace(os.Getenv("E2B_TERMINAL_TEMPLATE"))
	if apiKey == "" || pythonTemplate == "" || terminalTemplate == "" {
		t.Fatal("E2B_API_KEY, E2B_PYTHON_TEMPLATE and E2B_TERMINAL_TEMPLATE are required")
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
	backend := &recordingE2BBackend{e2bBackend: client}
	manager := newLiveTerminalManager(backend, terminalTemplate, 2)
	defer manager.Close()
	python := newPythonExecRunner(backend, pythonTemplate, 120)

	originalPython := runPythonExec
	originalTerminal := runTerminalExec
	originalResource := runTerminalResource
	originalActiveSessions := activeSessionCountFn
	runPythonExec = python.Execute
	runTerminalExec = manager.Execute
	runTerminalResource = manager.ResolveResource
	activeSessionCountFn = manager.ActiveSessionCount
	t.Cleanup(func() {
		runPythonExec = originalPython
		runTerminalExec = originalTerminal
		runTerminalResource = originalResource
		activeSessionCountFn = originalActiveSessions
	})

	service := &liveAllCapabilityService{}
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
	cfg.CallTimeout = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runSession(ctx, cfg); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("full worker session failed: %v", err)
	}
	if atomic.LoadInt32(&service.verifiedResults) != 4 {
		t.Fatalf("expected four verified capability results, got %d", service.verifiedResults)
	}
	manager.Close()
	if backend.killCount() != 2 {
		t.Fatalf("expected Python and terminal sandboxes to be cleaned up, kill_count=%d", backend.killCount())
	}
}

type liveAllCapabilityService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer
	verifiedResults int32
}

func (s *liveAllCapabilityService) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetHello() == nil {
		return status.Error(codes.InvalidArgument, "hello required")
	}
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{SessionId: "live-all-tools", HeartbeatIntervalSec: 1},
		},
	}); err != nil {
		return err
	}
	if err := s.waitForHeartbeat(stream); err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second).UnixMilli()
	for _, dispatch := range []*registryv1.CommandDispatch{
		{
			CommandId:      "live-echo",
			Capability:     "echo",
			PayloadJson:    []byte(`{"message":"echo-dispatch-live"}`),
			DeadlineUnixMs: deadline,
		},
		{
			CommandId:      "live-python",
			Capability:     "pythonExec",
			PayloadJson:    []byte(`{"code":"print(\"python-dispatch-live\")"}`),
			DeadlineUnixMs: deadline,
		},
		{
			CommandId:      "live-terminal",
			Capability:     "terminalExec",
			PayloadJson:    []byte(`{"command":"printf 'resource-dispatch-live' > /tmp/onlyboxes-dispatch-live.txt; printf 'terminal-dispatch-live'"}`),
			DeadlineUnixMs: deadline,
		},
	} {
		if err := stream.Send(&registryv1.ConnectResponse{
			Payload: &registryv1.ConnectResponse_CommandDispatch{CommandDispatch: dispatch},
		}); err != nil {
			return err
		}
	}

	seen := map[string]bool{}
	terminalSessionID := ""
	for len(seen) < 3 {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if heartbeat := frame.GetHeartbeat(); heartbeat != nil {
			if err := s.sendHeartbeatAck(stream); err != nil {
				return err
			}
			continue
		}
		result := frame.GetCommandResult()
		if result == nil || result.GetError() != nil {
			return status.Errorf(codes.InvalidArgument, "tool failed: %#v", result)
		}
		switch result.GetCommandId() {
		case "live-echo":
			var decoded struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(result.GetPayloadJson(), &decoded) != nil || decoded.Message != "echo-dispatch-live" {
				return status.Error(codes.InvalidArgument, "invalid echo result")
			}
		case "live-python":
			var decoded pythonExecResult
			if json.Unmarshal(result.GetPayloadJson(), &decoded) != nil ||
				strings.TrimSpace(decoded.Output) != "python-dispatch-live" ||
				decoded.ExitCode != 0 {
				return status.Error(codes.InvalidArgument, "invalid Python result")
			}
		case "live-terminal":
			var decoded terminalExecRunResult
			if json.Unmarshal(result.GetPayloadJson(), &decoded) != nil ||
				decoded.Stdout != "terminal-dispatch-live" ||
				decoded.SessionID == "" {
				return status.Error(codes.InvalidArgument, "invalid terminal result")
			}
			terminalSessionID = decoded.SessionID
		default:
			return status.Error(codes.InvalidArgument, "unknown command result")
		}
		seen[result.GetCommandId()] = true
	}
	if terminalSessionID == "" {
		return status.Error(codes.InvalidArgument, "terminal session missing")
	}
	resourcePayload, _ := json.Marshal(terminalResourcePayload{
		SessionID: terminalSessionID,
		FilePath:  "/tmp/onlyboxes-dispatch-live.txt",
		Action:    terminalResourceActionRead,
	})
	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_CommandDispatch{
			CommandDispatch: &registryv1.CommandDispatch{
				CommandId:      "live-resource",
				Capability:     "terminalResource",
				PayloadJson:    resourcePayload,
				DeadlineUnixMs: deadline,
			},
		},
	}); err != nil {
		return err
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetHeartbeat() != nil {
			if err := s.sendHeartbeatAck(stream); err != nil {
				return err
			}
			continue
		}
		result := frame.GetCommandResult()
		if result == nil {
			continue
		}
		if result.GetCommandId() != "live-resource" || result.GetError() != nil {
			return status.Errorf(codes.InvalidArgument, "resource failed: %#v", result)
		}
		var decoded terminalResourceRunResult
		if json.Unmarshal(result.GetPayloadJson(), &decoded) != nil ||
			string(decoded.Blob) != "resource-dispatch-live" {
			return status.Error(codes.InvalidArgument, "invalid resource result")
		}
		atomic.StoreInt32(&s.verifiedResults, 4)
		return status.Error(codes.FailedPrecondition, "live capability contract complete")
	}
}

func (s *liveAllCapabilityService) waitForHeartbeat(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetHeartbeat() != nil {
			return s.sendHeartbeatAck(stream)
		}
	}
}

func (s *liveAllCapabilityService) sendHeartbeatAck(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	return stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_HeartbeatAck{
			HeartbeatAck: &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1},
		},
	})
}
