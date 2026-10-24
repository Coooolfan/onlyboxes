package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func journalDeletionFence(t *testing.T, j *snapshotJournal, phase string) snapshotOperation {
	t.Helper()
	op := testSnapshotOperation()
	op.ID, op.Kind, op.Phase, op.Generation = "delete-session", "delete", "deleting", 2
	op.ContainerID, op.StagingBytes, op.LeaseExpiresUnixMS = "", 0, 0
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	if phase == "deleted" {
		if err := j.Update(op.ID, func(o *snapshotOperation) error {
			o.Phase = "deleted"
			o.TombstoneID = strings.Repeat("c", 64)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return op
}

func TestSnapshotUnknownCancellationCannotReplacePermanentDeletionFence(t *testing.T) {
	for _, phase := range []string{"deleting", "deleted"} {
		t.Run(phase, func(t *testing.T) {
			j := testSnapshotJournal(t)
			deletion := journalDeletionFence(t, j, phase)
			if err := j.CancelUnknown("future-cancel", deletion.SessionID, 3); !errors.Is(err, errSnapshotJournalConflict) {
				t.Fatal("unknown cancellation replaced the permanent fence", err)
			}
			replacement := deletion
			replacement.ID = "future-delete"
			replacement.Generation = 3
			if err := j.Claim(replacement); !errors.Is(err, errSnapshotJournalConflict) {
				t.Fatal("a new deletion operation replaced the stable fence", err)
			}
			state, err := j.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if state.Current[deletion.SessionID] != deletion.ID || len(state.Operations) != 1 {
				t.Fatal("rejected cancellation mutated journal ownership")
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openSnapshotJournal(j.root, "worker-a")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.CancelUnknown("future-cancel-after-reopen", deletion.SessionID, 100); !errors.Is(err, errSnapshotJournalConflict) {
				t.Fatal("reopen lost the permanent fence", err)
			}
		})
	}
}

func TestSnapshotDeletedScopeRejectsUnknownControlsBeforeNativeIO(t *testing.T) {
	c, j := testSnapshotController(t)
	deletion := journalDeletionFence(t, j, "deleted")
	root := t.TempDir()
	marker := filepath.Join(root, "native-io")
	t.Setenv("ONLYBOXES_SNAPSHOT_UNEXPECTED_IO", marker)
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte("#!/bin/sh\n: > \"$ONLYBOXES_SNAPSHOT_UNEXPECTED_IO\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	archive := transferTestArchive([]byte("snapshot"))
	flaggedRestore, _ := json.Marshal(wire.Control{PurgeSession: true, Spec: &deletion.Spec, Archive: &archive, LeaseTTLSec: 300})
	flaggedDelete, _ := json.Marshal(wire.Control{PurgeSession: true, Spec: &deletion.Spec})
	for _, entry := range []struct {
		action  registryv1.SandboxPersistenceControl_Action
		payload []byte
	}{
		{registryv1.SandboxPersistenceControl_CANCEL, []byte(`{}`)},
		{registryv1.SandboxPersistenceControl_PREPARE_RESTORE, flaggedRestore},
		{registryv1.SandboxPersistenceControl_CANCEL, flaggedDelete},
	} {
		control := &registryv1.SandboxPersistenceControl{RequestId: uuid.NewString(), OperationId: "future-cancel", ScopedSessionId: deletion.SessionID, Generation: 3, Action: entry.action, DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli(), PayloadJson: entry.payload}
		report := c.Handle(control)
		if report.Error == nil || report.Error.Code != "session_state_conflict" {
			t.Fatal("unknown control was accepted after permanent deletion", entry.action, report.Error)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("deleted-scope rejection reached the native runtime", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotJournalRejectsHistoryThatDisplacesADeletionFence(t *testing.T) {
	j := testSnapshotJournal(t)
	deletion := journalDeletionFence(t, j, "deleted")
	state, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	state.Operations["future-cancel"] = snapshotOperation{ID: "future-cancel", SessionID: deletion.SessionID, Generation: 3, Kind: "cancel", Phase: "canceled"}
	state.Current[deletion.SessionID] = "future-cancel"
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(j.root, "journal.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openSnapshotJournal(j.root, "worker-a"); err == nil {
		_ = reopened.Close()
		t.Fatal("inconsistent permanent-deletion history was accepted")
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(encoded) {
		t.Fatal("rejected history was rewritten or erased", err)
	}
}

func TestSnapshotFutureCancellationRetainsAnExistingActiveOwner(t *testing.T) {
	j := testSnapshotJournal(t)
	owner := testSnapshotOperation()
	owner.Kind, owner.Phase = "adopt", "active"
	if err := j.Claim(owner); err != nil {
		t.Fatal(err)
	}
	if err := j.CancelUnknown("future-cancel", owner.SessionID, 3); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal("valid future cancellation prevented owner recovery", err)
	}
	defer j.Close()
	state, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.Current[owner.SessionID] != owner.ID || !state.Operations[owner.ID].active() || state.Operations["future-cancel"].Kind != "cancel" {
		t.Fatal("future cancellation changed the existing owner")
	}
}

func TestSnapshotJournalRetainsNativeCleanupProofWithoutArtifactNames(t *testing.T) {
	j := testSnapshotJournal(t)
	archive := transferTestArchive([]byte("snapshot"))
	old := testSnapshotOperation()
	old.ID, old.Kind, old.Phase = "old-restore", "target", "prepared"
	old.Archive = &archive
	if err := j.Claim(old); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"importing", "ready", "active"} {
		if err := j.Update(old.ID, func(o *snapshotOperation) error {
			o.Phase = phase
			o.ImageID = "sha256:" + strings.Repeat("c", 64)
			o.Verified = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	source := testSnapshotOperation()
	source.ID = "old-export"
	source.Artifact = snapshotArtifactName(source.SessionID, source.ID, source.Generation)
	if err := j.Claim(source); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"frozen", "committed", "finalized"} {
		if err := j.Update(source.ID, func(o *snapshotOperation) error {
			o.Phase = phase
			o.Archive = &archive
			o.Verified = true
			o.RemoteVerified = true
			o.Committed = phase != "frozen"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	next := old
	next.ID = "next-restore"
	next.Generation = 2
	if err := j.Claim(next); err != nil {
		t.Fatal(err)
	}
	if err := j.CancelUnknown("empty-cancel", old.SessionID, 1); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"canceling", "canceled"} {
		if err := j.Update(next.ID, func(o *snapshotOperation) error { o.Phase = phase; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	newest := old
	newest.ID = "newest-restore"
	newest.Generation = 3
	if err := j.Claim(newest); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	state, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Operations["empty-cancel"]; exists {
		t.Fatal("empty lower-generation cancellation was not compacted")
	}
	retained, found := state.Operations[old.ID]
	if !found || retained.ImageID == "" || !snapshotImportRetired(state, finalizedSnapshotRoots(state), retained) {
		t.Fatal("compaction lost native image cleanup identity or finalized-root proof")
	}
	if state.Current[old.SessionID] != newest.ID {
		t.Fatal("compaction changed current generation")
	}
}
