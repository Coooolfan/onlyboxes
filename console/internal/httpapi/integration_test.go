package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/pkg/registryauth"
	"github.com/onlyboxes/onlyboxes/console/internal/grpcserver"
	"github.com/onlyboxes/onlyboxes/console/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRegisterAndListLifecycle(t *testing.T) {
	store := registry.NewStore()
	const workerID = "node-int-1"
	const workerSecret = "secret-int-1"

	registrySvc := grpcserver.NewRegistryService(
		store,
		map[string]string{workerID: workerSecret},
		5,
		15,
		60*time.Second,
	)
	grpcSrv := grpcserver.NewServer(registrySvc)
	grpcListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open grpc listener: %v", err)
	}
	defer grpcListener.Close()
	go func() {
		_ = grpcSrv.Serve(grpcListener)
	}()
	defer grpcSrv.Stop()

	handler := NewWorkerHandler(store, 15*time.Second)
	router := NewRouter(handler)
	httpSrv := httptest.NewServer(router)
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(grpcListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial grpc: %v", err)
	}
	defer conn.Close()

	client := registryv1.NewWorkerRegistryServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}

	hello := &registryv1.ConnectHello{
		NodeId:       workerID,
		NodeName:     "integration-node",
		ExecutorKind: "docker",
		Languages: []*registryv1.LanguageCapability{{
			Language: "python",
			Version:  "3.12",
		}},
		Version:         "v0.1.0",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "hello-nonce",
	}
	hello.Signature = registryauth.Sign(hello.GetNodeId(), hello.GetTimestampUnixMs(), hello.GetNonce(), workerSecret)
	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		t.Fatalf("send hello failed: %v", err)
	}

	connectResp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv connect ack failed: %v", err)
	}
	connectAck := connectResp.GetConnectAck()
	if connectAck == nil || connectAck.GetSessionId() == "" {
		t.Fatalf("expected connect_ack with session_id, got %#v", connectResp.GetPayload())
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Heartbeat{
			Heartbeat: &registryv1.HeartbeatFrame{
				NodeId:       workerID,
				SessionId:    connectAck.GetSessionId(),
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

	listOnline := requestList(t, httpSrv.URL+"/api/v1/workers?status=online")
	if listOnline.Total != 1 || len(listOnline.Items) != 1 {
		t.Fatalf("expected one online worker after register, got total=%d len=%d", listOnline.Total, len(listOnline.Items))
	}
	if listOnline.Items[0].NodeID != workerID || listOnline.Items[0].Status != registry.StatusOnline {
		t.Fatalf("unexpected online worker payload")
	}

	handler.nowFn = func() time.Time {
		return time.Now().Add(16 * time.Second)
	}
	listOffline := requestList(t, httpSrv.URL+"/api/v1/workers?status=offline")
	if listOffline.Total != 1 || len(listOffline.Items) != 1 {
		t.Fatalf("expected one offline worker after heartbeat timeout, got total=%d len=%d", listOffline.Total, len(listOffline.Items))
	}
	if listOffline.Items[0].NodeID != workerID || listOffline.Items[0].Status != registry.StatusOffline {
		t.Fatalf("unexpected offline worker payload")
	}
}

func requestList(t *testing.T, url string) listWorkersResponse {
	t.Helper()

	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("failed to call list API: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", res.StatusCode)
	}

	var payload listWorkersResponse
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	return payload
}
