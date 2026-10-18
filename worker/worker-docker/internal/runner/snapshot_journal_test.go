package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func testSnapshotOperation() snapshotOperation {
	return snapshotOperation{ID: "operation-1", SessionID: "obx:owner:session", Generation: 1, Kind: "source", Phase: "preparing", Spec: wire.RestoreSpec{Runtime: "docker", RuntimeVersion: "29.4.0", Platform: "linux/arm64", Format: wire.DockerRootFSFormat, Image: "runtime@sha256:" + strings.Repeat("a", 64), CPUs: .5, MemoryMiB: 256, MaxProcesses: 128, Env: map[string]string{"PRIVATE": "protected"}}, ContainerID: strings.Repeat("b", 64), LeaseExpiresUnixMS: time.Now().Add(time.Hour).UnixMilli(), StagingBytes: 1 << 20}
}

func testSnapshotJournal(t *testing.T) *snapshotJournal {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := openSnapshotJournal(root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func freezeTestSnapshot(t *testing.T, j *snapshotJournal, op snapshotOperation) {
	t.Helper()
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(op.ID, func(next *snapshotOperation) error {
		next.Phase = "frozen"
		next.Archive = &wire.Archive{SnapshotID: op.ID, Size: 8, ExpandedBytes: 8192, SHA256: strings.Repeat("c", 64), Format: op.Spec.Format}
		next.Artifact = snapshotArtifactName(op.SessionID, op.ID, op.Generation)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotJournalDurableOwnershipAndExclusiveWorker(t *testing.T) {
	j := testSnapshotJournal(t)
	op := testSnapshotOperation()
	freezeTestSnapshot(t, j, op)
	if other, err := openSnapshotJournal(j.root, "worker-a"); err == nil {
		_ = other.Close()
		t.Fatal("two processes acquired the journal")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := openSnapshotJournal(j.root, "worker-b"); err == nil {
		_ = other.Close()
		t.Fatal("a different worker inherited ownership")
	}
	reopened, err := openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.Current[op.SessionID] != op.ID || state.Operations[op.ID].Phase != "frozen" || state.Operations[op.ID].Archive.SHA256 != strings.Repeat("c", 64) {
		t.Fatal("reopen lost operation or archive identity")
	}
	for _, name := range []string{"journal.json", "journal.lock"} {
		stat, err := os.Stat(filepath.Join(j.root, name))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatalf("unsafe file mode: %s %v", name, err)
		}
	}
}

func TestSnapshotJournalFencesImmutabilityAndCanceledOperations(t *testing.T) {
	j := testSnapshotJournal(t)
	op := testSnapshotOperation()
	freezeTestSnapshot(t, j, op)
	for _, change := range []func(*snapshotOperation){
		func(o *snapshotOperation) { o.Generation++ },
		func(o *snapshotOperation) { o.StagingBytes-- },
		func(o *snapshotOperation) { o.ContainerID = strings.Repeat("d", 64) },
		func(o *snapshotOperation) { o.Spec.Env["PRIVATE"] = "overwritten" },
		func(o *snapshotOperation) { o.Spec.Command = []string{"changed"} },
		func(o *snapshotOperation) { o.Archive.SHA256 = strings.Repeat("d", 64) },
		func(o *snapshotOperation) { o.Phase = "active" },
	} {
		if err := j.Update(op.ID, func(o *snapshotOperation) error { change(o); return nil }); !errors.Is(err, errSnapshotJournalConflict) {
			t.Fatalf("immutable operation changed: %v", err)
		}
	}
	for _, phase := range []string{"rolling_back", "rolled_back"} {
		if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = phase; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "frozen"; return nil }); !errors.Is(err, errSnapshotJournalConflict) {
		t.Fatalf("canceled source restarted: %v", err)
	}
	next := op
	next.ID = "operation-2"
	if err := j.Claim(next); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "preparing"; return nil }); !errors.Is(err, errSnapshotJournalConflict) {
		t.Fatalf("superseded operation reclaimed ownership: %v", err)
	}
	state, _ := j.Snapshot()
	if state.Operations[op.ID].Spec.Env["PRIVATE"] != "protected" {
		t.Fatal("rejected callback mutated stored secret configuration")
	}
}

func TestSnapshotJournalWriteFailurePoisonsAdmissionAndPreservesPriorState(t *testing.T) {
	j := testSnapshotJournal(t)
	op := testSnapshotOperation()
	freezeTestSnapshot(t, j, op)
	j.persist = func(snapshotJournalState) error { return errors.New("simulated disk failure") }
	if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "rolling_back"; return nil }); !errors.Is(err, errSnapshotJournalUnavailable) {
		t.Fatal(err)
	}
	if _, err := j.Snapshot(); !errors.Is(err, errSnapshotJournalUnavailable) {
		t.Fatal("failed journal still allowed decisions")
	}
	if err := j.Claim(op); !errors.Is(err, errSnapshotJournalUnavailable) {
		t.Fatal("failed journal still accepted claims")
	}
	_ = j.Close()
	reopened, err := openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, _ := reopened.Snapshot()
	if state.Operations[op.ID].Phase != "frozen" {
		t.Fatal("failed write released ownership on disk")
	}
}

func TestSnapshotJournalRejectsCorruptOrSymlinkedState(t *testing.T) {
	for _, content := range []string{`{"version":1}`, `{"version":1,"worker_id":"worker-a","current":{},"operations":{}} {}`, `{"version":1,"worker_id":"worker-a","current":{"session":"missing"},"operations":{}}`} {
		root := t.TempDir()
		_ = os.Chmod(root, 0700)
		if err := os.WriteFile(filepath.Join(root, "journal.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if j, err := openSnapshotJournal(root, "worker-a"); err == nil {
			_ = j.Close()
			t.Fatal("corrupt state became an empty journal")
		}
	}
	root := t.TempDir()
	_ = os.Chmod(root, 0700)
	if err := os.Symlink(filepath.Join(t.TempDir(), "other"), filepath.Join(root, "journal.json")); err != nil {
		t.Fatal(err)
	}
	if j, err := openSnapshotJournal(root, "worker-a"); err == nil {
		_ = j.Close()
		t.Fatal("followed journal symlink")
	}
}

func TestSnapshotJournalDoesNotRetainMutableCallerConfiguration(t *testing.T) {
	j := testSnapshotJournal(t)
	op := testSnapshotOperation()
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	op.Spec.Env["PRIVATE"] = "changed outside journal"
	state, _ := j.Snapshot()
	if state.Operations[op.ID].Spec.Env["PRIVATE"] != "protected" {
		t.Fatal("caller mutated admitted restore configuration")
	}
	var retained *snapshotOperation
	if err := j.Update(op.ID, func(o *snapshotOperation) error { retained = o; return nil }); err != nil {
		t.Fatal(err)
	}
	retained.Spec.Env["PRIVATE"] = "changed after callback"
	state, _ = j.Snapshot()
	if state.Operations[op.ID].Spec.Env["PRIVATE"] != "protected" {
		t.Fatal("callback mutated committed restore configuration")
	}
}
