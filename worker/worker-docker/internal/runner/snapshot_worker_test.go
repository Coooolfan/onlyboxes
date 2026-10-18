package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func workerTestControl(action registryv1.SandboxPersistenceControl_Action) *registryv1.SandboxPersistenceControl {
	return &registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: "operation", ScopedSessionId: "obx:owner:session", Generation: 2, Action: action, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: []byte(`{}`)}
}

func TestSnapshotAdmissionOrdersCancellationBeforeAsyncExecution(t *testing.T) {
	c, j := testSnapshotController(t)
	if err := c.beginConnection(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	c.recoveredConnection("first")
	// The receive loop admits PREPARE before its goroutine runs. A subsequent
	// CANCEL must see it and cancel that same action, rather than overtake it.
	work := c.admit("first", workerTestControl(registryv1.SandboxPersistenceControl_PREPARE_RESTORE))
	cancel := c.admit("first", workerTestControl(registryv1.SandboxPersistenceControl_CANCEL))()
	var result wire.Result
	if cancel.Error != nil || json.Unmarshal(cancel.PayloadJson, &result) != nil || result.State != "busy" {
		t.Fatalf("cancel overtook admitted preparation: %+v", cancel)
	}
	report := work()
	if report.Error != nil {
		t.Fatalf("canceled action should request retry: %+v", report)
	}
	state, err := j.Snapshot()
	if err != nil || len(state.Operations) != 0 {
		t.Fatal("canceled preparation wrote a durable claim", err)
	}
}

func TestSnapshotConnectionEpochRejectsLateFramesWithoutPoisoning(t *testing.T) {
	c, _ := testSnapshotController(t)
	if err := c.beginConnection(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	c.recoveredConnection("first")
	work := c.admit("first", workerTestControl(registryv1.SandboxPersistenceControl_INSPECT))
	c.endConnection("first")
	if report := work(); report.Error != nil {
		t.Fatal(report.Error)
	}
	if err := c.beginConnection(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	c.recoveredConnection("second")
	c.endConnection("first")
	report := c.admit("first", workerTestControl(registryv1.SandboxPersistenceControl_CANCEL))()
	if report.Error == nil || report.Error.Code != "session_state_conflict" {
		t.Fatal("old stream action accepted", report)
	}
	c.manager.mu.Lock()
	failed, disconnected := c.manager.persistenceFailed, c.manager.persistenceDisconnected
	c.manager.mu.Unlock()
	if failed || disconnected {
		t.Fatal("old stream poisoned a healthy new connection")
	}
	report = c.admit("second", workerTestControl(registryv1.SandboxPersistenceControl_INSPECT))()
	var result wire.Result
	if report.Error != nil || json.Unmarshal(report.PayloadJson, &result) != nil || result.State != "missing" {
		t.Fatal("new connection could not inspect", report)
	}
}

func TestSnapshotCloseWaitsForAdmittedAction(t *testing.T) {
	c, _ := testSnapshotController(t)
	work := c.admit("", workerTestControl(registryv1.SandboxPersistenceControl_INSPECT))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if c.Close(ctx) == nil {
		t.Fatal("close ignored admitted but unfinished work")
	}
	work()
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotStartupCannotDisableAnExistingJournal(t *testing.T) {
	_, j := testSnapshotController(t)
	cfg := testConfig()
	cfg.Snapshot.Enabled = false
	cfg.Snapshot.TmpDir = j.root
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "persistence must remain enabled") {
		t.Fatal("worker ignored existing operation ownership", err)
	}
}

func TestSnapshotRecoveryRejectsConcurrentLifecycleAdmission(t *testing.T) {
	c, _ := testSnapshotController(t)
	if err := c.beginConnection(context.Background(), "recovering"); err != nil {
		t.Fatal(err)
	}
	report := c.admit("recovering", workerTestControl(registryv1.SandboxPersistenceControl_CANCEL))()
	var result wire.Result
	if report.Error != nil || json.Unmarshal(report.PayloadJson, &result) != nil || result.State != "busy" {
		t.Fatal("lifecycle action raced ownership recovery", report)
	}
}
