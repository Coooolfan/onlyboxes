package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runtimelease"
	"google.golang.org/grpc"
)

func snapshotControllerCall(t *testing.T, c *snapshotController, scope, operation string, generation int64, action registryv1.SandboxPersistenceControl_Action, payload wire.Control) wire.Result {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	report := c.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: operation, ScopedSessionId: scope, Generation: generation, Action: action, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: encoded})
	if report.Error != nil {
		t.Fatalf("action %s: %s", action, report.Error.Code)
	}
	var result wire.Result
	if json.Unmarshal(report.PayloadJson, &result) != nil {
		t.Fatal("invalid result")
	}
	return result
}

func TestIntegrationSnapshotControllerTransfersColdRestoreAndCache(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime, err := newDockerSnapshotRuntime(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	cfg := terminalSessionManagerConfig{WorkerID: "worker-a", DockerImage: image, LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 300}
	manager := newTerminalSessionManager(cfg)
	defer manager.Close()
	journal := testSnapshotJournal(t)
	controllerCfg := snapshotControllerConfig{MaxInflight: 2, TmpQuotaBytes: 2 << 30, CacheMaxBytes: 512 << 20, CacheTTL: time.Hour, DrainTimeout: time.Second, TransferConcurrency: 2, TransferIdleTimeout: 5 * time.Second}
	controllerCfg.LeaseGuard = snapshotNativeLeaseFixture(t, ctx, image, runtime.platform, "worker-a")
	controller, err := newSnapshotController(ctx, manager, journal, runtime, controllerCfg)
	if err != nil {
		t.Fatal(err)
	}
	scope := "obx:owner:controller-" + uuid.NewString()
	name := terminalSessionResourceName(scope)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", name)
		state, _ := journal.Snapshot()
		for _, op := range state.Operations {
			if op.ImageID != "" {
				_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", op.ImageID)
			}
		}
	}()
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, CreateIfMissing: true, Command: "printf controller-data > /root/onlyboxes-controller.txt"}); err != nil {
		t.Fatal(err)
	}
	described := snapshotControllerCall(t, controller, scope, uuid.NewString(), 1, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{})
	op := uuid.NewString()
	frozen := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT, wire.Control{Spec: described.Spec, MaxBytes: 256 << 20, MaxExpandedBytes: 1 << 30, DrainTimeoutSec: 1})
	if frozen.State != "frozen" || frozen.Archive == nil {
		t.Fatal("source not frozen")
	}
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 1, Command: "must not execute"}); err == nil {
		t.Fatal("snapshot source admitted commands")
	}
	storeFile, err := os.Create(filepath.Join(t.TempDir(), "shared-archive"))
	if err != nil {
		t.Fatal(err)
	}
	defer storeFile.Close()
	var completed atomic.Bool
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			offset, e := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
			if e != nil {
				w.WriteHeader(400)
				return
			}
			data, e := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
			if e != nil || len(data) > 8<<20 {
				w.WriteHeader(400)
				return
			}
			if _, e = storeFile.WriteAt(data, offset); e != nil {
				w.WriteHeader(500)
				return
			}
			hash := sha256.Sum256(data)
			w.Header().Set("ETag", hex.EncodeToString(hash[:]))
			return
		}
		if r.Method == http.MethodGet && completed.Load() {
			gets.Add(1)
			http.ServeContent(w, r, "archive", time.Unix(1, 0), io.NewSectionReader(storeFile, 0, frozen.Archive.Size))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	const partBytes int64 = 8 << 20
	for offset, number := int64(0), int32(1); offset < frozen.Archive.Size; {
		var parts []wire.UploadPart
		for len(parts) < 4 && offset < frozen.Archive.Size {
			size := min(partBytes, frozen.Archive.Size-offset)
			parts = append(parts, wire.UploadPart{Number: number, Grant: wire.TransferGrant{URL: server.URL + "/?private-signature=fixture-grant-secret-do-not-journal&offset=" + strconv.FormatInt(offset, 10), Method: http.MethodPut, ExpiresAt: time.Now().Add(time.Minute), Offset: offset, Size: size}})
			offset += size
			number++
		}
		result := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_UPLOAD_SNAPSHOT, wire.Control{Parts: parts})
		if len(result.Parts) != len(parts) {
			t.Fatal("missing uploaded part acknowledgements")
		}
	}
	if err := storeFile.Sync(); err != nil {
		t.Fatal(err)
	}
	completed.Store(true)
	grant := wire.TransferGrant{URL: server.URL + "/archive?private-signature=fixture-grant-secret-do-not-journal", Method: http.MethodGet, ExpiresAt: time.Now().Add(time.Minute), Size: frozen.Archive.Size}
	verified := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_VERIFY_SNAPSHOT, wire.Control{Archive: frozen.Archive, Download: &grant})
	if verified.State != "verified" {
		t.Fatal("remote content was not verified")
	}
	// No commit flag may authorize source removal.
	body, _ := json.Marshal(wire.Control{})
	denied := controller.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: op, ScopedSessionId: scope, Generation: 1, Action: registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: body})
	if denied.Error == nil {
		t.Fatal("uncommitted source was removed")
	}
	finalized := snapshotControllerCall(t, controller, scope, op, 1, registryv1.SandboxPersistenceControl_FINALIZE_SUSPEND, wire.Control{SnapshotCommitted: true})
	if finalized.State != "finalized" {
		t.Fatal("source cleanup incomplete")
	}
	if id, err := snapshotContainerID(ctx, scope); err != nil || id != "" {
		t.Fatal("source survived committed cleanup", err)
	}
	wake := uuid.NewString()
	snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_PREPARE_RESTORE, wire.Control{Spec: frozen.Spec, Archive: frozen.Archive, LeaseTTLSec: 300})
	before := gets.Load()
	ready := snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_RESTORE, wire.Control{Archive: frozen.Archive, Download: &grant})
	if ready.State != "ready" || ready.InstanceID == described.InstanceID {
		t.Fatal("restore did not create a new ready instance")
	}
	if gets.Load() != before {
		t.Fatal("verified same-node cache did not avoid archive download")
	}
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 2, Command: "must not execute"}); err == nil {
		t.Fatal("unactivated target admitted commands")
	}
	snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_ACTIVATE, wire.Control{})
	result, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 2, Command: "cat /root/onlyboxes-controller.txt"})
	if err != nil || result.Stdout != "controller-data" || result.Created {
		t.Fatalf("restored data = %q, %v", result.Stdout, err)
	}
	state, _ := journal.Snapshot()
	encodedState, _ := json.Marshal(state)
	if strings.Contains(string(encodedState), "fixture-grant-secret-do-not-journal") {
		t.Fatal("journal retained an ephemeral transfer grant")
	}
	// A different controller/journal has no local cache. Restore the same
	// immutable bytes into a different stable session to exercise actual GET.
	coldConfig := cfg
	coldConfig.WorkerID = "worker-b"
	coldManager := newTerminalSessionManager(coldConfig)
	defer coldManager.Close()
	coldRoot := t.TempDir()
	if err := os.Chmod(coldRoot, 0700); err != nil {
		t.Fatal(err)
	}
	coldJournal, err := openSnapshotJournal(coldRoot, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	defer coldJournal.Close()
	coldControllerConfig := controllerCfg
	coldControllerConfig.LeaseGuard = snapshotNativeLeaseFixture(t, ctx, image, runtime.platform, "worker-b")
	cold, err := newSnapshotController(ctx, coldManager, coldJournal, runtime, coldControllerConfig)
	if err != nil {
		t.Fatal(err)
	}
	snapshotControllerCall(t, controller, scope, wake, 2, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{Delete: true})
	coldScope := scope
	coldName := terminalSessionResourceName(coldScope)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", coldName)
		state, _ := coldJournal.Snapshot()
		for _, op := range state.Operations {
			if op.ImageID != "" {
				_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "image", "rm", op.ImageID)
			}
		}
	}()
	coldOp := uuid.NewString()
	snapshotControllerCall(t, cold, coldScope, coldOp, 3, registryv1.SandboxPersistenceControl_PREPARE_RESTORE, wire.Control{Spec: frozen.Spec, Archive: frozen.Archive, LeaseTTLSec: 300})
	before = gets.Load()
	snapshotControllerCall(t, cold, coldScope, coldOp, 3, registryv1.SandboxPersistenceControl_RESTORE, wire.Control{Archive: frozen.Archive, Download: &grant})
	if gets.Load() != before+1 {
		t.Fatal("cold controller skipped archive GET")
	}
	// A ready target has a running backend but is still fenced. Its startup
	// lease must trigger cleanup even though ordinary held-session timers stop.
	if err := coldJournal.Update(coldOp, func(o *snapshotOperation) error {
		o.LeaseExpiresUnixMS = time.Now().Add(-time.Second).UnixMilli()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	maintenance, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); cold.maintain(maintenance) }()
	defer func() { stopMaintenance(); <-maintenanceDone; _ = cold.Close(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := coldJournal.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if current.Operations[coldOp].Phase == "canceled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired unactivated target retained its running backend")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if id, err := snapshotContainerID(ctx, coldScope); err != nil || id != "" {
		t.Fatal("canceled target can still execute", err)
	}
}

func TestIntegrationSnapshotWorkerReopensFrozenAndRunningOwnership(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runtime, err := newDockerSnapshotRuntime(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	cfg := terminalSessionManagerConfig{WorkerID: "worker-a", PersistenceEnabled: true, PreserveOnClose: true, DockerImage: image, LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 300}
	ccfg := snapshotControllerConfig{MaxInflight: 2, TmpQuotaBytes: 2 << 30, CacheMaxBytes: 512 << 20, CacheTTL: time.Hour, DrainTimeout: time.Second, TransferConcurrency: 2, TransferIdleTimeout: 5 * time.Second}
	ccfg.LeaseGuard = snapshotNativeLeaseFixture(t, ctx, image, runtime.platform, "worker-a")
	journal := testSnapshotJournal(t)
	root := journal.root
	manager := newTerminalSessionManager(cfg)
	controller, err := newSnapshotController(ctx, manager, journal, runtime, ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close(context.Background()); manager.Close(); _ = journal.Close() }()
	scope := "obx:owner:recovery-" + uuid.NewString()
	name := terminalSessionResourceName(scope)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", name)
	}()
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, CreateIfMissing: true, Command: "printf restart-data > /root/persistent-recovery.txt"}); err != nil {
		t.Fatal(err)
	}
	described := snapshotControllerCall(t, controller, scope, uuid.NewString(), 1, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{})
	// Shutdown can interrupt preparation before the cgroup is paused. Retain
	// that source rootfs while stopping its still-running background writers.
	preparingID := uuid.NewString()
	preparing := snapshotOperation{ID: preparingID, SessionID: scope, Generation: 1, Kind: "source", Phase: "preparing", Spec: *described.Spec, ContainerID: described.InstanceID, LeaseExpiresUnixMS: described.LeaseExpiresUnixMS, StagingBytes: 1 << 20, Artifact: snapshotArtifactName(scope, preparingID, 1)}
	if err := journal.Claim(preparing); err != nil {
		t.Fatal(err)
	}
	if err := manager.InstallSnapshotJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := controller.stopForShutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var stopped struct{ Running bool }
	if err := dockerSnapshotJSON(ctx, &stopped, "inspect", "--format", "{{json .State}}", described.InstanceID); err != nil || stopped.Running {
		t.Fatal("shutdown left an unfinished source writing or removed its rootfs", err)
	}
	snapshotControllerCall(t, controller, scope, preparingID, 1, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{Rollback: true, Spec: described.Spec})

	operation := uuid.NewString()
	frozen := snapshotControllerCall(t, controller, scope, operation, 1, registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT, wire.Control{Spec: described.Spec, MaxBytes: 256 << 20, MaxExpandedBytes: 1 << 30, DrainTimeoutSec: 1})
	if frozen.State != "frozen" {
		t.Fatal("source was not frozen")
	}
	reopen := func() {
		t.Helper()
		if err := controller.Close(ctx); err != nil {
			t.Fatal(err)
		}
		manager.Close()
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		journal, err = openSnapshotJournal(root, "worker-a")
		if err != nil {
			t.Fatal(err)
		}
		manager = newTerminalSessionManager(cfg)
		controller, err = newSnapshotController(ctx, manager, journal, runtime, ccfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	hints := []*registryv1.SandboxPersistenceRecoveryCandidate{{ScopedSessionId: scope, OperationId: operation, Generation: 1, Role: registryv1.SandboxPersistenceRecoveryCandidate_SOURCE}}
	if err := controller.Recover(ctx, nil, hints); err != nil {
		t.Fatal(err)
	}
	manager.Recover(ctx, nil)
	var state struct {
		Paused  bool
		Running bool
	}
	if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", name); err != nil || !state.Paused {
		t.Fatal("reconnect thawed or removed a held source", err)
	}
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 1, Command: "exit 0"}); err == nil {
		t.Fatal("held source accepted execution")
	}
	rolled := snapshotControllerCall(t, controller, scope, operation, 1, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{Rollback: true, Spec: described.Spec})
	if rolled.State != "running" {
		t.Fatal("rollback did not restore the original instance")
	}
	if err := controller.stopForShutdown(ctx); err != nil {
		t.Fatal(err)
	}
	reopen()
	candidates := []*registryv1.TerminalSessionRecoveryCandidate{{SessionId: scope, Generation: 1, LeaseExpiresUnixMs: rolled.LeaseExpiresUnixMS}}
	if err := controller.Recover(ctx, candidates, nil); err != nil {
		t.Fatal(err)
	}
	results := manager.Recover(ctx, candidates)
	if len(results) != 1 || results[0].Status != registryv1.TerminalSessionRecoveryResult_RECOVERED {
		t.Fatal("durable active ownership could not recover", results)
	}
	result, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 1, Command: "cat /root/persistent-recovery.txt"})
	if err != nil || result.Stdout != "restart-data" {
		t.Fatal("running recovery lost source data", err)
	}
	durable, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	active := durable.Operations[operation]
	// A stale maintenance observation must not stop a subsequently renewed lease.
	stale := active
	stale.LeaseExpiresUnixMS = time.Now().Add(-time.Hour).UnixMilli()
	controller.expireActive(ctx, stale)
	if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", active.ContainerID); err != nil || !state.Running {
		t.Fatal("stale lease observation stopped a renewed session", err)
	}
	// With no local session after restart, the durable lease is still enforced;
	// expiry stops the native backend while retaining its writable rootfs.
	reopen()
	if err := journal.Update(operation, func(o *snapshotOperation) error {
		o.LeaseExpiresUnixMS = time.Now().Add(-time.Second).UnixMilli()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	maintenance, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); controller.maintain(maintenance) }()
	defer func() { stop(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := dockerSnapshotJSON(ctx, &state, "inspect", "--format", "{{json .State}}", active.ContainerID); err != nil {
			t.Fatal("lease maintenance removed the sole writable rootfs", err)
		}
		if !state.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("durable expired lease did not stop the backend")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestIntegrationSnapshotWorkerRunAdvertisesAndDispatchesLifecycle(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	control := workerTestControl(registryv1.SandboxPersistenceControl_CANCEL)
	control.OperationId = uuid.NewString()
	control.ScopedSessionId = "obx:owner:run-" + uuid.NewString()
	verified := make(chan error, 1)
	svc := &snapshotStreamRegistry{connect: func(stream grpc.BidiStreamingServer[registryv1.ConnectRequest, registryv1.ConnectResponse]) error {
		fail := func(err error) error { verified <- err; return err }
		req, err := stream.Recv()
		if err != nil {
			return fail(err)
		}
		caps := req.GetHello().GetSandboxPersistence()
		if !caps.GetEnabled() || caps.GetRuntime() != "docker" || caps.GetRuntimeVersion() == "" || caps.GetGuestPlatform() == "" || caps.GetMaxInflight() != 1 || caps.GetAvailableStagingBytes() <= 0 {
			return fail(fmt.Errorf("incomplete persistence capabilities: %v", caps))
		}
		controller := workerSnapshots.Load()
		if controller == nil {
			return fail(fmt.Errorf("hello preceded lifecycle installation"))
		}
		guard, ok := controller.config.LeaseGuard.(*dockerSnapshotLeaseClient)
		if !ok {
			return fail(fmt.Errorf("hello preceded independent lease installation"))
		}
		state, err := controller.journal.Snapshot()
		if err != nil || state.LeaseGuard == nil || state.LeaseGuard.HelperID != guard.helperID {
			return fail(fmt.Errorf("hello preceded durable helper ownership"))
		}
		var ready runtimelease.Identity
		if guard.rpc(stream.Context(), runtimelease.Request{Action: "ping"}, &ready) != nil || !ready.Ready || ready.WorkerID != req.GetHello().GetNodeId() || ready.EngineID != state.LeaseGuard.EngineID || ready.StateID != state.LeaseGuard.StateID {
			return fail(fmt.Errorf("hello preceded guardian identity confirmation"))
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_ConnectAck{ConnectAck: &registryv1.ConnectAck{SessionId: "run-connection", HeartbeatIntervalSec: 1}}}); err != nil {
			return fail(err)
		}
		req, err = stream.Recv()
		if err != nil {
			return fail(err)
		}
		if req.GetTerminalSessionRecoveryReport() == nil {
			return fail(fmt.Errorf("ordinary recovery was not completed"))
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_TerminalSessionRecoveryAck{TerminalSessionRecoveryAck: &registryv1.TerminalSessionRecoveryAck{}}}); err != nil {
			return fail(err)
		}
		if err := stream.Send(&registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_SandboxPersistenceControl{SandboxPersistenceControl: control}}); err != nil {
			return fail(err)
		}
		for {
			req, err = stream.Recv()
			if err != nil {
				return fail(err)
			}
			if report := req.GetSandboxPersistenceReport(); report != nil {
				var result wire.Result
				if report.Error != nil || json.Unmarshal(report.PayloadJson, &result) != nil || result.State != "canceled" || report.OperationId != control.OperationId {
					return fail(fmt.Errorf("invalid lifecycle report: %v", report))
				}
				verified <- nil
				<-stream.Context().Done()
				return stream.Context().Err()
			}
		}
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
	cfg.WorkerID = "snapshot-run-" + uuid.NewString()
	cfg.ConsoleGRPCTarget = listener.Addr().String()
	cfg.Snapshot = config.SnapshotConfig{LeaseGuardImage: snapshotGuardImageFixture(t, ctx, "linux/"+snapshotTestDaemonArch(t, ctx)), LeaseGuardDockerSocket: "/var/run/docker.sock", Enabled: true, TmpDir: root, HelperImage: image, MaxInflight: 1, TmpQuotaBytes: 1 << 30, CacheMaxBytes: 0, CacheTTL: time.Hour, TransferConcurrency: 2, TransferIdleTimeout: time.Second, DrainTimeout: time.Second}
	snapshotCleanupLaunchedGuard(t, root)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case err := <-verified:
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
	case err := <-done:
		t.Fatal("worker startup failed", err)
	case <-ctx.Done():
		<-done
		t.Fatal("worker did not complete lifecycle dispatch")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("worker shutdown failed", err)
	}
	journal, err := openSnapshotJournal(root, cfg.WorkerID)
	if err != nil {
		t.Fatal("shutdown did not release journal lock", err)
	}
	defer journal.Close()
	state, err := journal.Snapshot()
	if err != nil || state.Operations[control.OperationId].Phase != "canceled" {
		t.Fatal("RPC acknowledgement preceded durable operation state", err)
	}
}

func TestIntegrationSnapshotWorkerMissingJournalRetainsOwnedResources(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workerID := "snapshot-missing-" + uuid.NewString()
	var output bytesBuffer
	if err := dockerSnapshotCLI(ctx, nil, &output, "create", "--label", "onlyboxes.worker_id="+workerID, "--label", "onlyboxes.persistence=true", image, "sleep", "60"); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(output.String())
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = dockerSnapshotCLI(cleanup, nil, io.Discard, "rm", "-f", id)
	}()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.WorkerID = workerID
	cfg.Snapshot = config.SnapshotConfig{LeaseGuardImage: "sha256:" + strings.Repeat("a", 64), LeaseGuardDockerSocket: "/var/run/docker.sock", Enabled: true, TmpDir: root, HelperImage: image, MaxInflight: 1, TmpQuotaBytes: 1 << 30, CacheMaxBytes: 0, CacheTTL: time.Hour, TransferConcurrency: 2, TransferIdleTimeout: time.Second, DrainTimeout: time.Second}
	if err := Run(ctx, cfg); err == nil || !strings.Contains(err.Error(), "operation journal is missing") {
		t.Fatal("missing journal admitted unsafe orphan recovery", err)
	}
	output.Reset()
	if err := dockerSnapshotCLI(ctx, nil, &output, "inspect", "--format", "{{.Id}}", id); err != nil || strings.TrimSpace(output.String()) != id {
		t.Fatal("missing-journal failure removed owned writable data", err)
	}
	if _, err := os.Stat(filepath.Join(root, "journal.json")); !os.IsNotExist(err) {
		t.Fatal("missing ownership was silently replaced by an empty journal")
	}
}
