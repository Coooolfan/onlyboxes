package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"google.golang.org/grpc"
)

// The owned child runs the actual Worker Run entry, not only a lease issuer.
// SIGKILL intentionally bypasses every Go shutdown hook and session timer.
func TestSnapshotActiveWorkerProcessChild(t *testing.T) {
	path := os.Getenv("ONLYBOXES_ACTIVE_WORKER_FIXTURE")
	if path == "" {
		t.Skip("owned subprocess only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(2)
	}
	var cfg config.Config
	if json.Unmarshal(data, &cfg) != nil {
		os.Exit(2)
	}
	if Run(context.Background(), cfg) != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestIntegrationSnapshotAdoptedWorkerSIGKILLRetainsRootAndEnforcesLease(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("pinned fixture image required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("private journal")
	}
	snapshotCleanupLaunchedGuard(t, root)
	scope := "obx:owner:worker-kill-" + uuid.NewString()
	name := terminalSessionResourceName(scope)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", name)
	})
	type observed struct {
		result wire.Result
		err    error
	}
	ready := make(chan observed, 1)
	svc := &snapshotStreamRegistry{connect: func(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
		fail := func(err error) error {
			select {
			case ready <- observed{err: err}:
			default:
			}
			return err
		}
		req, err := stream.Recv()
		if err != nil {
			return fail(err)
		}
		if !req.GetHello().GetSandboxPersistence().GetEnabled() {
			return fail(fmt.Errorf("persistence absent"))
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_ConnectAck{ConnectAck: &registryv1.ConnectAck{SessionId: "owned-process", HeartbeatIntervalSec: 1}}}); err != nil {
			return fail(err)
		}
		recv := func() (*registryv1.ConnectRequest, error) {
			for {
				message, err := stream.Recv()
				if err != nil {
					return nil, err
				}
				if message.GetHeartbeat() != nil {
					if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_HeartbeatAck{HeartbeatAck: &registryv1.HeartbeatAck{HeartbeatIntervalSec: 1}}}); err != nil {
						return nil, err
					}
					continue
				}
				return message, nil
			}
		}
		req, err = recv()
		if err != nil {
			return fail(err)
		}
		if req.GetTerminalSessionRecoveryReport() == nil {
			return fail(fmt.Errorf("recovery report absent"))
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_TerminalSessionRecoveryAck{TerminalSessionRecoveryAck: &registryv1.TerminalSessionRecoveryAck{}}}); err != nil {
			return fail(err)
		}
		payload, _ := json.Marshal(map[string]any{"session_id": scope, "create_if_missing": true, "lease_ttl_sec": 12, "command": "printf adoption-data > /root/adoption.txt; (while :; do printf . >> /root/background; sleep 0.1; done) >/dev/null 2>&1 &"})
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_CommandDispatch{CommandDispatch: &registryv1.CommandDispatch{CommandId: uuid.NewString(), Capability: "terminalExec", PayloadJson: payload, DeadlineUnixMs: time.Now().Add(20 * time.Second).UnixMilli()}}}); err != nil {
			return fail(err)
		}
		req, err = recv()
		if err != nil {
			return fail(err)
		}
		if req.GetCommandResult() == nil || req.GetCommandResult().GetError() != nil {
			return fail(fmt.Errorf("seed command failed"))
		}
		control := workerTestControl(registryv1.SandboxPersistenceControl_INSPECT)
		control.ScopedSessionId = scope
		control.OperationId = uuid.NewString()
		control.Generation = 1
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_SandboxPersistenceControl{SandboxPersistenceControl: control}}); err != nil {
			return fail(err)
		}
		req, err = recv()
		if err != nil {
			return fail(err)
		}
		report := req.GetSandboxPersistenceReport()
		if report == nil || report.Error != nil {
			return fail(fmt.Errorf("initial adoption guard failed"))
		}
		var result wire.Result
		if json.Unmarshal(report.PayloadJson, &result) != nil || result.InstanceID == "" {
			return fail(fmt.Errorf("adoption identity absent"))
		}
		// Ordinary generation-zero renewal after a successful probe must update
		// the durable probe and the same generation-one guard record.
		payload, _ = json.Marshal(map[string]any{"session_id": scope, "lease_ttl_sec": 12, "generation": 0})
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_CommandDispatch{CommandDispatch: &registryv1.CommandDispatch{CommandId: uuid.NewString(), Capability: "terminalLeaseRenew", PayloadJson: payload, DeadlineUnixMs: time.Now().Add(10 * time.Second).UnixMilli()}}}); err != nil {
			return fail(err)
		}
		req, err = recv()
		if err != nil {
			return fail(err)
		}
		command := req.GetCommandResult()
		if command == nil || command.Error != nil {
			return fail(fmt.Errorf("probed session renewal failed"))
		}
		var renewed struct {
			Lease int64 `json:"lease_expires_unix_ms"`
		}
		if json.Unmarshal(command.PayloadJson, &renewed) != nil || renewed.Lease <= result.LeaseExpiresUnixMS {
			return fail(fmt.Errorf("probe renewal did not advance"))
		}
		result.LeaseExpiresUnixMS = renewed.Lease
		ready <- observed{result: result}
		<-stream.Context().Done()
		return stream.Context().Err()
	}}
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
	cfg.WorkerID = "snapshot-process-" + uuid.NewString()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.TerminalExecDockerImage = image
	cfg.TerminalLeaseMinSec = 1
	cfg.TerminalLeaseMaxSec = 600
	cfg.TerminalLeaseDefaultSec = 12
	cfg.Snapshot = config.SnapshotConfig{Enabled: true, TmpDir: root, HelperImage: image, LeaseGuardImage: snapshotGuardImageFixture(t, ctx, "linux/"+snapshotTestDaemonArch(t, ctx)), LeaseGuardDockerSocket: "/var/run/docker.sock", MaxInflight: 1, TmpQuotaBytes: 1 << 30, CacheMaxBytes: 0, CacheTTL: time.Hour, TransferConcurrency: 1, TransferIdleTimeout: time.Second, DrainTimeout: time.Second}
	file := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(cfg)
	if os.WriteFile(file, data, 0600) != nil {
		t.Fatal("fixture configuration")
	}
	log, err := os.OpenFile(filepath.Join(t.TempDir(), "worker.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestSnapshotActiveWorkerProcessChild$")
	child.Env = append(os.Environ(), "ONLYBOXES_ACTIVE_WORKER_FIXTURE="+file)
	child.Stdout = log
	child.Stderr = log
	if child.Start() != nil {
		t.Fatal("start owned Worker")
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	var result wire.Result
	select {
	case got := <-ready:
		if got.err != nil {
			t.Fatal(got.err)
		}
		result = got.result
	case <-ctx.Done():
		t.Fatal("Worker startup/adoption timed out")
	}
	if result.LeaseExpiresUnixMS-time.Now().UnixMilli() < 2000 {
		t.Fatal("insufficient live lease for process-loss injection")
	}
	var before, after bytes.Buffer
	if err := dockerSnapshotCLI(ctx, nil, &before, "exec", result.InstanceID, "cat", "/root/background"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := dockerSnapshotCLI(ctx, nil, &after, "exec", result.InstanceID, "cat", "/root/background"); err != nil || after.Len() <= before.Len() {
		t.Fatal("background writer was not active before Worker loss", err)
	}
	data, err = os.ReadFile(filepath.Join(root, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state snapshotJournalState
	if json.Unmarshal(data, &state) != nil || state.Probes[scope] != result.InstanceID || state.ProbeLeases[scope] != result.LeaseExpiresUnixMS || state.LeaseGuard == nil {
		t.Fatal("probe acknowledgement preceded durable ownership")
	}
	if child.Process.Kill() != nil {
		t.Fatal("kill owned Worker")
	}
	_ = child.Wait()
	deadline := time.UnixMilli(result.LeaseExpiresUnixMS).Add(5 * time.Second)
	for {
		var native struct{ Running bool }
		if err := dockerSnapshotJSON(ctx, &native, "inspect", "--format", "{{json .State}}", result.InstanceID); err != nil {
			t.Fatal("root disappeared after Worker SIGKILL", err)
		}
		if !native.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live background writers escaped independent lease after Worker SIGKILL")
		}
		time.Sleep(100 * time.Millisecond)
	}
	output := filepath.Join(t.TempDir(), "data")
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "cp", result.InstanceID+":/root/adoption.txt", output); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(output)
	if err != nil || string(data) != "adoption-data" {
		t.Fatal("process loss discarded writable data")
	}
	var helper struct{ Running bool }
	if dockerSnapshotJSON(ctx, &helper, "inspect", "--format", "{{json .State}}", state.LeaseGuard.HelperID) != nil || !helper.Running {
		t.Fatal("lease enforcement depended on dead Worker process")
	}
	t.Log("actual Worker Run SIGKILL: initial adoption, ordinary renewal, background-writer expiry and retained rootfs verified against a registry fixture")
}
