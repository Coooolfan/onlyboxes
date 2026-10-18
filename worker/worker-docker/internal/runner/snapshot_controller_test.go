package runner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func testSnapshotController(t *testing.T) (*snapshotController, *snapshotJournal) {
	t.Helper()
	manager := newTerminalSessionManager(terminalSessionManagerConfig{})
	t.Cleanup(manager.Close)
	journal := testSnapshotJournal(t)
	controller, err := newSnapshotController(context.Background(), manager, journal, &dockerSnapshotRuntime{version: "29.4.0", platform: "linux/arm64"}, snapshotControllerConfig{MaxInflight: 2, TmpQuotaBytes: 1 << 30, CacheMaxBytes: 1 << 20, CacheTTL: time.Hour, DrainTimeout: time.Second, TransferConcurrency: 2, TransferIdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close(context.Background()) })
	return controller, journal
}

func TestSnapshotControllerRejectsInvalidControlAndPoisonsClosedAdmission(t *testing.T) {
	controller, _ := testSnapshotController(t)
	for _, payload := range []string{`{"unknown_private_url":"https://secret.invalid"}`, `{} {}`, `"invalid"`} {
		report := controller.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: "operation", ScopedSessionId: "obx:owner:session", Generation: 1, Action: registryv1.SandboxPersistenceControl_INSPECT, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: []byte(payload)})
		if report.Error == nil || strings.Contains(report.Error.Message, "secret.invalid") {
			t.Fatal("invalid metadata was accepted or exposed", report.Error)
		}
	}
	if err := controller.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	report := controller.Handle(&registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: "operation", ScopedSessionId: "obx:owner:session", Generation: 1, Action: registryv1.SandboxPersistenceControl_INSPECT, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(), PayloadJson: []byte(`{}`)})
	if report.Error == nil || report.Error.Code != "persistence_unavailable" {
		t.Fatal("closed controller admitted work")
	}
}

func TestSnapshotCancelTombstonePreventsLatePreparationAndSurvivesReopen(t *testing.T) {
	journal := testSnapshotJournal(t)
	if err := journal.CancelUnknown("unknown", "obx:owner:session", 2); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSnapshotJournal(journal.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	op := testSnapshotOperation()
	op.ID = "unknown"
	op.Generation = 2
	if err := reopened.Claim(op); err == nil {
		t.Fatal("canceled operation was resurrected")
	}
	state, _ := reopened.Snapshot()
	if !state.Operations["unknown"].terminal() {
		t.Fatal("cancel tombstone lost")
	}
}

func TestSnapshotCacheEvictionProtectsUncommittedSourcesAndPins(t *testing.T) {
	controller, journal := testSnapshotController(t)
	create := func(id, scope string, committed bool) snapshotOperation {
		op := testSnapshotOperation()
		op.ID = id
		op.SessionID = scope
		op.Artifact = snapshotArtifactName(scope, id, 1)
		if err := journal.Claim(op); err != nil {
			t.Fatal(err)
		}
		archive := transferTestArchive([]byte("snapshot"))
		archive.SnapshotID = id
		if err := journal.Update(id, func(o *snapshotOperation) error {
			o.Phase = "frozen"
			o.Archive = &archive
			o.Verified = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if committed {
			if err := journal.Update(id, func(o *snapshotOperation) error { o.Phase = "committed"; o.Committed = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := journal.Update(id, func(o *snapshotOperation) error {
				o.Phase = "finalized"
				o.CacheUntilUnixMS = time.Now().Add(time.Hour).UnixMilli()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(controller.artifact(op.Artifact), []byte("snapshot"), 0600); err != nil {
			t.Fatal(err)
		}
		return op
	}
	cached := create("cached", "obx:owner:cached", true)
	pending := create("pending", "obx:owner:pending", false)
	controller.config.CacheMaxBytes = 0
	_, release, err := controller.openArtifact(cached.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.evictCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(controller.artifact(cached.Artifact)); err != nil {
		t.Fatal("pinned cache was removed")
	}
	release()
	if err := controller.evictCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(controller.artifact(cached.Artifact)); !os.IsNotExist(err) {
		t.Fatal("disabled cache retained an unused archive")
	}
	if _, err := os.Stat(controller.artifact(pending.Artifact)); err != nil {
		t.Fatal("cache eviction deleted an uncommitted source")
	}
}

func TestSnapshotControllerLeaseRenewalSurvivesUnrelatedJournalUpdates(t *testing.T) {
	controller, journal := testSnapshotController(t)
	op := testSnapshotOperation()
	op.Kind = "adopt"
	op.Phase = "active"
	op.StagingBytes = 0
	if err := journal.Claim(op); err != nil {
		t.Fatal(err)
	}
	if err := controller.manager.InstallSnapshotJournal(journal); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Hour)
	if err := controller.extendLease(op.SessionID, 1, until); err != nil {
		t.Fatal(err)
	}
	state, _ := journal.Snapshot()
	if state.Operations[op.ID].LeaseExpiresUnixMS != until.UnixMilli() {
		t.Fatal("confirmed lease extension was not durable")
	}
	if err := controller.extendLease(op.SessionID, 2, until.Add(time.Hour)); err == nil {
		t.Fatal("stale generation extended lease")
	}

}

func TestSnapshotLateCancelCannotRemoveActivatedTarget(t *testing.T) {
	controller, journal := testSnapshotController(t)
	op := testSnapshotOperation()
	op.Kind = "target"
	op.Generation = 2
	op.Phase = "prepared"
	op.ContainerID = ""
	op.LeaseTTLSec = 60
	archive := transferTestArchive([]byte("snapshot"))
	op.Archive = &archive
	if err := journal.Claim(op); err != nil {
		t.Fatal(err)
	}
	if err := journal.Update(op.ID, func(o *snapshotOperation) error {
		o.Phase = "importing"
		o.ContainerID = strings.Repeat("b", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"ready", "active"} {
		if err := journal.Update(op.ID, func(o *snapshotOperation) error { o.Phase = phase; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	state, _ := journal.Snapshot()
	op = state.Operations[op.ID]
	result, err := controller.cancel(context.Background(), op, wire.Control{})
	if err != nil || result.State != "active" {
		t.Fatalf("late cancellation removed activated target: %s %v", result.State, err)
	}
}

func TestSnapshotAdoptionProbeSurvivesRestartAndRejectsReplacement(t *testing.T) {
	journal := testSnapshotJournal(t)
	scope := "obx:owner:probe"
	id := strings.Repeat("b", 64)
	if err := journal.Probe(scope, id); err != nil {
		t.Fatal(err)
	}
	if err := journal.Probe(scope, strings.Repeat("c", 64)); err == nil {
		t.Fatal("adoption probe switched to another same-name instance")
	}
	_ = journal.Close()
	reopened, err := openSnapshotJournal(journal.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	manager := newTerminalSessionManager(terminalSessionManagerConfig{})
	defer manager.Close()
	if err := manager.InstallSnapshotJournal(reopened); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.claimSession(scope, time.Now().Add(time.Minute), true); err == nil {
		t.Fatal("late legacy create replaced the inspected persistent source")
	}
}
