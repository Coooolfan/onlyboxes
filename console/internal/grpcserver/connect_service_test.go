package grpcserver

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/pkg/registryauth"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func newBufClient(t *testing.T, svc registryv1.WorkerRegistryServiceServer) (registryv1.WorkerRegistryServiceClient, func()) {
	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	registryv1.RegisterWorkerRegistryServiceServer(server, svc)
	go func() {
		_ = server.Serve(listener)
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}

	conn, err := grpc.NewClient(
		"bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	}
	return registryv1.NewWorkerRegistryServiceClient(conn), cleanup
}

func TestConnectRejectsFirstFrameWithoutHello(t *testing.T) {
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}

	err = stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       "node-1",
				SessionId:    "session-x",
				SentAtUnixMs: time.Now().UnixMilli(),
			},
		},
	})
	if err != nil {
		t.Fatalf("send heartbeat failed: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestConnectRejectsUnknownWorkerID(t *testing.T) {
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	now := time.Now().UnixMilli()
	hello := &registryv1.ConnectHello{
		NodeId:          "unknown-node",
		TimestampUnixMs: now,
		Nonce:           "nonce-1",
	}
	hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), "secret-unknown")

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		t.Fatalf("send hello failed: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestConnectRejectsInvalidSignature(t *testing.T) {
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	hello := &registryv1.ConnectHello{
		NodeId:          "node-1",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "nonce-1",
		Signature:       strings.Repeat("a", 64),
	}

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		t.Fatalf("send hello failed: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestConnectRejectsTimestampOutsideReplayWindow(t *testing.T) {
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	svc.nowFn = func() time.Time {
		return time.UnixMilli(1_700_000_000_000)
	}
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	hello := &registryv1.ConnectHello{
		NodeId:          "node-1",
		TimestampUnixMs: svc.nowFn().Add(-120 * time.Second).UnixMilli(),
		Nonce:           "nonce-1",
	}
	hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), "secret-1")

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		t.Fatalf("send hello failed: %v", err)
	}

	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestConnectRejectsNonceReplay(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	svc.nowFn = func() time.Time {
		return now
	}
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	connectOnce := func() error {
		hello := &registryv1.ConnectHello{
			NodeId:          "node-1",
			TimestampUnixMs: now.UnixMilli(),
			Nonce:           "nonce-replay",
		}
		hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), "secret-1")

		stream, err := client.Connect(context.Background())
		if err != nil {
			return err
		}
		if err := stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
		}); err != nil {
			return err
		}
		_, err = stream.Recv()
		return err
	}

	if err := connectOnce(); err != nil {
		t.Fatalf("first connect should pass, got %v", err)
	}
	if err := connectOnce(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated on replay, got %v", err)
	}
}

func TestConnectAndHeartbeatSuccess(t *testing.T) {
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	svc.newSessionIDFn = func() (string, error) {
		return "session-1", nil
	}
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream := mustConnectWithHello(t, client, "node-1", "secret-1", "nonce-1")

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv connect ack failed: %v", err)
	}
	if resp.GetConnectAck() == nil || resp.GetConnectAck().GetSessionId() != "session-1" {
		t.Fatalf("expected connect_ack session-1, got %#v", resp.GetPayload())
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       "node-1",
				SessionId:    "session-1",
				SentAtUnixMs: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send heartbeat failed: %v", err)
	}

	heartbeatResp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv heartbeat ack failed: %v", err)
	}
	if heartbeatResp.GetHeartbeatAck() == nil {
		t.Fatalf("expected heartbeat_ack, got %#v", heartbeatResp.GetPayload())
	}
}

func TestConnectReplacesOldSession(t *testing.T) {
	svc := NewRegistryService(registry.NewStore(), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	sessionIDs := []string{"session-a", "session-b"}
	svc.newSessionIDFn = func() (string, error) {
		if len(sessionIDs) == 0 {
			return "", errors.New("no sessions")
		}
		session := sessionIDs[0]
		sessionIDs = sessionIDs[1:]
		return session, nil
	}

	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	streamA := mustConnectWithHello(t, client, "node-1", "secret-1", "nonce-a")
	respA, err := streamA.Recv()
	if err != nil {
		t.Fatalf("recv first connect ack failed: %v", err)
	}
	if respA.GetConnectAck() == nil || respA.GetConnectAck().GetSessionId() != "session-a" {
		t.Fatalf("expected session-a, got %#v", respA.GetPayload())
	}

	streamB := mustConnectWithHello(t, client, "node-1", "secret-1", "nonce-b")
	respB, err := streamB.Recv()
	if err != nil {
		t.Fatalf("recv second connect ack failed: %v", err)
	}
	if respB.GetConnectAck() == nil || respB.GetConnectAck().GetSessionId() != "session-b" {
		t.Fatalf("expected session-b, got %#v", respB.GetPayload())
	}

	if err := streamA.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       "node-1",
				SessionId:    "session-a",
				SentAtUnixMs: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send old-session heartbeat failed: %v", err)
	}

	_, err = streamA.Recv()
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for old session, got %v", err)
	}

	if err := streamB.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       "node-1",
				SessionId:    "session-b",
				SentAtUnixMs: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send latest-session heartbeat failed: %v", err)
	}

	respHeartbeat, err := streamB.Recv()
	if err != nil {
		t.Fatalf("recv latest-session heartbeat ack failed: %v", err)
	}
	if respHeartbeat.GetHeartbeatAck() == nil {
		t.Fatalf("expected heartbeat_ack, got %#v", respHeartbeat.GetPayload())
	}
}

func mustConnectWithHello(t *testing.T, client registryv1.WorkerRegistryServiceClient, workerID string, secret string, nonce string) grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse] {
	t.Helper()

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}

	hello := &registryv1.ConnectHello{
		NodeId:          workerID,
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           nonce,
	}
	hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), secret)
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		t.Fatalf("send hello failed: %v", err)
	}
	return stream
}
