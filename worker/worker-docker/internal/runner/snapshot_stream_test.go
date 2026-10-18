package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type snapshotStreamRegistry struct {
	registryv1.UnimplementedWorkerRegistryServiceServer
	connect func(grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error
}

func (s *snapshotStreamRegistry) Connect(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
	return s.connect(stream)
}

func TestSnapshotStreamHeartbeatAndReconnectDuringBlockedUpload(t *testing.T) {
	c, j := testSnapshotController(t)
	transfer, err := newSnapshotTransfer(2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.transfer = transfer
	op := testSnapshotOperation()
	op.ID = "operation"
	op.Artifact = snapshotArtifactName(op.SessionID, op.ID, op.Generation)
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	data := []byte("durable-source-content")
	archive := transferTestArchive(data)
	archive.SnapshotID = op.ID
	if err := j.Update(op.ID, func(o *snapshotOperation) error {
		o.Phase = "frozen"
		o.Archive = &archive
		o.Verified = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.artifact(op.Artifact), data, 0600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	release := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		close(stopped)
	}))
	defer httpServer.Close()
	defer close(release)
	old := workerSnapshots.Swap(c)
	oldRecovery := recoverPersistenceSessionsFn
	recoverPersistenceSessionsFn = c.Recover
	defer func() { workerSnapshots.Store(old); recoverPersistenceSessionsFn = oldRecovery }()
	var connections atomic.Int32
	svc := &snapshotStreamRegistry{}
	svc.connect = func(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
		attempt := connections.Add(1)
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		if !req.GetHello().GetSandboxPersistence().GetEnabled() {
			return status.Error(codes.InvalidArgument, "persistence capability absent")
		}
		id := fmt.Sprintf("connection-%d", attempt)
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_ConnectAck{ConnectAck: &registryv1.ConnectAck{SessionId: id, HeartbeatIntervalSec: 1}}}); err != nil {
			return err
		}
		req, err = stream.Recv()
		if err != nil {
			return err
		}
		if req.GetTerminalSessionRecoveryReport() == nil {
			return status.Error(codes.InvalidArgument, "recovery report absent")
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_TerminalSessionRecoveryAck{TerminalSessionRecoveryAck: &registryv1.TerminalSessionRecoveryAck{}}}); err != nil {
			return err
		}
		control := workerTestControl(registryv1.SandboxPersistenceControl_INSPECT)
		control.OperationId = "reconnect-probe"
		if attempt == 1 {
			control.OperationId = op.ID
			control.Generation = 1
			control.Action = registryv1.SandboxPersistenceControl_UPLOAD_SNAPSHOT
			control.PayloadJson, _ = json.Marshal(wire.Control{Parts: []wire.UploadPart{{Number: 1, Grant: wire.TransferGrant{URL: httpServer.URL, Method: http.MethodPut, ExpiresAt: time.Now().Add(time.Minute), Size: int64(len(data))}}}})
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_SandboxPersistenceControl{SandboxPersistenceControl: control}}); err != nil {
			return err
		}
		for {
			req, err = stream.Recv()
			if err != nil {
				return err
			}
			if heartbeat := req.GetHeartbeat(); heartbeat != nil {
				if heartbeat.SandboxPersistence == nil || heartbeat.SandboxPersistence.Inflight != 1 {
					return status.Error(codes.InvalidArgument, "heartbeat lost held-operation usage")
				}
				if attempt == 1 {
					select {
					case <-started:
						return status.Error(codes.Unavailable, "simulate connection loss during transfer")
					default:
					}
				}
				if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_HeartbeatAck{HeartbeatAck: &registryv1.HeartbeatAck{}}}); err != nil {
					return err
				}
			} else if report := req.GetSandboxPersistenceReport(); report != nil {
				if attempt == 1 {
					return status.Error(codes.InvalidArgument, "upload unexpectedly finished before disconnect")
				}
				var result wire.Result
				if report.Error != nil || json.Unmarshal(report.PayloadJson, &result) != nil || result.State != "missing" {
					return status.Error(codes.InvalidArgument, "reconnected control failed")
				}
				return status.Error(codes.FailedPrecondition, "verified reconnect")
			}
		}
	}
	server := grpc.NewServer()
	registryv1.RegisterWorkerRegistryServiceServer(server, svc)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer server.Stop()
	go server.Serve(listener)
	cfg := testConfig()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.CallTimeout = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := runSession(ctx, cfg); status.Code(err) != codes.Unavailable {
		t.Fatalf("blocked upload prevented heartbeat/disconnect: %v", err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("disconnect did not cancel signed transfer")
	}
	if err := runSession(ctx, cfg); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reconnected stream failed: %v", err)
	}
	state, err := j.Snapshot()
	if err != nil || state.Operations[op.ID].Phase != "frozen" {
		t.Fatal("disconnect lost source ownership", err)
	}
}
