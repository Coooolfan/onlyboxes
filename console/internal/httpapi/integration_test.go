package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

	handler := NewWorkerHandler(store, 15*time.Second, registrySvc, registrySvc, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
	httpSrv := httptest.NewServer(router)
	defer httpSrv.Close()
	dashboardClient := newAuthenticatedClient(t, httpSrv)

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
		Capabilities: []*registryv1.CapabilityDeclaration{{
			Name: "echo",
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

	listOnline := requestList(t, dashboardClient, httpSrv.URL+"/api/v1/workers?status=online")
	if listOnline.Total != 1 || len(listOnline.Items) != 1 {
		t.Fatalf("expected one online worker after register, got total=%d len=%d", listOnline.Total, len(listOnline.Items))
	}
	if listOnline.Items[0].NodeID != workerID || listOnline.Items[0].Status != registry.StatusOnline {
		t.Fatalf("unexpected online worker payload")
	}

	handler.nowFn = func() time.Time {
		return time.Now().Add(16 * time.Second)
	}
	listOffline := requestList(t, dashboardClient, httpSrv.URL+"/api/v1/workers?status=offline")
	if listOffline.Total != 1 || len(listOffline.Items) != 1 {
		t.Fatalf("expected one offline worker after heartbeat timeout, got total=%d len=%d", listOffline.Total, len(listOffline.Items))
	}
	if listOffline.Items[0].NodeID != workerID || listOffline.Items[0].Status != registry.StatusOffline {
		t.Fatalf("unexpected offline worker payload")
	}
}

func TestLegacyTokenHeaderIsRejected(t *testing.T) {
	handler := NewWorkerHandler(registry.NewStore(), 15*time.Second, nil, nil, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/commands/echo", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Onlyboxes-MCP-Token", testMCPToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for legacy token header, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEchoCommandLifecycle(t *testing.T) {
	store := registry.NewStore()
	const workerID = "node-echo-1"
	const workerSecret = "secret-echo-1"

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

	handler := NewWorkerHandler(store, 15*time.Second, registrySvc, registrySvc, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
		NodeId:          workerID,
		NodeName:        "echo-worker",
		ExecutorKind:    "docker",
		Capabilities:    []*registryv1.CapabilityDeclaration{{Name: "echo"}},
		Version:         "v0.1.0",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "hello-nonce-echo",
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
	if connectResp.GetConnectAck() == nil {
		t.Fatalf("expected connect_ack, got %#v", connectResp.GetPayload())
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
						Echo: &registryv1.EchoResult{
							Message: dispatch.GetEcho().GetMessage(),
						},
					},
				},
			})
		}
	}()

	res, err := postJSONWithMCPToken(httpSrv.URL+"/api/v1/commands/echo", `{"message":"hello echo"}`)
	if err != nil {
		t.Fatalf("failed to call echo API: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", res.StatusCode)
	}

	var payload echoCommandResponse
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("failed to decode echo response: %v", err)
	}
	if payload.Message != "hello echo" {
		t.Fatalf("expected echoed message, got %q", payload.Message)
	}
}

func TestTaskLifecycleSync(t *testing.T) {
	store := registry.NewStore()
	const workerID = "node-task-1"
	const workerSecret = "secret-task-1"

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

	handler := NewWorkerHandler(store, 15*time.Second, registrySvc, registrySvc, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
		NodeId:          workerID,
		NodeName:        "task-worker",
		ExecutorKind:    "docker",
		Capabilities:    []*registryv1.CapabilityDeclaration{{Name: "echo", MaxInflight: 4}},
		Version:         "v0.1.0",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "hello-nonce-task",
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
	if connectResp.GetConnectAck() == nil {
		t.Fatalf("expected connect_ack, got %#v", connectResp.GetPayload())
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
						CommandId:       dispatch.GetCommandId(),
						PayloadJson:     dispatch.GetPayloadJson(),
						CompletedUnixMs: time.Now().UnixMilli(),
					},
				},
			})
		}
	}()

	res, err := postJSONWithMCPToken(
		httpSrv.URL+"/api/v1/tasks",
		`{"capability":"echo","input":{"message":"hello task"},"mode":"sync","wait_ms":1000,"timeout_ms":5000}`,
	)
	if err != nil {
		t.Fatalf("failed to call task API: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", res.StatusCode)
	}

	var payload taskResponse
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("failed to decode task response: %v", err)
	}
	if payload.Status != string(grpcserver.TaskStatusSucceeded) {
		t.Fatalf("expected succeeded status, got %s", payload.Status)
	}
	if !strings.Contains(string(payload.Result), `"hello task"`) {
		t.Fatalf("expected echoed result payload, got %s", string(payload.Result))
	}
}

func TestMCPLifecycle(t *testing.T) {
	store := registry.NewStore()
	const workerID = "node-mcp-1"
	const workerSecret = "secret-mcp-1"

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

	handler := NewWorkerHandler(store, 15*time.Second, registrySvc, registrySvc, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
		NodeName:     "mcp-worker",
		ExecutorKind: "docker",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: "echo", MaxInflight: 4},
			{Name: "pythonExec", MaxInflight: 4},
		},
		Version:         "v0.1.0",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "hello-nonce-mcp",
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
	if connectResp.GetConnectAck() == nil {
		t.Fatalf("expected connect_ack, got %#v", connectResp.GetPayload())
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

			capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
			switch capability {
			case "echo":
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId:       dispatch.GetCommandId(),
							PayloadJson:     dispatch.GetPayloadJson(),
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			case "pythonexec":
				pythonPayload := struct {
					Code string `json:"code"`
				}{}
				if err := json.Unmarshal(dispatch.GetPayloadJson(), &pythonPayload); err != nil {
					return
				}
				resultPayload, err := json.Marshal(struct {
					Output   string `json:"output"`
					Stderr   string `json:"stderr"`
					ExitCode int    `json:"exit_code"`
				}{
					Output:   "ran:" + pythonPayload.Code,
					Stderr:   "",
					ExitCode: 7,
				})
				if err != nil {
					return
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
			default:
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId: dispatch.GetCommandId(),
							Error: &registryv1.CommandError{
								Code:    "unsupported_capability",
								Message: "unsupported capability",
							},
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			}
		}
	}()

	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-integration-client",
		Version: "v0.1.0",
	}, nil)
	_, err = mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL + "/mcp",
		DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Fatalf("expected MCP connect without token to fail")
	}

	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL + "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient:           newMCPTokenHTTPClient(testMCPToken),
	}, nil)
	if err != nil {
		t.Fatalf("failed to connect MCP client: %v", err)
	}
	defer session.Close()

	echoResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello mcp"},
	})
	if err != nil {
		t.Fatalf("mcp echo tools/call failed: %v", err)
	}
	if echoResult.IsError {
		t.Fatalf("expected mcp echo tool success, got error=%q", firstTextContent(echoResult))
	}
	echoStructured := structuredContentMap(t, echoResult.StructuredContent)
	if got := toString(t, echoStructured["message"]); got != "hello mcp" {
		t.Fatalf("expected echo message hello mcp, got %q", got)
	}

	pythonResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "pythonExec",
		Arguments: map[string]any{"code": "print('ok')"},
	})
	if err != nil {
		t.Fatalf("mcp pythonExec tools/call failed: %v", err)
	}
	if pythonResult.IsError {
		t.Fatalf("expected mcp pythonExec tool success, got error=%q", firstTextContent(pythonResult))
	}
	pythonStructured := structuredContentMap(t, pythonResult.StructuredContent)
	if got := toString(t, pythonStructured["output"]); got != "ran:print('ok')" {
		t.Fatalf("unexpected pythonExec output: %q", got)
	}
	if got := toInt(t, pythonStructured["exit_code"]); got != 7 {
		t.Fatalf("expected exit_code=7, got %d", got)
	}
}

func TestTerminalLifecycle(t *testing.T) {
	store := registry.NewStore()
	const workerID = "node-terminal-1"
	const workerSecret = "secret-terminal-1"

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

	handler := NewWorkerHandler(store, 15*time.Second, registrySvc, registrySvc, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
		NodeName:     "terminal-worker",
		ExecutorKind: "docker",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: terminalExecCapabilityName, MaxInflight: 4},
			{Name: terminalResourceCapabilityName, MaxInflight: 4},
		},
		Version:         "v0.1.0",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "hello-nonce-terminal",
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
	if connectResp.GetConnectAck() == nil {
		t.Fatalf("expected connect_ack, got %#v", connectResp.GetPayload())
	}

	go func() {
		sessionContent := map[string]string{}
		sessionFiles := map[string]map[string][]byte{}
		sessionFileMIME := map[string]map[string]string{}
		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			dispatch := resp.GetCommandDispatch()
			if dispatch == nil {
				continue
			}

			capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
			switch capability {
			case "terminalexec":
				payload := terminalExecPayload{}
				if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecInvalidPayloadCode,
									Message: "invalid payload",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				sessionID := strings.TrimSpace(payload.SessionID)
				created := false
				if sessionID == "" {
					sessionID = "session-1"
					if _, ok := sessionContent[sessionID]; !ok {
						sessionContent[sessionID] = ""
						sessionFiles[sessionID] = map[string][]byte{}
						sessionFileMIME[sessionID] = map[string]string{}
						created = true
					}
				}

				if _, ok := sessionContent[sessionID]; !ok {
					if !payload.CreateIfMissing {
						_ = stream.Send(&registryv1.ConnectRequest{
							Payload: &registryv1.ConnectRequest_CommandResult{
								CommandResult: &registryv1.CommandResult{
									CommandId: dispatch.GetCommandId(),
									Error: &registryv1.CommandError{
										Code:    terminalExecSessionNotFoundCode,
										Message: "session not found",
									},
									CompletedUnixMs: time.Now().UnixMilli(),
								},
							},
						})
						continue
					}
					sessionContent[sessionID] = ""
					sessionFiles[sessionID] = map[string][]byte{}
					sessionFileMIME[sessionID] = map[string]string{}
					created = true
				}

				stdout := ""
				switch strings.TrimSpace(payload.Command) {
				case "write":
					sessionContent[sessionID] = "persisted"
					sessionFiles[sessionID]["/workspace/state.txt"] = []byte("persisted")
					sessionFileMIME[sessionID]["/workspace/state.txt"] = "text/plain"
					sessionFiles[sessionID]["/workspace/image.png"] = []byte{0x89, 0x50, 0x4e, 0x47}
					sessionFileMIME[sessionID]["/workspace/image.png"] = "image/png"
					sessionFiles[sessionID]["/workspace/fail-image.png"] = []byte{0x89, 0x50, 0x4e, 0x47}
					sessionFileMIME[sessionID]["/workspace/fail-image.png"] = "image/png"
					sessionFiles[sessionID]["/workspace/sound.wav"] = []byte{0x52, 0x49, 0x46, 0x46}
					sessionFileMIME[sessionID]["/workspace/sound.wav"] = "audio/wav"
				case "read":
					stdout = sessionContent[sessionID]
				}

				resultJSON, _ := json.Marshal(terminalCommandResponse{
					SessionID:          sessionID,
					Created:            created,
					Stdout:             stdout,
					Stderr:             "",
					ExitCode:           0,
					StdoutTruncated:    false,
					StderrTruncated:    false,
					LeaseExpiresUnixMS: time.Now().Add(60 * time.Second).UnixMilli(),
				})
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId:       dispatch.GetCommandId(),
							PayloadJson:     resultJSON,
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			case "terminalresource":
				payload := mcpTerminalResourcePayload{}
				if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecInvalidPayloadCode,
									Message: "invalid payload",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				sessionID := strings.TrimSpace(payload.SessionID)
				filePath := strings.TrimSpace(payload.FilePath)
				if sessionID == "" || filePath == "" {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecInvalidPayloadCode,
									Message: "session_id and file_path are required",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}
				files, ok := sessionFiles[sessionID]
				if !ok {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecSessionNotFoundCode,
									Message: "session not found",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}
				if filePath == "/workspace/dir" {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    "path_is_directory",
									Message: "path is directory",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}
				content, exists := files[filePath]
				if !exists {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    "file_not_found",
									Message: "file not found",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				action := strings.TrimSpace(strings.ToLower(payload.Action))
				if action == "" {
					action = "validate"
				}
				if action == "read" && filePath == "/workspace/fail-image.png" {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    "file_too_large",
									Message: "file too large",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				result := mcpTerminalResourceResult{
					SessionID: sessionID,
					FilePath:  filePath,
					MIMEType:  "application/octet-stream",
					SizeBytes: int64(len(content)),
				}
				if mimes, ok := sessionFileMIME[sessionID]; ok {
					if mimeType := strings.TrimSpace(mimes[filePath]); mimeType != "" {
						result.MIMEType = mimeType
					}
				}
				if action == "read" {
					result.Blob = append([]byte(nil), content...)
				}
				resultJSON, _ := json.Marshal(result)
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId:       dispatch.GetCommandId(),
							PayloadJson:     resultJSON,
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			default:
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId: dispatch.GetCommandId(),
							Error: &registryv1.CommandError{
								Code:    "unsupported_capability",
								Message: "unsupported capability",
							},
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			}
		}
	}()

	writeRes, err := postJSONWithMCPToken(
		httpSrv.URL+"/api/v1/commands/terminal",
		`{"command":"write"}`,
	)
	if err != nil {
		t.Fatalf("failed to call terminal write API: %v", err)
	}
	defer writeRes.Body.Close()
	if writeRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", writeRes.StatusCode)
	}
	writePayload := terminalCommandResponse{}
	if err := json.NewDecoder(writeRes.Body).Decode(&writePayload); err != nil {
		t.Fatalf("failed to decode write response: %v", err)
	}
	if strings.TrimSpace(writePayload.SessionID) == "" {
		t.Fatalf("expected session_id in write response")
	}

	readReqBody := `{"command":"read","session_id":"` + writePayload.SessionID + `"}`
	readRes, err := postJSONWithMCPToken(httpSrv.URL+"/api/v1/commands/terminal", readReqBody)
	if err != nil {
		t.Fatalf("failed to call terminal read API: %v", err)
	}
	defer readRes.Body.Close()
	if readRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", readRes.StatusCode)
	}
	readPayload := terminalCommandResponse{}
	if err := json.NewDecoder(readRes.Body).Decode(&readPayload); err != nil {
		t.Fatalf("failed to decode read response: %v", err)
	}
	if readPayload.Stdout != "persisted" {
		t.Fatalf("expected persisted terminal stdout, got %q", readPayload.Stdout)
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-terminal-client",
		Version: "v0.1.0",
	}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL + "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient:           newMCPTokenHTTPClient(testMCPToken),
	}, nil)
	if err != nil {
		t.Fatalf("failed to connect MCP client: %v", err)
	}
	defer session.Close()

	toolResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "terminalExec",
		Arguments: map[string]any{
			"command":    "read",
			"session_id": writePayload.SessionID,
		},
	})
	if err != nil {
		t.Fatalf("mcp terminalExec tools/call failed: %v", err)
	}
	if toolResult.IsError {
		t.Fatalf("expected mcp terminalExec success, got error=%q", firstTextContent(toolResult))
	}
	terminalStructured := structuredContentMap(t, toolResult.StructuredContent)
	if got := toString(t, terminalStructured["stdout"]); got != "persisted" {
		t.Fatalf("unexpected terminalExec output: %q", got)
	}

	textResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "readImage",
		Arguments: map[string]any{
			"session_id": writePayload.SessionID,
			"file_path":  "/workspace/state.txt",
		},
	})
	if err != nil {
		t.Fatalf("mcp readImage tools/call failed: %v", err)
	}
	if textResult.IsError {
		t.Fatalf("expected readImage success, got error=%q", firstTextContent(textResult))
	}
	if textResult.StructuredContent != nil {
		t.Fatalf("expected no structured content for readImage")
	}
	if len(textResult.Content) != 1 {
		t.Fatalf("expected one text content for text mime, got %d", len(textResult.Content))
	}
	textContent, ok := textResult.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content for non-image mime, got %T", textResult.Content[0])
	}
	if !strings.Contains(textContent.Text, "unsupported mime type: text/plain; expected image/*") {
		t.Fatalf("unexpected text mime fallback message: %q", textContent.Text)
	}

	imageResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "readImage",
		Arguments: map[string]any{
			"session_id": writePayload.SessionID,
			"file_path":  "/workspace/image.png",
		},
	})
	if err != nil {
		t.Fatalf("mcp readImage(image) tools/call failed: %v", err)
	}
	if imageResult.IsError {
		t.Fatalf("expected image read success, got error=%q", firstTextContent(imageResult))
	}
	if imageResult.StructuredContent != nil {
		t.Fatalf("expected no structured content for readImage")
	}
	if len(imageResult.Content) != 1 {
		t.Fatalf("expected [image] content, got %d", len(imageResult.Content))
	}
	imageContent, ok := imageResult.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("expected image content, got %T", imageResult.Content[0])
	}
	if imageContent.MIMEType != "image/png" {
		t.Fatalf("expected image MIME image/png, got %q", imageContent.MIMEType)
	}
	if len(imageContent.Data) == 0 {
		t.Fatalf("expected non-empty image bytes")
	}

	audioResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "readImage",
		Arguments: map[string]any{
			"session_id": writePayload.SessionID,
			"file_path":  "/workspace/sound.wav",
		},
	})
	if err != nil {
		t.Fatalf("mcp readImage(audio) tools/call failed: %v", err)
	}
	if audioResult.IsError {
		t.Fatalf("expected audio fallback success, got error=%q", firstTextContent(audioResult))
	}
	if audioResult.StructuredContent != nil {
		t.Fatalf("expected no structured content for readImage")
	}
	if len(audioResult.Content) != 1 {
		t.Fatalf("expected one text content for audio mime, got %d", len(audioResult.Content))
	}
	audioTextContent, ok := audioResult.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content for audio fallback, got %T", audioResult.Content[0])
	}
	if !strings.Contains(audioTextContent.Text, "unsupported mime type: audio/wav; expected image/*") {
		t.Fatalf("unexpected audio mime fallback message: %q", audioTextContent.Text)
	}

	failImagePath := "/workspace/fail-image.png"
	failImageResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "readImage",
		Arguments: map[string]any{
			"session_id": writePayload.SessionID,
			"file_path":  failImagePath,
		},
	})
	if err != nil {
		t.Fatalf("mcp readImage(fail-image) tools/call failed: %v", err)
	}
	if !failImageResult.IsError {
		t.Fatalf("expected fail-image read to be tool error")
	}
	if got := firstTextContent(failImageResult); !strings.Contains(got, "file_too_large") {
		t.Fatalf("expected file_too_large error text, got %q", got)
	}
}

func TestTokenIsolationLifecycle(t *testing.T) {
	store := registry.NewStore()
	const workerID = "node-token-isolation-1"
	const workerSecret = "secret-token-isolation-1"

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

	handler := NewWorkerHandler(store, 15*time.Second, registrySvc, registrySvc, ":50051")
	router := NewRouter(handler, newTestConsoleAuth(t), newTestMCPAuth())
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
		NodeName:     "token-isolation-worker",
		ExecutorKind: "docker",
		Capabilities: []*registryv1.CapabilityDeclaration{
			{Name: "echo", MaxInflight: 4},
			{Name: terminalExecCapabilityName, MaxInflight: 4},
			{Name: terminalResourceCapabilityName, MaxInflight: 4},
		},
		Version:         "v0.1.0",
		TimestampUnixMs: time.Now().UnixMilli(),
		Nonce:           "hello-nonce-token-isolation",
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
	if connectResp.GetConnectAck() == nil {
		t.Fatalf("expected connect_ack, got %#v", connectResp.GetPayload())
	}

	go func() {
		sessionContent := map[string]string{}
		sessionFiles := map[string]map[string][]byte{}
		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			dispatch := resp.GetCommandDispatch()
			if dispatch == nil {
				continue
			}

			capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
			switch capability {
			case "echo":
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId:       dispatch.GetCommandId(),
							PayloadJson:     dispatch.GetPayloadJson(),
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			case "terminalexec":
				payload := terminalExecPayload{}
				if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecInvalidPayloadCode,
									Message: "invalid payload",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				sessionID := strings.TrimSpace(payload.SessionID)
				if sessionID == "" {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecInvalidPayloadCode,
									Message: "session_id is required",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				if _, ok := sessionContent[sessionID]; !ok {
					if !payload.CreateIfMissing {
						_ = stream.Send(&registryv1.ConnectRequest{
							Payload: &registryv1.ConnectRequest_CommandResult{
								CommandResult: &registryv1.CommandResult{
									CommandId: dispatch.GetCommandId(),
									Error: &registryv1.CommandError{
										Code:    terminalExecSessionNotFoundCode,
										Message: "session not found",
									},
									CompletedUnixMs: time.Now().UnixMilli(),
								},
							},
						})
						continue
					}
					sessionContent[sessionID] = ""
					sessionFiles[sessionID] = map[string][]byte{}
				}

				stdout := ""
				switch strings.TrimSpace(payload.Command) {
				case "write":
					sessionContent[sessionID] = "persisted"
					sessionFiles[sessionID]["/workspace/state.txt"] = []byte("persisted")
				case "read":
					stdout = sessionContent[sessionID]
				}

				resultJSON, _ := json.Marshal(terminalCommandResponse{
					SessionID:          sessionID,
					Created:            false,
					Stdout:             stdout,
					Stderr:             "",
					ExitCode:           0,
					StdoutTruncated:    false,
					StderrTruncated:    false,
					LeaseExpiresUnixMS: time.Now().Add(60 * time.Second).UnixMilli(),
				})
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId:       dispatch.GetCommandId(),
							PayloadJson:     resultJSON,
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			case "terminalresource":
				payload := mcpTerminalResourcePayload{}
				if err := json.Unmarshal(dispatch.GetPayloadJson(), &payload); err != nil {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecInvalidPayloadCode,
									Message: "invalid payload",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				sessionID := strings.TrimSpace(payload.SessionID)
				filePath := strings.TrimSpace(payload.FilePath)
				files, ok := sessionFiles[sessionID]
				if !ok {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    terminalExecSessionNotFoundCode,
									Message: "session not found",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}
				content, ok := files[filePath]
				if !ok {
					_ = stream.Send(&registryv1.ConnectRequest{
						Payload: &registryv1.ConnectRequest_CommandResult{
							CommandResult: &registryv1.CommandResult{
								CommandId: dispatch.GetCommandId(),
								Error: &registryv1.CommandError{
									Code:    "file_not_found",
									Message: "file not found",
								},
								CompletedUnixMs: time.Now().UnixMilli(),
							},
						},
					})
					continue
				}

				result := mcpTerminalResourceResult{
					SessionID: sessionID,
					FilePath:  filePath,
					MIMEType:  "text/plain",
					SizeBytes: int64(len(content)),
				}
				action := strings.TrimSpace(strings.ToLower(payload.Action))
				if action == "read" {
					result.Blob = append([]byte(nil), content...)
				}
				resultJSON, _ := json.Marshal(result)
				_ = stream.Send(&registryv1.ConnectRequest{
					Payload: &registryv1.ConnectRequest_CommandResult{
						CommandResult: &registryv1.CommandResult{
							CommandId:       dispatch.GetCommandId(),
							PayloadJson:     resultJSON,
							CompletedUnixMs: time.Now().UnixMilli(),
						},
					},
				})
			}
		}
	}()

	taskBody := `{"capability":"echo","input":{"message":"hello token"},"mode":"async","request_id":"req-shared"}`
	taskARes, err := postJSONWithToken(httpSrv.URL+"/api/v1/tasks", taskBody, testMCPToken)
	if err != nil {
		t.Fatalf("submit task token A failed: %v", err)
	}
	defer taskARes.Body.Close()
	if taskARes.StatusCode == http.StatusConflict {
		t.Fatalf("expected token A submit to succeed, got 409 body=%s", readBody(t, taskARes))
	}
	taskA := taskResponse{}
	if err := json.NewDecoder(taskARes.Body).Decode(&taskA); err != nil {
		t.Fatalf("decode token A task response failed: %v", err)
	}
	if strings.TrimSpace(taskA.TaskID) == "" {
		t.Fatalf("expected task_id for token A")
	}

	taskBRes, err := postJSONWithToken(httpSrv.URL+"/api/v1/tasks", taskBody, testMCPTokenB)
	if err != nil {
		t.Fatalf("submit task token B failed: %v", err)
	}
	defer taskBRes.Body.Close()
	if taskBRes.StatusCode == http.StatusConflict {
		t.Fatalf("expected token B submit not to conflict with token A")
	}
	taskB := taskResponse{}
	if err := json.NewDecoder(taskBRes.Body).Decode(&taskB); err != nil {
		t.Fatalf("decode token B task response failed: %v", err)
	}
	if strings.TrimSpace(taskB.TaskID) == "" {
		t.Fatalf("expected task_id for token B")
	}
	if taskA.TaskID == taskB.TaskID {
		t.Fatalf("expected token A/B request_id dedup isolation, got same task_id=%q", taskA.TaskID)
	}

	getCrossReq, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/tasks/"+taskA.TaskID, nil)
	if err != nil {
		t.Fatalf("build cross-token get task request failed: %v", err)
	}
	getCrossReq.Header.Set(trustedTokenHeader, testMCPTokenB)
	getCrossRes, err := http.DefaultClient.Do(getCrossReq)
	if err != nil {
		t.Fatalf("cross-token get task failed: %v", err)
	}
	defer getCrossRes.Body.Close()
	if getCrossRes.StatusCode != http.StatusNotFound {
		t.Fatalf("expected cross-token get task 404, got %d", getCrossRes.StatusCode)
	}

	cancelCrossReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/v1/tasks/"+taskA.TaskID+"/cancel", nil)
	if err != nil {
		t.Fatalf("build cross-token cancel task request failed: %v", err)
	}
	cancelCrossReq.Header.Set(trustedTokenHeader, testMCPTokenB)
	cancelCrossRes, err := http.DefaultClient.Do(cancelCrossReq)
	if err != nil {
		t.Fatalf("cross-token cancel task failed: %v", err)
	}
	defer cancelCrossRes.Body.Close()
	if cancelCrossRes.StatusCode != http.StatusNotFound {
		t.Fatalf("expected cross-token cancel task 404, got %d", cancelCrossRes.StatusCode)
	}

	writeRes, err := postJSONWithToken(httpSrv.URL+"/api/v1/commands/terminal", `{"command":"write"}`, testMCPToken)
	if err != nil {
		t.Fatalf("terminal write token A failed: %v", err)
	}
	defer writeRes.Body.Close()
	if writeRes.StatusCode != http.StatusOK {
		t.Fatalf("expected terminal write 200, got %d", writeRes.StatusCode)
	}
	writePayload := terminalCommandResponse{}
	if err := json.NewDecoder(writeRes.Body).Decode(&writePayload); err != nil {
		t.Fatalf("decode terminal write response failed: %v", err)
	}
	if strings.TrimSpace(writePayload.SessionID) == "" {
		t.Fatalf("expected session_id from terminal write")
	}

	readBodyByA := `{"command":"read","session_id":"` + writePayload.SessionID + `"}`
	readByARes, err := postJSONWithToken(httpSrv.URL+"/api/v1/commands/terminal", readBodyByA, testMCPToken)
	if err != nil {
		t.Fatalf("terminal read token A failed: %v", err)
	}
	defer readByARes.Body.Close()
	if readByARes.StatusCode != http.StatusOK {
		t.Fatalf("expected token A terminal read 200, got %d", readByARes.StatusCode)
	}

	readBodyByB := `{"command":"read","session_id":"` + writePayload.SessionID + `"}`
	readByBRes, err := postJSONWithToken(httpSrv.URL+"/api/v1/commands/terminal", readBodyByB, testMCPTokenB)
	if err != nil {
		t.Fatalf("terminal read token B failed: %v", err)
	}
	defer readByBRes.Body.Close()
	if readByBRes.StatusCode != http.StatusNotFound {
		t.Fatalf("expected token B terminal read 404, got %d", readByBRes.StatusCode)
	}

	mcpClientA := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-token-a-client",
		Version: "v0.1.0",
	}, nil)
	mcpSessionA, err := mcpClientA.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL + "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient:           newMCPTokenHTTPClient(testMCPToken),
	}, nil)
	if err != nil {
		t.Fatalf("connect mcp token A failed: %v", err)
	}
	defer mcpSessionA.Close()

	readImageA, err := mcpSessionA.CallTool(ctx, &mcp.CallToolParams{
		Name: "readImage",
		Arguments: map[string]any{
			"session_id": writePayload.SessionID,
			"file_path":  "/workspace/state.txt",
		},
	})
	if err != nil {
		t.Fatalf("mcp readImage token A failed: %v", err)
	}
	if readImageA.IsError {
		t.Fatalf("expected mcp readImage token A success, got %q", firstTextContent(readImageA))
	}

	mcpClientB := mcp.NewClient(&mcp.Implementation{
		Name:    "mcp-token-b-client",
		Version: "v0.1.0",
	}, nil)
	mcpSessionB, err := mcpClientB.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL + "/mcp",
		DisableStandaloneSSE: true,
		HTTPClient:           newMCPTokenHTTPClient(testMCPTokenB),
	}, nil)
	if err != nil {
		t.Fatalf("connect mcp token B failed: %v", err)
	}
	defer mcpSessionB.Close()

	readImageB, err := mcpSessionB.CallTool(ctx, &mcp.CallToolParams{
		Name: "readImage",
		Arguments: map[string]any{
			"session_id": writePayload.SessionID,
			"file_path":  "/workspace/state.txt",
		},
	})
	if err != nil {
		t.Fatalf("mcp readImage token B failed: %v", err)
	}
	if !readImageB.IsError {
		t.Fatalf("expected mcp readImage token B tool error")
	}
	if got := firstTextContent(readImageB); !strings.Contains(got, "session_not_found") {
		t.Fatalf("expected session_not_found for token B, got %q", got)
	}
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	if res == nil || res.Body == nil {
		return ""
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return ""
	}
	return string(data)
}

func structuredContentMap(t *testing.T, value any) map[string]any {
	t.Helper()

	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	if raw, ok := value.(json.RawMessage); ok {
		decoded := map[string]any{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("failed to decode structured content: %v", err)
		}
		return decoded
	}
	t.Fatalf("unexpected structured content type %T", value)
	return nil
}

func firstTextContent(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		return ""
	}
	return text.Text
}

func toString(t *testing.T, value any) string {
	t.Helper()
	parsed, ok := value.(string)
	if !ok {
		t.Fatalf("expected string value, got %#v", value)
	}
	return parsed
}

func toInt(t *testing.T, value any) int {
	t.Helper()
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		t.Fatalf("expected numeric value, got %#v", value)
		return 0
	}
}

type mcpTokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t *mcpTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	if strings.TrimSpace(t.token) != "" {
		cloned.Header.Set(trustedTokenHeader, strings.TrimSpace(t.token))
	}
	return base.RoundTrip(cloned)
}

func newMCPTokenHTTPClient(token string) *http.Client {
	return &http.Client{
		Transport: &mcpTokenTransport{
			base:  http.DefaultTransport,
			token: token,
		},
	}
}

func postJSONWithMCPToken(url string, body string) (*http.Response, error) {
	return postJSONWithToken(url, body, testMCPToken)
}

func postJSONWithToken(url string, body string, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set(trustedTokenHeader, strings.TrimSpace(token))
	}
	return http.DefaultClient.Do(req)
}

func requestList(t *testing.T, client *http.Client, url string) listWorkersResponse {
	t.Helper()

	res, err := client.Get(url)
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

func newAuthenticatedClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("failed to create cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}
	reqBody, err := json.Marshal(loginRequest{
		Username: testDashboardUsername,
		Password: testDashboardPassword,
	})
	if err != nil {
		t.Fatalf("failed to marshal login request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/console/login", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("failed to build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to login: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected login 200, got %d", res.StatusCode)
	}
	return client
}
