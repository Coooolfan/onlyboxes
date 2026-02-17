package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/pkg/registryauth"
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

func TestBuildHelloSignsWithWorkerSecret(t *testing.T) {
	cfg := testConfig()
	hello, err := buildHello(cfg)
	if err != nil {
		t.Fatalf("buildHello failed: %v", err)
	}

	if hello.GetNodeId() != cfg.WorkerID {
		t.Fatalf("expected node_id=%s, got %s", cfg.WorkerID, hello.GetNodeId())
	}
	if hello.GetNonce() == "" {
		t.Fatalf("expected nonce to be set")
	}
	if !registryauth.Verify(
		hello.GetNodeId(),
		hello.GetTimestampUnixMs(),
		hello.GetNonce(),
		cfg.WorkerSecret,
		hello.GetSignature(),
	) {
		t.Fatalf("expected signature to verify")
	}
	if len(hello.GetCapabilities()) != 1 || hello.GetCapabilities()[0].GetName() != echoCapabilityName {
		t.Fatalf("expected hardcoded echo capability, got %#v", hello.GetCapabilities())
	}
	if hello.GetCapabilities()[0].GetMaxInflight() != defaultMaxInflight {
		t.Fatalf("expected max_inflight=%d, got %d", defaultMaxInflight, hello.GetCapabilities()[0].GetMaxInflight())
	}
}

func TestRunSessionReceivesFailedPreconditionFromServer(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = runSession(ctx, cfg)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if got := atomic.LoadInt32(&fakeSvc.heartbeatCount); got < 2 {
		t.Fatalf("expected at least two heartbeats before rejection, got %d", got)
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
	if result.GetEcho() == nil || result.GetEcho().GetMessage() != "hello" {
		t.Fatalf("expected echo payload, got %#v", result)
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

func testConfig() config.Config {
	return config.Config{
		ConsoleGRPCTarget: "127.0.0.1:65535",
		WorkerID:          "worker-1",
		WorkerSecret:      "secret-1",
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatJitter:   0,
		CallTimeout:       50 * time.Millisecond,
		NodeName:          "node-test",
		ExecutorKind:      "docker",
		Version:           "test",
	}
}

type fakeRegistryService struct {
	registryv1.UnimplementedWorkerRegistryServiceServer

	secretByNodeID  map[string]string
	heartbeatCount  int32
	echoResultCount int32
	dispatchEcho    bool
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
	if !registryauth.Verify(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), secret, hello.GetSignature()) {
		return status.Error(codes.Unauthenticated, "invalid signature")
	}
	if len(hello.GetCapabilities()) == 0 || hello.GetCapabilities()[0].GetName() != echoCapabilityName {
		return status.Error(codes.InvalidArgument, "missing echo capability")
	}
	if hello.GetCapabilities()[0].GetMaxInflight() <= 0 {
		return status.Error(codes.InvalidArgument, "missing max_inflight")
	}

	if err := stream.Send(&registryv1.ConnectResponse{
		Payload: &registryv1.ConnectResponse_ConnectAck{
			ConnectAck: &registryv1.ConnectAck{
				SessionId:            "session-1",
				ServerTimeUnixMs:     time.Now().UnixMilli(),
				HeartbeatIntervalSec: 1,
				OfflineTtlSec:        15,
			},
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
			if commandResult.GetEcho() != nil && commandResult.GetEcho().GetMessage() == "echo-from-console" {
				atomic.AddInt32(&s.echoResultCount, 1)
				return status.Error(codes.FailedPrecondition, "session replaced after command roundtrip")
			}
			return status.Error(codes.InvalidArgument, "unexpected command_result payload")
		}
		if heartbeat == nil {
			return status.Error(codes.InvalidArgument, "heartbeat frame is required")
		}
		if heartbeat.GetNodeId() == "" || heartbeat.GetSessionId() == "" {
			return status.Error(codes.InvalidArgument, "invalid heartbeat frame")
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
		if count >= 2 && !s.dispatchEcho {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("session outdated after %d heartbeats", count))
		}

		if err := stream.Send(&registryv1.ConnectResponse{
			Payload: &registryv1.ConnectResponse_HeartbeatAck{
				HeartbeatAck: &registryv1.HeartbeatAck{
					ServerTimeUnixMs:     time.Now().UnixMilli(),
					HeartbeatIntervalSec: 1,
					OfflineTtlSec:        15,
				},
			},
		}); err != nil {
			return err
		}
	}
}
