package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func TestIntegrationSnapshotPurgeFencesNativeNameAndSurvivesRestart(t *testing.T) {
	if os.Getenv("DOCKER_INTEGRATION") != "1" {
		t.Skip("set DOCKER_INTEGRATION=1 and DOCKER_SNAPSHOT_HELPER_IMAGE")
	}
	image := os.Getenv("DOCKER_SNAPSHOT_HELPER_IMAGE")
	if image == "" {
		t.Fatal("DOCKER_SNAPSHOT_HELPER_IMAGE is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	backend, err := newDockerSnapshotRuntime(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	managerCfg := terminalSessionManagerConfig{WorkerID: "worker-a", PersistenceEnabled: true, DockerImage: image, LeaseMinSec: 1, LeaseMaxSec: 600, LeaseDefaultSec: 300, PreserveOnClose: true}
	manager := newTerminalSessionManager(managerCfg)
	journal := testSnapshotJournal(t)
	cfg := snapshotControllerConfig{MaxInflight: 2, TmpQuotaBytes: 2 << 30, CacheMaxBytes: 512 << 20, CacheTTL: time.Hour, DrainTimeout: time.Second, TransferConcurrency: 2, TransferIdleTimeout: time.Second}
	controller, err := newSnapshotController(ctx, manager, journal, backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close(context.Background()); manager.Close(); _ = journal.Close() }()
	scope := "obx:owner:purge-" + uuid.NewString()
	other := scope + "-other"
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, id := range []string{scope, other} {
			_ = dockerSnapshotCLI(clean, nil, io.Discard, "rm", "-f", terminalSessionResourceName(id))
		}
	}()
	for _, id := range []string{scope, other} {
		if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: id, CreateIfMissing: true, Command: "printf preserve-until-delete > /root/data"}); err != nil {
			t.Fatal(err)
		}
	}
	described := snapshotControllerCall(t, controller, scope, uuid.NewString(), 1, registryv1.SandboxPersistenceControl_INSPECT, wire.Control{})
	operation := uuid.NewString()
	snapshotControllerCall(t, controller, scope, operation, 1, registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT, wire.Control{Spec: described.Spec, MaxBytes: 256 << 20, MaxExpandedBytes: 1 << 30, DrainTimeoutSec: 1})

	// An exporter can die after producing bytes but before the final rename.
	orphan := controller.artifact(snapshotExportTempPrefix(operation) + "crashed.tar")
	unrelated := controller.artifact(snapshotExportTempPrefix("another-operation") + "keep.tar")
	if err := os.WriteFile(orphan, []byte("private-export-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("another-session"), 0600); err != nil {
		t.Fatal(err)
	}
	deletion := "delete-" + uuid.NewString()
	purged := snapshotControllerCall(t, controller, scope, deletion, 2, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{PurgeSession: true, Spec: described.Spec})
	if purged.State != "deleted" {
		t.Fatal("purge did not finish")
	}
	state, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	marker := state.Operations[deletion]
	if marker.Kind != "delete" || marker.Phase != "deleted" || marker.TombstoneID == "" || marker.TombstoneID == described.InstanceID {
		t.Fatal("deletion did not retain an independent native fence", marker)
	}
	var old bytesBuffer
	if err := dockerSnapshotCLI(ctx, nil, &old, "ps", "-a", "--no-trunc", "--filter", "id="+described.InstanceID, "--format", "{{.ID}}"); err != nil || old.Len() != 0 {
		t.Fatal("source writable layer survived purge", err)
	}
	if files, err := os.ReadDir(controller.artifact("")); err != nil || len(files) != 1 {
		t.Fatal("purge retained private archive bytes", err)
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "another-session" {
		t.Fatal("purge removed another operation temporary file", err)
	}
	var native dockerSnapshotContainer
	if err := dockerSnapshotJSON(ctx, &native, "inspect", "--format", "{{json .}}", marker.TombstoneID); err != nil || native.State.Running || !native.HostConfig.ReadonlyRootfs || !deletionMarker(native, scope, deletion, 2) {
		t.Fatal("invalid native deletion fence", err)
	}
	// A native create whose client was disconnected before cleanup must still
	// lose the name race after the deletion acknowledgement.
	if err := dockerSnapshotCLI(ctx, nil, io.Discard, "create", "--name", terminalSessionResourceName(scope), image, "sleep", "60"); err == nil {
		t.Fatal("late native creation bypassed the deletion fence")
	}
	if _, err := manager.Execute(ctx, terminalExecRequest{SessionID: scope, Generation: 1, CreateIfMissing: true, Command: "exit 0"}); err == nil {
		t.Fatal("deleted session accepted a stale command")
	}
	if result, err := manager.Execute(ctx, terminalExecRequest{SessionID: other, Command: "cat /root/data"}); err != nil || result.Stdout != "preserve-until-delete" {
		t.Fatal("purge touched another session", err)
	}

	// Workers sharing one daemon observe the same global deletion marker; an
	// old cache holder can acknowledge cleanup without owning the active root.
	secondRoot := t.TempDir()
	if err := os.Chmod(secondRoot, 0700); err != nil {
		t.Fatal(err)
	}
	secondJournal, err := openSnapshotJournal(secondRoot, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	defer secondJournal.Close()
	secondManager := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer secondManager.Close()
	second, err := newSnapshotController(ctx, secondManager, secondJournal, backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	snapshotControllerCall(t, second, scope, deletion, 2, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{PurgeSession: true, Spec: described.Spec})
	secondState, err := secondJournal.Snapshot()
	if err != nil || secondState.Operations[deletion].TombstoneID != marker.TombstoneID {
		t.Fatal("cache holder did not reuse the global native fence", err)
	}
	root := journal.root
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
	manager = newTerminalSessionManager(managerCfg)
	controller, err = newSnapshotController(ctx, manager, journal, backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Recover(ctx, nil, []*registryv1.SandboxPersistenceRecoveryCandidate{{ScopedSessionId: scope, OperationId: deletion, Generation: 2, Role: registryv1.SandboxPersistenceRecoveryCandidate_TARGET}}); err != nil {
		t.Fatal("deletion could not recover", err)
	}
	snapshotControllerCall(t, controller, scope, deletion, 2, registryv1.SandboxPersistenceControl_CANCEL, wire.Control{PurgeSession: true, Spec: described.Spec})
	stale := workerTestControl(registryv1.SandboxPersistenceControl_CANCEL)
	stale.ScopedSessionId = scope
	stale.OperationId = operation
	stale.Generation = 1
	body, _ := json.Marshal(wire.Control{Rollback: true, Spec: described.Spec})
	stale.PayloadJson = body
	if report := controller.Handle(stale); report.Error == nil {
		t.Fatal("late rollback resurrected a deleted source")
	}
	next := testSnapshotOperation()
	next.SessionID = scope
	next.ID = "future-restore"
	next.Generation = 3
	next.Kind = "target"
	next.Phase = "prepared"
	next.ContainerID = ""
	next.Spec = *described.Spec
	archive := transferTestArchive([]byte("snapshot"))
	archive.Format = next.Spec.Format
	next.Archive = &archive
	if err := journal.Claim(next); err == nil {
		t.Fatal("new generation reused a deleted stable identity")
	}
}
