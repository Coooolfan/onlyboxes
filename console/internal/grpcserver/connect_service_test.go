package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/testutil/registrytest"
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
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
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
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	hello := &registryv1.ConnectHello{
		NodeId:       "unknown-node",
		WorkerSecret: "secret-unknown",
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

func TestCreateProvisionedWorkerAllowsDynamicConnect(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := NewRegistryService(store, map[string]string{}, 5, 15, 60*time.Second)

	workerID, workerSecret, err := svc.CreateProvisionedWorker(time.Now(), 15*time.Second)
	if err != nil {
		t.Fatalf("create provisioned worker failed: %v", err)
	}
	if strings.TrimSpace(workerID) == "" || strings.TrimSpace(workerSecret) == "" {
		t.Fatalf("expected non-empty worker_id and worker_secret")
	}
	if _, ok := svc.GetWorkerSecret(workerID); !ok {
		t.Fatalf("expected secret lookup for worker %s", workerID)
	}

	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, workerID, workerSecret, "nonce-dynamic", []string{"echo"})
	if err != nil {
		t.Fatalf("dynamic worker connect failed: %v", err)
	}
	_ = stream.CloseSend()
}

func TestDeleteProvisionedWorkerDisconnectsSessionAndRevokesCredential(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := NewRegistryService(store, map[string]string{}, 5, 15, 60*time.Second)

	workerID, workerSecret, err := svc.CreateProvisionedWorker(time.Now(), 15*time.Second)
	if err != nil {
		t.Fatalf("create provisioned worker failed: %v", err)
	}

	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, sessionID, err := connectWorker(client, workerID, workerSecret, "nonce-delete-1", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	if removed := svc.DeleteProvisionedWorker(workerID); !removed {
		t.Fatalf("expected delete to return true")
	}
	if _, ok := svc.GetWorkerSecret(workerID); ok {
		t.Fatalf("expected credential to be revoked")
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       workerID,
				SessionId:    sessionID,
				SentAtUnixMs: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send heartbeat after delete failed: %v", err)
	}

	_, err = stream.Recv()
	if code := status.Code(err); code != codes.PermissionDenied && code != codes.NotFound {
		t.Fatalf("expected PermissionDenied or NotFound after delete, got %v", err)
	}

	_, _, err = connectWorker(client, workerID, workerSecret, "nonce-delete-2", []string{"echo"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated on reconnect after delete, got %v", err)
	}
}

func TestConnectRejectsInvalidWorkerSecret(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	hello := &registryv1.ConnectHello{
		NodeId:       "node-1",
		WorkerSecret: "secret-invalid",
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

func TestConnectAcceptsHelloWithoutLegacyAuthFields(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, sessionID, err := connectWorker(client, "node-1", "secret-1", "nonce-legacy", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	if strings.TrimSpace(sessionID) == "" {
		t.Fatalf("expected non-empty session_id")
	}
	_ = stream.CloseSend()
}

func TestConnectAndHeartbeatSuccess(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	svc.newSessionIDFn = func() (string, error) {
		return "session-1", nil
	}
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, sessionID, err := connectWorker(client, "node-1", "secret-1", "nonce-1", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	if sessionID != "session-1" {
		t.Fatalf("expected session-1, got %s", sessionID)
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       "node-1",
				SessionId:    sessionID,
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

func TestConnectReturnsInternalWhenStoreUpsertFails(t *testing.T) {
	store := registrytest.NewStore(t)
	if _, err := store.Persistence().SQL.ExecContext(
		context.Background(),
		`CREATE TRIGGER fail_connect_upsert
BEFORE INSERT ON worker_nodes
BEGIN
  SELECT RAISE(FAIL, 'forced connect upsert failure');
END`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	svc := NewRegistryService(store, map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{
			Hello: &registryv1.ConnectHello{
				NodeId:       "node-1",
				WorkerSecret: "secret-1",
			},
		},
	}); err != nil {
		t.Fatalf("send hello failed: %v", err)
	}

	if _, err := stream.Recv(); status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal when upsert fails, got %v", err)
	}
}

func TestHandleHeartbeatReturnsDeadlineExceededWhenControlQueueFull(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_000, 0)
	svc.nowFn = func() time.Time {
		return now
	}

	hello := &registryv1.ConnectHello{
		NodeId:       "node-1",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: "echo"}},
	}
	if err := svc.store.Upsert(hello, "session-1", now); err != nil {
		t.Fatalf("seed store upsert failed: %v", err)
	}

	session := newActiveSession("node-1", "session-1", hello)
	for i := 0; i < controlOutboundBufferSize; i++ {
		if err := session.enqueueControl(context.Background(), newHeartbeatAck(now, 5, 15)); err != nil {
			t.Fatalf("failed to preload control queue: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := svc.handleHeartbeat(ctx, session, &registryv1.HeartbeatFrame{
		NodeId:       "node-1",
		SessionId:    "session-1",
		SentAtUnixMs: now.UnixMilli(),
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestConnectReplacesOldSession(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
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

	streamA, sessionA, err := connectWorker(client, "node-1", "secret-1", "nonce-a", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker A failed: %v", err)
	}
	if sessionA != "session-a" {
		t.Fatalf("expected session-a, got %s", sessionA)
	}

	streamB, sessionB, err := connectWorker(client, "node-1", "secret-1", "nonce-b", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker B failed: %v", err)
	}
	if sessionB != "session-b" {
		t.Fatalf("expected session-b, got %s", sessionB)
	}

	if err := streamA.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       "node-1",
				SessionId:    sessionA,
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
				SessionId:    sessionB,
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

func TestDispatchEchoSuccess(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-echo", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	go echoResponder(stream)

	got, err := svc.DispatchEcho(context.Background(), "hello", 2*time.Second)
	if err != nil {
		t.Fatalf("dispatch echo failed: %v", err)
	}
	if got != "hello" {
		t.Fatalf("expected hello, got %q", got)
	}
}

func TestDispatchEchoNoCapabilityWorker(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	if _, _, err := connectWorker(client, "node-1", "secret-1", "nonce-no-echo", []string{"build"}); err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	_, err := svc.DispatchEcho(context.Background(), "hello", 2*time.Second)
	if !errors.Is(err, ErrNoEchoWorker) {
		t.Fatalf("expected ErrNoEchoWorker, got %v", err)
	}
}

func TestDispatchEchoNoWorkerCapacity(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	if _, _, err := connectWorker(client, "node-1", "secret-1", "nonce-no-capacity", []string{"echo"}); err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	session := svc.getSession("node-1")
	if session == nil {
		t.Fatalf("expected active session for node-1")
	}

	for i := 0; i < defaultCapabilityMaxInflight; i++ {
		if !session.tryAcquireCapability("echo") {
			t.Fatalf("expected capability slot %d to be acquirable", i)
		}
	}
	defer func() {
		for i := 0; i < defaultCapabilityMaxInflight; i++ {
			session.releaseCapability("echo")
		}
	}()

	_, err := svc.DispatchEcho(context.Background(), "hello", 2*time.Second)
	if !errors.Is(err, ErrNoWorkerCapacity) {
		t.Fatalf("expected ErrNoWorkerCapacity, got %v", err)
	}
}

func TestDispatchEchoRoundRobin(t *testing.T) {
	svc := NewRegistryService(
		registrytest.NewStore(t),
		map[string]string{
			"node-a": "secret-a",
			"node-b": "secret-b",
		},
		5,
		15,
		60*time.Second,
	)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	streamA, _, err := connectWorker(client, "node-a", "secret-a", "nonce-a", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker A failed: %v", err)
	}
	streamB, _, err := connectWorker(client, "node-b", "secret-b", "nonce-b", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker B failed: %v", err)
	}

	var countA int32
	var countB int32
	go countingEchoResponder(streamA, &countA)
	go countingEchoResponder(streamB, &countB)

	if _, err := svc.DispatchEcho(context.Background(), "m1", 2*time.Second); err != nil {
		t.Fatalf("first dispatch failed: %v", err)
	}
	if _, err := svc.DispatchEcho(context.Background(), "m2", 2*time.Second); err != nil {
		t.Fatalf("second dispatch failed: %v", err)
	}

	if atomic.LoadInt32(&countA) != 1 || atomic.LoadInt32(&countB) != 1 {
		t.Fatalf("expected round-robin distribution 1/1, got node-a=%d node-b=%d", countA, countB)
	}
}

func TestDispatchEchoCommandError(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-err", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	go func() {
		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			dispatch := resp.GetCommandDispatch()
			if dispatch == nil {
				continue
			}
			_ = stream.Send(&registryv1.ConnectRequest{
				Payload: &registryv1.ConnectRequest_CommandResult{
					CommandResult: &registryv1.CommandResult{
						CommandId: dispatch.GetCommandId(),
						Error: &registryv1.CommandError{
							Code:    "echo_failed",
							Message: "mock failure",
						},
					},
				},
			})
			return
		}
	}()

	_, err = svc.DispatchEcho(context.Background(), "hello", 2*time.Second)
	var commandErr *CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("expected CommandExecutionError, got %v", err)
	}
	if commandErr.Code != "echo_failed" {
		t.Fatalf("expected echo_failed code, got %s", commandErr.Code)
	}
}

func TestDispatchEchoTimeout(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-timeout", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	go func() {
		for {
			_, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			// Intentionally ignore command dispatch, do not send command result.
		}
	}()

	_, err = svc.DispatchEcho(context.Background(), "hello", 200*time.Millisecond)
	if !errors.Is(err, ErrEchoTimeout) {
		t.Fatalf("expected ErrEchoTimeout, got %v", err)
	}
}

func TestDispatchEchoConcurrentCommandIDs(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-concurrency", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	go func() {
		commands := make([]*registryv1.CommandDispatch, 0, 2)
		for len(commands) < 2 {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			dispatch := resp.GetCommandDispatch()
			if dispatch == nil {
				continue
			}
			commands = append(commands, dispatch)
		}

		// Return results in reverse order to validate command_id correlation.
		for i := len(commands) - 1; i >= 0; i-- {
			dispatch := commands[i]
			_ = stream.Send(&registryv1.ConnectRequest{
				Payload: &registryv1.ConnectRequest_CommandResult{
					CommandResult: &registryv1.CommandResult{
						CommandId: dispatch.GetCommandId(),
						Echo: &registryv1.EchoResult{
							Message: dispatch.GetEcho().GetMessage(),
						},
					},
				},
			})
		}
	}()

	messages := []string{"first", "second"}
	results := make([]string, 0, len(messages))
	errCh := make(chan error, len(messages))
	resultMu := sync.Mutex{}
	var wg sync.WaitGroup
	for _, message := range messages {
		wg.Add(1)
		go func(message string) {
			defer wg.Done()
			result, callErr := svc.DispatchEcho(context.Background(), message, 2*time.Second)
			if callErr != nil {
				errCh <- callErr
				return
			}
			resultMu.Lock()
			results = append(results, result)
			resultMu.Unlock()
		}(message)
	}
	wg.Wait()
	close(errCh)

	for callErr := range errCh {
		t.Fatalf("unexpected dispatch error: %v", callErr)
	}

	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		seen[result] = struct{}{}
	}
	for _, message := range messages {
		if _, ok := seen[message]; !ok {
			t.Fatalf("expected result %q, got %#v", message, results)
		}
	}
}

func TestSubmitTaskSyncSuccess(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-task-sync", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	go payloadEchoResponder(stream)

	result, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeSync,
		Wait:       2 * time.Second,
		Timeout:    2 * time.Second,
		OwnerID:    "owner-a",
	})
	if err != nil {
		t.Fatalf("submit task failed: %v", err)
	}
	if !result.Completed {
		t.Fatalf("expected completed task for sync mode")
	}
	if result.Task.Status != TaskStatusSucceeded {
		t.Fatalf("expected succeeded status, got %s", result.Task.Status)
	}
	if !strings.Contains(string(result.Task.ResultJSON), `"hello-task"`) {
		t.Fatalf("expected echoed payload, got %s", string(result.Task.ResultJSON))
	}
}

func TestSubmitTaskRejectsEmptyOwnerID(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-task-empty-owner", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	go payloadEchoResponder(stream)

	_, err = svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeSync,
		Wait:       2 * time.Second,
		Timeout:    2 * time.Second,
		OwnerID:    "",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty owner_id, got %v", err)
	}
	if status.Convert(err).Message() != "owner_id is required" {
		t.Fatalf("expected owner_id is required message, got %q", status.Convert(err).Message())
	}
}

func TestSubmitTaskFailsWhenCapacityIsFull(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	if _, _, err := connectWorker(client, "node-1", "secret-1", "nonce-capacity", []string{"echo"}); err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	session := svc.getSession("node-1")
	if session == nil {
		t.Fatalf("expected active session for node-1")
	}

	for i := 0; i < defaultCapabilityMaxInflight; i++ {
		if !session.tryAcquireCapability("echo") {
			t.Fatalf("expected capability slot %d to be acquirable", i)
		}
	}
	defer func() {
		for i := 0; i < defaultCapabilityMaxInflight; i++ {
			session.releaseCapability("echo")
		}
	}()

	_, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeAsync,
		Wait:       100 * time.Millisecond,
		Timeout:    1 * time.Second,
		OwnerID:    "owner-a",
	})
	if !errors.Is(err, ErrNoWorkerCapacity) {
		t.Fatalf("expected ErrNoWorkerCapacity, got %v", err)
	}
}

func TestSubmitTaskRequestIDDedupConcurrent(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-task-request-concurrent", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	go payloadEchoResponder(stream)

	var createdTaskCount int32
	svc.newTaskIDFn = func() (string, error) {
		taskNumber := atomic.AddInt32(&createdTaskCount, 1)
		return fmt.Sprintf("task-request-%d", taskNumber), nil
	}

	request := SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeAsync,
		Wait:       100 * time.Millisecond,
		Timeout:    2 * time.Second,
		RequestID:  "request-concurrent-1",
		OwnerID:    "owner-a",
	}

	type submitResponse struct {
		result SubmitTaskResult
		err    error
	}
	results := make(chan submitResponse, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, submitErr := svc.SubmitTask(context.Background(), request)
			results <- submitResponse{result: result, err: submitErr}
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	successTaskIDs := make([]string, 0, 2)
	for response := range results {
		if response.err == nil {
			successTaskIDs = append(successTaskIDs, response.result.Task.TaskID)
			continue
		}
		if !errors.Is(response.err, ErrTaskRequestInProgress) {
			t.Fatalf("expected ErrTaskRequestInProgress or nil, got %v", response.err)
		}
	}

	if atomic.LoadInt32(&createdTaskCount) != 1 {
		t.Fatalf("expected only one created task, got %d", atomic.LoadInt32(&createdTaskCount))
	}
	if len(successTaskIDs) == 0 {
		t.Fatalf("expected at least one successful submit result")
	}
	firstTaskID := successTaskIDs[0]
	for _, taskID := range successTaskIDs {
		if taskID != firstTaskID {
			t.Fatalf("expected all successful submits to reference the same task_id, got %v", successTaskIDs)
		}
	}
}

func TestSubmitTaskRequestIDConflictWhileReserved(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-task-request-reserve", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	go payloadEchoResponder(stream)

	firstTaskIDReady := make(chan struct{})
	releaseFirstTaskID := make(chan struct{})
	var createdTaskCount int32
	svc.newTaskIDFn = func() (string, error) {
		taskNumber := atomic.AddInt32(&createdTaskCount, 1)
		if taskNumber == 1 {
			close(firstTaskIDReady)
			<-releaseFirstTaskID
		}
		return fmt.Sprintf("task-reserved-%d", taskNumber), nil
	}

	request := SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeAsync,
		Wait:       100 * time.Millisecond,
		Timeout:    2 * time.Second,
		RequestID:  "request-reserved-1",
		OwnerID:    "owner-a",
	}

	firstResultCh := make(chan SubmitTaskResult, 1)
	firstErrCh := make(chan error, 1)
	go func() {
		result, submitErr := svc.SubmitTask(context.Background(), request)
		if submitErr != nil {
			firstErrCh <- submitErr
			return
		}
		firstResultCh <- result
	}()

	select {
	case <-firstTaskIDReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first submit to reserve request_id")
	}

	_, err = svc.SubmitTask(context.Background(), request)
	if !errors.Is(err, ErrTaskRequestInProgress) {
		t.Fatalf("expected ErrTaskRequestInProgress, got %v", err)
	}

	close(releaseFirstTaskID)

	select {
	case submitErr := <-firstErrCh:
		t.Fatalf("expected first submit to succeed, got %v", submitErr)
	case result := <-firstResultCh:
		if strings.TrimSpace(result.Task.TaskID) == "" {
			t.Fatalf("expected first submit to produce a task_id")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first submit completion")
	}

	if atomic.LoadInt32(&createdTaskCount) != 1 {
		t.Fatalf("expected only one created task, got %d", atomic.LoadInt32(&createdTaskCount))
	}
}

func TestSubmitTaskRequestIDDedupIsolatedByOwner(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(client, "node-1", "secret-1", "nonce-task-request-owner", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	go payloadEchoResponder(stream)

	requestA := SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeAsync,
		Wait:       100 * time.Millisecond,
		Timeout:    2 * time.Second,
		RequestID:  "request-shared-1",
		OwnerID:    "owner-a",
	}
	requestB := requestA
	requestB.OwnerID = "owner-b"

	resultA, err := svc.SubmitTask(context.Background(), requestA)
	if err != nil {
		t.Fatalf("submit task for owner-a failed: %v", err)
	}
	resultB, err := svc.SubmitTask(context.Background(), requestB)
	if err != nil {
		t.Fatalf("submit task for owner-b failed: %v", err)
	}
	if strings.TrimSpace(resultA.Task.TaskID) == "" || strings.TrimSpace(resultB.Task.TaskID) == "" {
		t.Fatalf("expected non-empty task ids, got owner-a=%q owner-b=%q", resultA.Task.TaskID, resultB.Task.TaskID)
	}
	if resultA.Task.TaskID == resultB.Task.TaskID {
		t.Fatalf("expected different task ids for different owners, got %q", resultA.Task.TaskID)
	}
}

func TestGetAndCancelTaskAreOwnerScoped(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), map[string]string{"node-1": "secret-1"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	_, _, err := connectWorker(client, "node-1", "secret-1", "nonce-task-owner-scope", []string{"echo"})
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}

	result, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "echo",
		InputJSON:  []byte(`{"message":"hello-task"}`),
		Mode:       TaskModeAsync,
		Wait:       100 * time.Millisecond,
		Timeout:    2 * time.Second,
		OwnerID:    "owner-a",
	})
	if err != nil {
		t.Fatalf("submit task failed: %v", err)
	}
	taskID := strings.TrimSpace(result.Task.TaskID)
	if taskID == "" {
		t.Fatalf("expected non-empty task id")
	}

	if _, ok := svc.GetTask(taskID, "owner-a"); !ok {
		t.Fatalf("expected owner-a to see the task")
	}
	if _, ok := svc.GetTask(taskID, "owner-b"); ok {
		t.Fatalf("expected owner-b not to see owner-a task")
	}

	if _, err := svc.CancelTask(taskID, "owner-b"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expected owner-b cancel to return ErrTaskNotFound, got %v", err)
	}
	canceled, err := svc.CancelTask(taskID, "owner-a")
	if err != nil {
		t.Fatalf("expected owner-a cancel success, got %v", err)
	}
	if canceled.Status != TaskStatusCanceled {
		t.Fatalf("expected canceled status, got %s", canceled.Status)
	}
}

func TestPendingCommandCloseResultIsIdempotent(t *testing.T) {
	pending := &pendingCommand{
		resultCh: make(chan commandOutcome, 1),
	}

	first := commandOutcome{message: "first-result"}
	second := commandOutcome{message: "second-result"}
	pending.closeResult(&first)
	pending.closeResult(&second)
	pending.closeResult(nil)

	outcome, ok := <-pending.resultCh
	if !ok {
		t.Fatalf("expected first outcome before channel close")
	}
	if outcome.message != first.message {
		t.Fatalf("expected %q, got %q", first.message, outcome.message)
	}

	_, ok = <-pending.resultCh
	if ok {
		t.Fatalf("expected result channel to be closed")
	}
}

func TestDispatchCommandContextCanceledAfterEnqueueCleansPending(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Now()
	hello := &registryv1.ConnectHello{
		NodeId:       "node-1",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: "echo"}},
	}
	if err := svc.store.Upsert(hello, "session-1", now); err != nil {
		t.Fatalf("seed store upsert failed: %v", err)
	}
	session := newActiveSession("node-1", "session-1", hello)
	svc.swapSession(session)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, dispatchErr := svc.dispatchCommand(ctx, "echo", buildEchoPayload("cleanup"), 2*time.Second, nil)
		errCh <- dispatchErr
	}()

	select {
	case response := <-session.commandOutbound:
		dispatch := response.GetCommandDispatch()
		if dispatch == nil {
			t.Fatalf("expected command dispatch frame, got %#v", response.GetPayload())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for command dispatch enqueue")
	}

	cancel()

	select {
	case dispatchErr := <-errCh:
		if !errors.Is(dispatchErr, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", dispatchErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for dispatchCommand to return")
	}

	session.pendingMu.Lock()
	pendingCount := len(session.pending)
	session.pendingMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("expected no pending command after cancellation, got %d", pendingCount)
	}

	inflight, _, ok := session.inflightSnapshot("echo")
	if !ok {
		t.Fatalf("expected echo capability snapshot")
	}
	if inflight != 0 {
		t.Fatalf("expected inflight to be released, got %d", inflight)
	}
}

func TestCloseTaskRuntimeRecordCancelRunsOnce(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	record := &taskRecord{
		id:     "task-cancel-once",
		status: TaskStatusRunning,
		done:   make(chan struct{}),
	}

	var cancelCallCount int32
	record.cancel = func() {
		atomic.AddInt32(&cancelCallCount, 1)
	}

	svc.closeTaskRuntimeRecord(record)
	svc.closeTaskRuntimeRecord(record)

	if atomic.LoadInt32(&cancelCallCount) != 1 {
		t.Fatalf("expected cancel to run once, got %d", atomic.LoadInt32(&cancelCallCount))
	}
	if record.cancel != nil {
		t.Fatalf("expected cancel function to be cleared")
	}

	select {
	case <-record.done:
	default:
		t.Fatalf("expected done channel to be closed")
	}
}

func connectWorker(
	client registryv1.WorkerRegistryServiceClient,
	workerID string,
	secret string,
	nonce string,
	capabilities []string,
) (grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse], string, error) {
	_ = nonce
	stream, err := client.Connect(context.Background())
	if err != nil {
		return nil, "", err
	}

	hello := &registryv1.ConnectHello{
		NodeId:       workerID,
		WorkerSecret: secret,
	}
	for _, capability := range capabilities {
		hello.Capabilities = append(hello.Capabilities, &registryv1.CapabilityDeclaration{Name: capability})
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		return nil, "", err
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, "", err
	}
	ack := resp.GetConnectAck()
	if ack == nil {
		return nil, "", fmt.Errorf("expected connect_ack, got %#v", resp.GetPayload())
	}
	return stream, ack.GetSessionId(), nil
}

func echoResponder(stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse]) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil {
			continue
		}
		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId: dispatch.GetCommandId(),
					Echo: &registryv1.EchoResult{
						Message: dispatch.GetEcho().GetMessage(),
					},
				},
			},
		})
	}
}

func countingEchoResponder(stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse], counter *int32) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil {
			continue
		}
		atomic.AddInt32(counter, 1)
		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId: dispatch.GetCommandId(),
					Echo: &registryv1.EchoResult{
						Message: dispatch.GetEcho().GetMessage(),
					},
				},
			},
		})
	}
}

func payloadEchoResponder(stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse]) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil {
			continue
		}
		payload := dispatch.GetPayloadJson()
		message := dispatch.GetEcho().GetMessage()
		if len(payload) == 0 {
			payload = buildEchoPayload(message)
		}
		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     payload,
					CompletedUnixMs: time.Now().UnixMilli(),
				},
			},
		})
	}
}
