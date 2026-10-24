package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotImportRetirementRequiresConfirmedNativeDisposition(t *testing.T) {
	target := testSnapshotOperation()
	target.ID, target.Kind, target.Phase = "restore", "target", "active"
	archive := transferTestArchive([]byte("snapshot"))
	target.Archive = &archive
	source := target
	source.ID, source.Kind, source.Phase = "suspend", "source", "finalized"
	source.Committed, source.RemoteVerified, source.Verified = true, true, true
	for _, tc := range []struct {
		name    string
		change  func(*snapshotJournalState)
		retired bool
	}{
		{"confirmed-source-removal", func(*snapshotJournalState) {}, true},
		{"active-current", func(s *snapshotJournalState) { s.Current[target.SessionID] = target.ID }, false},
		{"only-newer-operation", func(s *snapshotJournalState) { delete(s.Operations, source.ID) }, false},
		{"paused-source", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Phase = "frozen"
			s.Operations[source.ID] = o
		}, false},
		{"committed-before-cleanup", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Phase = "committed"
			s.Operations[source.ID] = o
		}, false},
		{"unverified-remote", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.RemoteVerified = false
			s.Operations[source.ID] = o
		}, false},
		{"uncommitted-source", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Committed = false
			s.Operations[source.ID] = o
		}, false},
		{"different-native-root", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.ContainerID = strings.Repeat("c", 64)
			s.Operations[source.ID] = o
		}, false},
		{"different-generation", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Generation++
			s.Operations[source.ID] = o
		}, false},
		{"different-scope", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.SessionID = "obx:other:session"
			s.Operations[source.ID] = o
		}, false},
		{"different-spec", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Spec.CPUs++
			s.Operations[source.ID] = o
		}, false},
		{"confirmed-cancel", func(s *snapshotJournalState) {
			delete(s.Operations, source.ID)
			o := s.Operations[target.ID]
			o.Phase = "canceled"
			s.Operations[target.ID] = o
			s.Current[target.SessionID] = target.ID
		}, true},
		{"cancel-not-confirmed", func(s *snapshotJournalState) {
			delete(s.Operations, source.ID)
			o := s.Operations[target.ID]
			o.Phase = "canceling"
			s.Operations[target.ID] = o
		}, false},
		{"logical-delete-only", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Kind = "delete"
			o.Phase = "deleting"
			o.Generation++
			s.Operations[source.ID] = o
		}, false},
		{"physical-delete-confirmed", func(s *snapshotJournalState) {
			o := s.Operations[source.ID]
			o.Kind = "delete"
			o.Phase = "deleted"
			o.Generation++
			s.Operations[source.ID] = o
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := snapshotJournalState{Current: map[string]string{target.SessionID: source.ID}, Operations: map[string]snapshotOperation{target.ID: target, source.ID: source}}
			tc.change(&state)
			if actual := snapshotImportRetired(state, finalizedSnapshotRoots(state), state.Operations[target.ID]); actual != tc.retired {
				t.Fatal("incorrect import retirement", actual)
			}
		})
	}
}

func TestSnapshotImportCleanupUsesScopeAdmissionAndRetainsItsDurableIdentity(t *testing.T) {
	c, j := testSnapshotController(t)
	op := testSnapshotOperation()
	op.Kind, op.Phase = "target", "prepared"
	archive := transferTestArchive([]byte("snapshot"))
	op.Archive = &archive
	if err := j.Claim(op); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "canceling"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := j.Update(op.ID, func(o *snapshotOperation) error { o.Phase = "canceled"; return nil }); err != nil {
		t.Fatal(err)
	}
	state, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	next := make(map[string]time.Time)
	c.actions[op.SessionID] = snapshotActiveAction{cancel: func() {}}
	if c.reconcileRetiredImports(context.Background(), state, now, next) != 0 || len(next) != 0 {
		t.Fatal("cleanup raced an admitted lifecycle action")
	}
	delete(c.actions, op.SessionID)
	c.recovering = true
	if c.reconcileRetiredImports(context.Background(), state, now, next) != 0 {
		t.Fatal("cleanup ran during reconnect")
	}
	c.recovering = false
	root := t.TempDir()
	marker := filepath.Join(root, "called")
	t.Setenv("ONLYBOXES_IMPORT_GC_MARKER", marker)
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte("#!/bin/sh\n: > \"$ONLYBOXES_IMPORT_GC_MARKER\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	if c.reconcileRetiredImports(context.Background(), state, now, next) != 1 {
		t.Fatal("confirmed cancellation was not collected")
	}
	c.wg.Wait()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("native cleanup was not called")
	}
	if c.reconcileRetiredImports(context.Background(), state, now, next) != 0 {
		t.Fatal("cleanup ignored rate limit")
	}
	fresh, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Current[op.SessionID] != op.ID || fresh.Operations[op.ID].Phase != "canceled" || fresh.Operations[op.ID].Archive == nil {
		t.Fatal("image cleanup discarded the cancellation fence or archive identity")
	}
}
