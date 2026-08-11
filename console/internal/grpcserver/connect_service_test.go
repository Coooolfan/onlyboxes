package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
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
		"passthrough:///bufnet",
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
				NodeId:    "node-1",
				SessionId: "session-x",
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

func TestCreateProvisionedWorkerForOwnerLimitsWorkerSysToOne(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := NewRegistryService(store, map[string]string{}, 5, 15, 60*time.Second)
	now := time.Now()

	firstID, firstSecret, err := svc.CreateProvisionedWorkerForOwner("owner-a", registry.WorkerTypeSys, now, 15*time.Second)
	if err != nil {
		t.Fatalf("create first worker-sys failed: %v", err)
	}
	if strings.TrimSpace(firstID) == "" || strings.TrimSpace(firstSecret) == "" {
		t.Fatalf("expected non-empty first worker-sys credentials")
	}

	_, _, err = svc.CreateProvisionedWorkerForOwner("owner-a", registry.WorkerTypeSys, now, 15*time.Second)
	if !errors.Is(err, ErrWorkerSysAlreadyExists) {
		t.Fatalf("expected ErrWorkerSysAlreadyExists, got %v", err)
	}
}

func TestCreateProvisionedWorkerForOwnerWorkerSysConcurrentSingleton(t *testing.T) {
	store := registrytest.NewStore(t)
	svc := NewRegistryService(store, map[string]string{}, 5, 15, 60*time.Second)
	now := time.Now()

	const concurrentCreates = 16
	var wg sync.WaitGroup
	var successCount atomic.Int32
	errCh := make(chan error, concurrentCreates)

	start := make(chan struct{})
	for i := 0; i < concurrentCreates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			workerID, workerSecret, err := svc.CreateProvisionedWorkerForOwner("owner-a", registry.WorkerTypeSys, now, 15*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			if strings.TrimSpace(workerID) == "" || strings.TrimSpace(workerSecret) == "" {
				errCh <- errors.New("expected non-empty worker-sys credentials")
				return
			}
			successCount.Add(1)
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if !errors.Is(err, ErrWorkerSysAlreadyExists) {
			t.Fatalf("expected ErrWorkerSysAlreadyExists for non-winning calls, got %v", err)
		}
	}
	if successCount.Load() != 1 {
		t.Fatalf("expected exactly one successful worker-sys creation, got %d", successCount.Load())
	}
	if count := store.CountWorkersByOwnerAndType("owner-a", registry.WorkerTypeSys); count != 1 {
		t.Fatalf("expected one worker-sys in store, got %d", count)
	}
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

	if removed, err := svc.DeleteProvisionedWorker(workerID); err != nil || !removed {
		t.Fatalf("expected delete to return true, removed=%t err=%v", removed, err)
	}
	if _, ok := svc.GetWorkerSecret(workerID); ok {
		t.Fatalf("expected credential to be revoked")
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:    workerID,
				SessionId: sessionID,
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

func TestConnectRejectsWorkerSysNonAllowedCapability(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_010, 0)
	seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-sys",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)
	if seeded != 1 {
		t.Fatalf("expected one seeded worker, got %d", seeded)
	}

	svc := NewRegistryService(store, map[string]string{"node-sys": "secret-sys"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	_, _, err := connectWorker(client, "node-sys", "secret-sys", "nonce-sys-invalid-capability", []string{"echo"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestConnectRejectsWorkerSysMissingReadImageCapability(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_015, 0)
	seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-sys",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)
	if seeded != 1 {
		t.Fatalf("expected one seeded worker, got %d", seeded)
	}

	svc := NewRegistryService(store, map[string]string{"node-sys": "secret-sys"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	_, _, err := connectWorker(client, "node-sys", "secret-sys", "nonce-sys-missing-read-image", []string{computerUseCapabilityDeclared})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestConnectWorkerSysForcesRequiredCapabilitiesMaxInflightOne(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_020, 0)
	seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-sys",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)
	if seeded != 1 {
		t.Fatalf("expected one seeded worker, got %d", seeded)
	}

	svc := NewRegistryService(store, map[string]string{"node-sys": "secret-sys"}, 5, 15, 60*time.Second)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	stream, _, err := connectWorker(
		client,
		"node-sys",
		"secret-sys",
		"nonce-sys-required-capabilities",
		[]string{computerUseCapabilityDeclared, readImageCapabilityDeclared},
	)
	if err != nil {
		t.Fatalf("connect worker failed: %v", err)
	}
	defer stream.CloseSend()

	session := svc.getSession("node-sys")
	if session == nil {
		t.Fatalf("expected active session for node-sys")
	}
	_, maxInflight, ok := session.inflightSnapshot(computerUseCapabilityName)
	if !ok {
		t.Fatalf("expected computerUse capability to be present")
	}
	if maxInflight != 1 {
		t.Fatalf("expected computerUse max_inflight to be forced to 1, got %d", maxInflight)
	}
	_, maxInflight, ok = session.inflightSnapshot(readImageCapabilityName)
	if !ok {
		t.Fatalf("expected readImage capability to be present")
	}
	if maxInflight != 1 {
		t.Fatalf("expected readImage max_inflight to be forced to 1, got %d", maxInflight)
	}
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
				NodeId:    "node-1",
				SessionId: sessionID,
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
		if err := session.enqueueControl(context.Background(), newHeartbeatAck(5)); err != nil {
			t.Fatalf("failed to preload control queue: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := svc.handleHeartbeat(ctx, session, &registryv1.HeartbeatFrame{
		NodeId:    "node-1",
		SessionId: "session-1",
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
				NodeId:    "node-1",
				SessionId: sessionA,
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
				NodeId:    "node-1",
				SessionId: sessionB,
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

func TestSubmitTaskComputerUseRoutesByOwnerAndCapacity(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_050, 0)
	seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-owner-a",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
		{
			NodeID: "node-owner-b",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-b",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)
	if seeded != 2 {
		t.Fatalf("expected two seeded workers, got %d", seeded)
	}

	svc := NewRegistryService(
		store,
		map[string]string{
			"node-owner-a": "secret-owner-a",
			"node-owner-b": "secret-owner-b",
		},
		5,
		15,
		60*time.Second,
	)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	streamA, _, err := connectWorker(client, "node-owner-a", "secret-owner-a", "nonce-owner-a", []string{computerUseCapabilityDeclared, readImageCapabilityDeclared})
	if err != nil {
		t.Fatalf("connect worker owner-a failed: %v", err)
	}
	defer streamA.CloseSend()
	streamB, _, err := connectWorker(client, "node-owner-b", "secret-owner-b", "nonce-owner-b", []string{computerUseCapabilityDeclared, readImageCapabilityDeclared})
	if err != nil {
		t.Fatalf("connect worker owner-b failed: %v", err)
	}
	defer streamB.CloseSend()

	go computerUseResponder(streamA, "owner-a")
	go computerUseResponder(streamB, "owner-b")

	resultB, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "computerUse",
		InputJSON:  []byte(`{"command":"echo owner-b"}`),
		Mode:       TaskModeSync,
		Timeout:    2 * time.Second,
		OwnerID:    "owner-b",
	})
	if err != nil {
		t.Fatalf("submit owner-b task failed: %v", err)
	}
	if !resultB.Completed || resultB.Task.Status != TaskStatusSucceeded {
		t.Fatalf("expected owner-b task succeeded, got completed=%v status=%s", resultB.Completed, resultB.Task.Status)
	}
	if !strings.Contains(string(resultB.Task.ResultJSON), "owner-b") {
		t.Fatalf("expected owner-b worker result, got %s", string(resultB.Task.ResultJSON))
	}

	sessionA := svc.getSession("node-owner-a")
	if sessionA == nil {
		t.Fatalf("expected active session for owner-a worker")
	}
	if !sessionA.tryAcquireCapability(computerUseCapabilityName) {
		t.Fatalf("expected to acquire owner-a computerUse slot")
	}
	defer sessionA.releaseCapability(computerUseCapabilityName)

	_, err = svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "computerUse",
		InputJSON:  []byte(`{"command":"echo owner-a"}`),
		Mode:       TaskModeSync,
		Timeout:    500 * time.Millisecond,
		OwnerID:    "owner-a",
	})
	if !errors.Is(err, ErrNoWorkerCapacity) {
		t.Fatalf("expected ErrNoWorkerCapacity for owner-a, got %v", err)
	}
}

func TestSubmitTaskReadImageRoutesByOwnerAndCapacity(t *testing.T) {
	store := registrytest.NewStore(t)
	now := time.Unix(1_700_000_055, 0)
	seeded := store.SeedProvisionedWorkers([]registry.ProvisionedWorker{
		{
			NodeID: "node-owner-a",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-a",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
		{
			NodeID: "node-owner-b",
			Labels: map[string]string{
				registry.LabelOwnerIDKey:    "owner-b",
				registry.LabelWorkerTypeKey: registry.WorkerTypeSys,
			},
		},
	}, now, 15*time.Second)
	if seeded != 2 {
		t.Fatalf("expected two seeded workers, got %d", seeded)
	}

	svc := NewRegistryService(
		store,
		map[string]string{
			"node-owner-a": "secret-owner-a",
			"node-owner-b": "secret-owner-b",
		},
		5,
		15,
		60*time.Second,
	)
	client, cleanup := newBufClient(t, svc)
	defer cleanup()

	streamA, _, err := connectWorker(client, "node-owner-a", "secret-owner-a", "nonce-owner-a-read-image", []string{computerUseCapabilityDeclared, readImageCapabilityDeclared})
	if err != nil {
		t.Fatalf("connect worker owner-a failed: %v", err)
	}
	defer streamA.CloseSend()
	streamB, _, err := connectWorker(client, "node-owner-b", "secret-owner-b", "nonce-owner-b-read-image", []string{computerUseCapabilityDeclared, readImageCapabilityDeclared})
	if err != nil {
		t.Fatalf("connect worker owner-b failed: %v", err)
	}
	defer streamB.CloseSend()

	go readImageResponder(streamA, "owner-a")
	go readImageResponder(streamB, "owner-b")

	resultB, err := svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "readImage",
		InputJSON:  []byte(`{"session_id":"computerUse","file_path":"/workspace/image.png","action":"validate"}`),
		Mode:       TaskModeSync,
		Timeout:    2 * time.Second,
		OwnerID:    "owner-b",
	})
	if err != nil {
		t.Fatalf("submit owner-b readImage task failed: %v", err)
	}
	if !resultB.Completed || resultB.Task.Status != TaskStatusSucceeded {
		t.Fatalf("expected owner-b readImage task succeeded, got completed=%v status=%s", resultB.Completed, resultB.Task.Status)
	}
	if !strings.Contains(string(resultB.Task.ResultJSON), "owner-b") {
		t.Fatalf("expected owner-b readImage worker result, got %s", string(resultB.Task.ResultJSON))
	}

	sessionA := svc.getSession("node-owner-a")
	if sessionA == nil {
		t.Fatalf("expected active session for owner-a worker")
	}
	if !sessionA.tryAcquireCapability(readImageCapabilityName) {
		t.Fatalf("expected to acquire owner-a readImage slot")
	}
	defer sessionA.releaseCapability(readImageCapabilityName)

	_, err = svc.SubmitTask(context.Background(), SubmitTaskRequest{
		Capability: "readImage",
		InputJSON:  []byte(`{"session_id":"computerUse","file_path":"/workspace/image.png","action":"validate"}`),
		Mode:       TaskModeSync,
		Timeout:    500 * time.Millisecond,
		OwnerID:    "owner-a",
	})
	if !errors.Is(err, ErrNoWorkerCapacity) {
		t.Fatalf("expected ErrNoWorkerCapacity for owner-a readImage, got %v", err)
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

func TestSubmitTaskTerminalSessionReusedOnSameWorker(t *testing.T) {
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

	streamA, _, err := connectWorker(client, "node-a", "secret-a", "nonce-terminal-a", []string{"terminalexec"})
	if err != nil {
		t.Fatalf("connect worker A failed: %v", err)
	}
	streamB, _, err := connectWorker(client, "node-b", "secret-b", "nonce-terminal-b", []string{"terminalexec"})
	if err != nil {
		t.Fatalf("connect worker B failed: %v", err)
	}

	go terminalExecResponder(streamA, "worker-a")
	go terminalExecResponder(streamB, "worker-b")

	type terminalResult struct {
		SessionID string `json:"session_id"`
		Stdout    string `json:"stdout"`
	}

	submitTerminal := func(sessionID string) (terminalResult, error) {
		input := `{"command":"uname -a"}`
		if strings.TrimSpace(sessionID) != "" {
			input = fmt.Sprintf(`{"command":"uname -a","session_id":"%s"}`, sessionID)
		}
		result, submitErr := svc.SubmitTask(context.Background(), SubmitTaskRequest{
			Capability: "terminalExec",
			InputJSON:  []byte(input),
			Mode:       TaskModeSync,
			Timeout:    2 * time.Second,
			OwnerID:    "owner-a",
		})
		if submitErr != nil {
			return terminalResult{}, submitErr
		}
		if result.Task.Status != TaskStatusSucceeded {
			return terminalResult{}, fmt.Errorf("unexpected task status: %s", result.Task.Status)
		}
		output := terminalResult{}
		if err := json.Unmarshal(result.Task.ResultJSON, &output); err != nil {
			return terminalResult{}, err
		}
		return output, nil
	}

	first, err := submitTerminal("")
	if err != nil {
		t.Fatalf("first terminal submit failed: %v", err)
	}
	if strings.TrimSpace(first.SessionID) == "" {
		t.Fatalf("expected non-empty session_id in first result")
	}
	if !strings.Contains(first.Stdout, "worker-a") && !strings.Contains(first.Stdout, "worker-b") {
		t.Fatalf("expected worker marker in stdout, got %q", first.Stdout)
	}

	for i := 0; i < 3; i++ {
		next, submitErr := submitTerminal(first.SessionID)
		if submitErr != nil {
			t.Fatalf("reuse submit #%d failed: %v", i+2, submitErr)
		}
		if next.SessionID != first.SessionID {
			t.Fatalf("expected session_id %q, got %q", first.SessionID, next.SessionID)
		}
		if next.Stdout != first.Stdout {
			t.Fatalf("expected stable stdout %q, got %q", first.Stdout, next.Stdout)
		}
	}
}

func TestSubmitTaskTerminalConcurrentFirstReuseSticksToOneWorker(t *testing.T) {
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

	streamA, _, err := connectWorker(client, "node-a", "secret-a", "nonce-terminal-concurrent-a", []string{"terminalexec"})
	if err != nil {
		t.Fatalf("connect worker A failed: %v", err)
	}
	streamB, _, err := connectWorker(client, "node-b", "secret-b", "nonce-terminal-concurrent-b", []string{"terminalexec"})
	if err != nil {
		t.Fatalf("connect worker B failed: %v", err)
	}

	go delayedTerminalExecResponder(streamA, "worker-a", 200*time.Millisecond)
	go delayedTerminalExecResponder(streamB, "worker-b", 200*time.Millisecond)

	type terminalResult struct {
		SessionID string `json:"session_id"`
		Stdout    string `json:"stdout"`
	}
	submitTerminal := func(sessionID string) (terminalResult, error) {
		input := fmt.Sprintf(`{"command":"uname -a","session_id":"%s","create_if_missing":true}`, sessionID)
		result, submitErr := svc.SubmitTask(context.Background(), SubmitTaskRequest{
			Capability: "terminalExec",
			InputJSON:  []byte(input),
			Mode:       TaskModeSync,
			Timeout:    2 * time.Second,
			OwnerID:    "owner-a",
		})
		if submitErr != nil {
			return terminalResult{}, submitErr
		}
		if result.Task.Status != TaskStatusSucceeded {
			return terminalResult{}, fmt.Errorf("unexpected task status: %s", result.Task.Status)
		}
		output := terminalResult{}
		if err := json.Unmarshal(result.Task.ResultJSON, &output); err != nil {
			return terminalResult{}, err
		}
		return output, nil
	}

	for round := 0; round < 5; round++ {
		sessionID := fmt.Sprintf("session-concurrent-%d", round+1)
		start := make(chan struct{})
		type callResult struct {
			result terminalResult
			err    error
		}
		callResults := make(chan callResult, 2)
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				result, submitErr := submitTerminal(sessionID)
				callResults <- callResult{result: result, err: submitErr}
			}()
		}
		close(start)

		first := callResult{}
		second := callResult{}
		for i := 0; i < 2; i++ {
			select {
			case result := <-callResults:
				if i == 0 {
					first = result
				} else {
					second = result
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for concurrent submit result #%d in round %d", i+1, round+1)
			}
		}

		if first.err != nil {
			t.Fatalf("first concurrent submit failed in round %d: %v", round+1, first.err)
		}
		if second.err != nil {
			t.Fatalf("second concurrent submit failed in round %d: %v", round+1, second.err)
		}
		if first.result.SessionID != sessionID || second.result.SessionID != sessionID {
			t.Fatalf("expected both results to keep session_id=%q in round %d, got %q and %q", sessionID, round+1, first.result.SessionID, second.result.SessionID)
		}
		if first.result.Stdout != second.result.Stdout {
			t.Fatalf("expected same worker marker for concurrent first reuse in round %d, got %q and %q", round+1, first.result.Stdout, second.result.Stdout)
		}
	}
}

func TestDispatchCommandTerminalRouteRollbackWhenEnqueueFails(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_000, 0)
	svc.nowFn = func() time.Time {
		return now
	}

	hello := &registryv1.ConnectHello{
		NodeId:       "node-1",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec}},
	}
	if err := svc.store.Upsert(hello, "worker-session-1", now); err != nil {
		t.Fatalf("seed store upsert failed: %v", err)
	}
	session := newActiveSession("node-1", "worker-session-1", hello)
	svc.swapSession(session)

	for i := 0; i < commandOutboundBufferSize; i++ {
		if err := session.enqueueCommand(context.Background(), &registryv1.ConnectResponse{}); err != nil {
			t.Fatalf("failed to preload command queue at %d: %v", i, err)
		}
	}

	payloadJSON, err := json.Marshal(terminalExecScopedPayload{
		Command:         "uname -a",
		SessionID:       "session-rollback",
		CreateIfMissing: true,
	})
	if err != nil {
		t.Fatalf("marshal payload failed: %v", err)
	}

	_, dispatchErr := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payloadJSON, 30*time.Millisecond, dispatchOptions{ownerID: "owner-a"})
	if !errors.Is(dispatchErr, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", dispatchErr)
	}
	if _, ok := svc.touchTerminalSessionRoute("session-rollback", now.Add(time.Second)); ok {
		t.Fatalf("expected route to be rolled back after enqueue failure")
	}
}

func TestDispatchCommandTerminalSessionNotFoundClearsRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_200, 0)
	svc.nowFn = func() time.Time {
		return now
	}

	hello := &registryv1.ConnectHello{
		NodeId:       "node-1",
		Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec}},
	}
	if err := svc.store.Upsert(hello, "worker-session-1", now); err != nil {
		t.Fatalf("seed store upsert failed: %v", err)
	}
	session := newActiveSession("node-1", "worker-session-1", hello)
	svc.swapSession(session)
	svc.bindTerminalSessionRoute("session-missing", "node-1", now)

	go func() {
		response := <-session.commandOutbound
		dispatch := response.GetCommandDispatch()
		if dispatch == nil {
			return
		}
		session.resolvePending(&registryv1.CommandResult{
			CommandId: dispatch.GetCommandId(),
			Error: &registryv1.CommandError{
				Code:    terminalSessionNotFoundCode,
				Message: "session not found",
			},
			CompletedUnixMs: now.UnixMilli(),
		})
	}()

	payloadJSON, err := json.Marshal(terminalExecScopedPayload{
		Command:   "uname -a",
		SessionID: "session-missing",
	})
	if err != nil {
		t.Fatalf("marshal payload failed: %v", err)
	}

	outcome, dispatchErr := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payloadJSON, 2*time.Second, dispatchOptions{ownerID: "owner-a"})
	if dispatchErr != nil {
		t.Fatalf("dispatch command failed: %v", dispatchErr)
	}
	var commandErr *CommandExecutionError
	if !errors.As(outcome.err, &commandErr) {
		t.Fatalf("expected CommandExecutionError, got %v", outcome.err)
	}
	if commandErr.Code != terminalSessionNotFoundCode {
		t.Fatalf("expected %q code, got %q", terminalSessionNotFoundCode, commandErr.Code)
	}
	if _, ok := svc.touchTerminalSessionRoute("session-missing", now.Add(time.Second)); ok {
		t.Fatalf("expected route to be cleared after session_not_found")
	}
}

func TestDispatchCommandTerminalSessionCapacityOnlyClearsNewRoute(t *testing.T) {
	tests := []struct {
		name             string
		prebindRoute     bool
		wantRoutePresent bool
	}{
		{name: "new_route_is_cleared", wantRoutePresent: false},
		{name: "existing_route_is_retained", prebindRoute: true, wantRoutePresent: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
			now := time.Unix(1_700_000_300, 0)
			svc.nowFn = func() time.Time { return now }

			hello := &registryv1.ConnectHello{
				NodeId:       "node-1",
				Capabilities: []*registryv1.CapabilityDeclaration{{Name: taskCapabilityTerminalExec}},
			}
			if err := svc.store.Upsert(hello, "worker-session-1", now); err != nil {
				t.Fatalf("seed store upsert failed: %v", err)
			}
			session := newActiveSession("node-1", "worker-session-1", hello)
			svc.swapSession(session)
			if tc.prebindRoute {
				svc.bindTerminalSessionRoute("session-full", "node-1", now)
			}

			go func() {
				response := <-session.commandOutbound
				dispatch := response.GetCommandDispatch()
				if dispatch == nil {
					return
				}
				session.resolvePending(&registryv1.CommandResult{
					CommandId: dispatch.GetCommandId(),
					Error: &registryv1.CommandError{
						Code:    terminalSessionCapacityExceededCode,
						Message: "terminal session capacity exceeded",
					},
					CompletedUnixMs: now.UnixMilli(),
				})
			}()

			payloadJSON, err := json.Marshal(terminalExecScopedPayload{
				Command:         "uname -a",
				SessionID:       "session-full",
				CreateIfMissing: true,
			})
			if err != nil {
				t.Fatalf("marshal payload failed: %v", err)
			}

			outcome, dispatchErr := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, payloadJSON, 2*time.Second, dispatchOptions{ownerID: "owner-a"})
			if dispatchErr != nil {
				t.Fatalf("dispatch command failed: %v", dispatchErr)
			}
			var commandErr *CommandExecutionError
			if !errors.As(outcome.err, &commandErr) {
				t.Fatalf("expected CommandExecutionError, got %v", outcome.err)
			}
			if commandErr.Code != terminalSessionCapacityExceededCode {
				t.Fatalf("expected %q code, got %q", terminalSessionCapacityExceededCode, commandErr.Code)
			}
			_, routePresent := svc.touchTerminalSessionRoute("session-full", now.Add(time.Second))
			if routePresent != tc.wantRoutePresent {
				t.Fatalf("route presence: got %t want %t", routePresent, tc.wantRoutePresent)
			}
		})
	}
}

func TestDispatchCommandTerminalSessionCapacityDoesNotClearConcurrentProvisionalRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_350, 0)
	svc.nowFn = func() time.Time { return now }

	hello := &registryv1.ConnectHello{
		NodeId: "node-1",
		Capabilities: []*registryv1.CapabilityDeclaration{{
			Name:        taskCapabilityTerminalExec,
			MaxInflight: 2,
		}},
	}
	if err := svc.store.Upsert(hello, "worker-session-1", now); err != nil {
		t.Fatalf("seed store upsert failed: %v", err)
	}
	session := newActiveSession("node-1", "worker-session-1", hello)
	svc.swapSession(session)

	payload := func(command string) []byte {
		t.Helper()
		encoded, err := json.Marshal(terminalExecScopedPayload{
			Command:         command,
			SessionID:       "session-shared",
			CreateIfMissing: true,
		})
		if err != nil {
			t.Fatalf("marshal payload failed: %v", err)
		}
		return encoded
	}
	firstPayload := payload("first")
	secondPayload := payload("second")
	type dispatchResult struct {
		outcome commandOutcome
		err     error
	}
	firstDone := make(chan dispatchResult, 1)
	secondDone := make(chan dispatchResult, 1)

	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, firstPayload, 2*time.Second, dispatchOptions{ownerID: "owner-a"})
		firstDone <- dispatchResult{outcome: outcome, err: err}
	}()
	firstDispatch := (<-session.commandOutbound).GetCommandDispatch()
	if firstDispatch == nil {
		t.Fatal("expected first command dispatch")
	}

	go func() {
		outcome, err := svc.dispatchCommand(context.Background(), taskCapabilityTerminalExec, secondPayload, 2*time.Second, dispatchOptions{ownerID: "owner-a"})
		secondDone <- dispatchResult{outcome: outcome, err: err}
	}()
	secondDispatch := (<-session.commandOutbound).GetCommandDispatch()
	if secondDispatch == nil {
		t.Fatal("expected second command dispatch")
	}

	session.resolvePending(&registryv1.CommandResult{
		CommandId: firstDispatch.GetCommandId(),
		Error: &registryv1.CommandError{
			Code:    terminalSessionCapacityExceededCode,
			Message: "terminal session capacity exceeded",
		},
		CompletedUnixMs: now.UnixMilli(),
	})
	first := <-firstDone
	if first.err != nil || !isSessionCapacityCommandError(first.outcome.err) {
		t.Fatalf("unexpected first result: outcome=%v err=%v", first.outcome.err, first.err)
	}
	if nodeID, ok := svc.touchTerminalSessionRoute("session-shared", now.Add(time.Second)); !ok || nodeID != "node-1" {
		t.Fatalf("capacity rollback cleared an in-flight route: node=%q ok=%t", nodeID, ok)
	}

	session.resolvePending(&registryv1.CommandResult{
		CommandId:       secondDispatch.GetCommandId(),
		PayloadJson:     []byte(`{"session_id":"session-shared","lease_expires_unix_ms":4102444800000}`),
		CompletedUnixMs: now.UnixMilli(),
	})
	second := <-secondDone
	if second.err != nil || second.outcome.err != nil {
		t.Fatalf("unexpected second result: outcome=%v err=%v", second.outcome.err, second.err)
	}

	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-shared"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != "node-1" || route.ReservationID != 0 || route.ProvisionalUses != 0 {
		t.Fatalf("successful concurrent dispatch did not confirm route: %#v", route)
	}
}

func TestTerminalSessionRouteReservationWaitsForAllProvisionalUses(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_375, 0)

	_, reservationID := svc.reserveTerminalSessionRoute("session-shared", "node-1", now)
	nodeID, joinedReservationID, ok := svc.claimTerminalSessionRoute("session-shared", now.Add(time.Second))
	if !ok || nodeID != "node-1" || reservationID == 0 || joinedReservationID != reservationID {
		t.Fatalf("failed to join provisional route: node=%q reservation=%d joined=%d ok=%t", nodeID, reservationID, joinedReservationID, ok)
	}

	if got := svc.clearTerminalSessionRouteReservation("session-shared", "node-1", reservationID); got != routeReservationStillInUse {
		t.Fatalf("first rollback result: got %d want %d", got, routeReservationStillInUse)
	}
	if _, ok := svc.touchTerminalSessionRoute("session-shared", now.Add(2*time.Second)); !ok {
		t.Fatal("first rollback cleared a route still used by another dispatch")
	}

	if got := svc.clearTerminalSessionRouteReservation("session-shared", "node-1", joinedReservationID); got != routeReservationRemoved {
		t.Fatalf("last rollback result: got %d want %d", got, routeReservationRemoved)
	}
	if _, ok := svc.touchTerminalSessionRoute("session-shared", now.Add(3*time.Second)); ok {
		t.Fatal("last rollback did not clear provisional route")
	}
}

func TestTerminalSessionRouteReservationRollbackDoesNotClearConfirmedRoute(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_400, 0)

	nodeID, reservationID := svc.reserveTerminalSessionRoute("session-confirmed", "node-1", now)
	if nodeID != "node-1" || reservationID == 0 {
		t.Fatalf("expected a provisional route, got node=%q reservation=%d", nodeID, reservationID)
	}

	// A concurrent request can complete first and confirm the shared route.
	svc.bindTerminalSessionRoute("session-confirmed", "node-1", now.Add(time.Second))
	if got := svc.clearTerminalSessionRouteReservation("session-confirmed", "node-1", reservationID); got != routeReservationNotOwned {
		t.Fatalf("late rollback result: got %d want %d", got, routeReservationNotOwned)
	}

	if gotNodeID, ok := svc.touchTerminalSessionRoute("session-confirmed", now.Add(2*time.Second)); !ok || gotNodeID != "node-1" {
		t.Fatalf("late capacity rollback cleared a confirmed route: node=%q ok=%t", gotNodeID, ok)
	}
}

func TestTerminalSessionRouteReservationRejectsStaleABAToken(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_450, 0)

	_, firstReservationID := svc.reserveTerminalSessionRoute("session-aba", "node-1", now)
	svc.clearTerminalSessionRoute("session-aba", "node-1")
	_, secondReservationID := svc.reserveTerminalSessionRoute("session-aba", "node-1", now.Add(time.Second))
	if firstReservationID == 0 || secondReservationID == 0 || firstReservationID == secondReservationID {
		t.Fatalf("expected distinct route reservation IDs, first=%d second=%d", firstReservationID, secondReservationID)
	}

	svc.clearTerminalSessionRouteReservation("session-aba", "node-1", firstReservationID)
	if gotNodeID, ok := svc.touchTerminalSessionRoute("session-aba", now.Add(2*time.Second)); !ok || gotNodeID != "node-1" {
		t.Fatalf("stale ABA rollback cleared a newer reservation: node=%q ok=%t", gotNodeID, ok)
	}

	svc.clearTerminalSessionRouteReservation("session-aba", "node-1", secondReservationID)
	if _, ok := svc.touchTerminalSessionRoute("session-aba", now.Add(3*time.Second)); ok {
		t.Fatal("current route reservation was not cleared")
	}
}

func TestTerminalSessionRouteConfirmationRejectsStaleABAToken(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	now := time.Unix(1_700_000_475, 0)

	_, firstReservationID := svc.reserveTerminalSessionRoute("session-confirm-aba", "node-1", now)
	svc.clearTerminalSessionRoute("session-confirm-aba", "node-1")
	_, secondReservationID := svc.reserveTerminalSessionRoute("session-confirm-aba", "node-1", now.Add(time.Second))
	if firstReservationID == 0 || secondReservationID == 0 || firstReservationID == secondReservationID {
		t.Fatalf("expected distinct route reservation IDs, first=%d second=%d", firstReservationID, secondReservationID)
	}

	if svc.confirmTerminalSessionRoute("session-confirm-aba", "node-1", firstReservationID, now.Add(2*time.Second)) {
		t.Fatal("stale reservation unexpectedly confirmed a newer route")
	}
	svc.terminalRoutesMu.RLock()
	route := svc.terminalSessionToNode["session-confirm-aba"]
	svc.terminalRoutesMu.RUnlock()
	if route.ReservationID != secondReservationID || route.ProvisionalUses != 1 {
		t.Fatalf("stale confirmation changed newer reservation: %#v", route)
	}

	if !svc.confirmTerminalSessionRoute("session-confirm-aba", "node-1", secondReservationID, now.Add(3*time.Second)) {
		t.Fatal("current reservation was not confirmed")
	}
	svc.terminalRoutesMu.RLock()
	route = svc.terminalSessionToNode["session-confirm-aba"]
	svc.terminalRoutesMu.RUnlock()
	if route.NodeID != "node-1" || route.ReservationID != 0 || route.ProvisionalUses != 0 {
		t.Fatalf("current confirmation did not stabilize route: %#v", route)
	}
}

func TestPruneExpiredTerminalSessionRoutes(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	svc.terminalRouteTTL = 1000 * time.Millisecond
	base := time.Unix(1_700_000_500, 0)

	svc.bindTerminalSessionRoute("session-expired", "node-1", base)
	svc.bindTerminalSessionRoute("session-fresh", "node-1", base.Add(900*time.Millisecond))

	removed := svc.PruneExpiredTerminalSessionRoutes(base.Add(1500 * time.Millisecond))
	if removed != 1 {
		t.Fatalf("expected one expired route removed, got %d", removed)
	}
	if _, ok := svc.touchTerminalSessionRoute("session-expired", base.Add(2*time.Second)); ok {
		t.Fatalf("expected expired route to be pruned")
	}
	nodeID, ok := svc.touchTerminalSessionRoute("session-fresh", base.Add(2*time.Second))
	if !ok || nodeID != "node-1" {
		t.Fatalf("expected fresh route to remain on node-1, got node=%q ok=%v", nodeID, ok)
	}
}

func TestPickSessionForNodeAndCapabilityReturnsNoWorkerCapacityWhenFull(t *testing.T) {
	svc := NewRegistryService(registrytest.NewStore(t), nil, 5, 15, 60*time.Second)
	hello := &registryv1.ConnectHello{
		NodeId: "node-1",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: "echo", MaxInflight: 1},
		},
	}
	session := newActiveSession("node-1", "session-1", hello)
	svc.swapSession(session)

	if !session.tryAcquireCapability("echo") {
		t.Fatalf("expected first capability acquire to succeed")
	}
	defer session.releaseCapability("echo")

	if _, err := svc.pickSessionForNodeAndCapability("node-1", "echo"); !errors.Is(err, ErrNoWorkerCapacity) {
		t.Fatalf("expected ErrNoWorkerCapacity, got %v", err)
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
						CommandId:       dispatch.GetCommandId(),
						PayloadJson:     dispatch.GetPayloadJson(),
						CompletedUnixMs: time.Now().UnixMilli(),
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
		_, dispatchErr := svc.dispatchCommand(ctx, "echo", buildEchoPayload("cleanup"), 2*time.Second, dispatchOptions{})
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
	hello := &registryv1.ConnectHello{
		NodeId:       workerID,
		WorkerSecret: secret,
	}
	for _, capability := range capabilities {
		hello.Capabilities = append(hello.Capabilities, &registryv1.CapabilityDeclaration{Name: capability})
	}
	return connectWorkerWithHello(client, hello)
}

func connectWorkerWithHello(
	client registryv1.WorkerRegistryServiceClient,
	hello *registryv1.ConnectHello,
) (grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse], string, error) {
	stream, err := client.Connect(context.Background())
	if err != nil {
		return nil, "", err
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
	hasTerminalExec := false
	for _, capability := range hello.GetCapabilities() {
		if normalizeCapability(capability.GetName()) == taskCapabilityTerminalExec {
			hasTerminalExec = true
			break
		}
	}
	if hasTerminalExec {
		results := make([]*registryv1.TerminalSessionRecoveryResult, 0, len(ack.GetTerminalSessionRecoveryCandidates()))
		for _, candidate := range ack.GetTerminalSessionRecoveryCandidates() {
			results = append(results, &registryv1.TerminalSessionRecoveryResult{
				SessionId: candidate.GetSessionId(),
				Status:    registryv1.TerminalSessionRecoveryResult_RECOVERED,
			})
		}
		if err := stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_TerminalSessionRecoveryReport{
				TerminalSessionRecoveryReport: &registryv1.TerminalSessionRecoveryReport{Results: results},
			},
		}); err != nil {
			return nil, "", err
		}
		recoveryAck, err := stream.Recv()
		if err != nil {
			return nil, "", err
		}
		if recoveryAck.GetTerminalSessionRecoveryAck() == nil {
			return nil, "", fmt.Errorf("expected terminal_session_recovery_ack, got %#v", recoveryAck.GetPayload())
		}
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
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     dispatch.GetPayloadJson(),
					CompletedUnixMs: time.Now().UnixMilli(),
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
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     dispatch.GetPayloadJson(),
					CompletedUnixMs: time.Now().UnixMilli(),
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

func terminalExecResponder(stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse], workerMarker string) {
	sessions := make(map[string]string)
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil || dispatch.GetCapability() != taskCapabilityTerminalExec {
			continue
		}

		payload := terminalExecScopedPayload{}
		if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "invalid terminal payload")
			continue
		}
		sessionID := strings.TrimSpace(payload.SessionID)
		if sessionID == "" {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "session_id is required")
			continue
		}

		stdout, exists := sessions[sessionID]
		created := false
		if !exists {
			if !payload.CreateIfMissing {
				sendTerminalExecError(stream, dispatch.GetCommandId(), "session_not_found", "session not found")
				continue
			}
			stdout = "Linux " + workerMarker
			sessions[sessionID] = stdout
			created = true
		}

		resultPayload, err := marshalTerminalExecSuccessPayload(sessionID, created, stdout)
		if err != nil {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "failed to encode result")
			continue
		}

		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     resultPayload,
					CompletedUnixMs: time.Now().UnixMilli(),
				},
			},
		})
	}
}

func computerUseResponder(stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse], marker string) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil {
			continue
		}
		if normalizeCapability(dispatch.GetCapability()) != computerUseCapabilityName {
			continue
		}
		payloadJSON, _ := json.Marshal(map[string]any{
			"stdout":                marker,
			"stderr":                "",
			"exit_code":             0,
			"stdout_truncated":      false,
			"stderr_truncated":      false,
			"lease_expires_unix_ms": time.Now().Add(time.Minute).UnixMilli(),
		})
		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     payloadJSON,
					CompletedUnixMs: time.Now().UnixMilli(),
				},
			},
		})
	}
}

func readImageResponder(stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse], marker string) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil {
			continue
		}
		if normalizeCapability(dispatch.GetCapability()) != readImageCapabilityName {
			continue
		}

		payload := terminalResourceScopedPayload{}
		if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "invalid readImage payload")
			continue
		}
		if strings.TrimSpace(payload.SessionID) != "computerUse" {
			sendTerminalExecError(stream, dispatch.GetCommandId(), terminalSessionNotFoundCode, "session not found")
			continue
		}
		filePath := strings.TrimSpace(payload.FilePath)
		if filePath == "" {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "session_id and file_path are required")
			continue
		}
		action := strings.TrimSpace(strings.ToLower(payload.Action))
		if action == "" {
			action = "validate"
		}
		if action != "validate" && action != "read" {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "action must be validate or read")
			continue
		}

		result := map[string]any{
			"session_id": "computerUse",
			"file_path":  filePath,
			"mime_type":  "image/png",
			"size_bytes": len(marker),
			"worker":     marker,
		}
		if action == "read" {
			result["blob"] = []byte(marker)
		}
		payloadJSON, _ := json.Marshal(result)
		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     payloadJSON,
					CompletedUnixMs: time.Now().UnixMilli(),
				},
			},
		})
	}
}

func delayedTerminalExecResponder(
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	workerMarker string,
	delay time.Duration,
) {
	sessions := make(map[string]string)
	for {
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		dispatch := resp.GetCommandDispatch()
		if dispatch == nil || dispatch.GetCapability() != taskCapabilityTerminalExec {
			continue
		}

		payload := terminalExecScopedPayload{}
		if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "invalid terminal payload")
			continue
		}
		sessionID := strings.TrimSpace(payload.SessionID)
		if sessionID == "" {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "session_id is required")
			continue
		}

		stdout, exists := sessions[sessionID]
		created := false
		if !exists {
			if !payload.CreateIfMissing {
				sendTerminalExecError(stream, dispatch.GetCommandId(), "session_not_found", "session not found")
				continue
			}
			stdout = "Linux " + workerMarker
			sessions[sessionID] = stdout
			created = true
		}

		if delay > 0 {
			time.Sleep(delay)
		}

		resultPayload, err := marshalTerminalExecSuccessPayload(sessionID, created, stdout)
		if err != nil {
			sendTerminalExecError(stream, dispatch.GetCommandId(), "invalid_payload", "failed to encode result")
			continue
		}

		_ = stream.Send(&registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId:       dispatch.GetCommandId(),
					PayloadJson:     resultPayload,
					CompletedUnixMs: time.Now().UnixMilli(),
				},
			},
		})
	}
}

func marshalTerminalExecSuccessPayload(sessionID string, created bool, stdout string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"session_id":            sessionID,
		"created":               created,
		"stdout":                stdout,
		"stderr":                "",
		"exit_code":             0,
		"stdout_truncated":      false,
		"stderr_truncated":      false,
		"lease_expires_unix_ms": time.Now().Add(time.Minute).UnixMilli(),
	})
}

func sendTerminalExecError(
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	commandID string,
	code string,
	message string,
) {
	_ = stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId: commandID,
				Error: &registryv1.CommandError{
					Code:    code,
					Message: message,
				},
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	})
}
