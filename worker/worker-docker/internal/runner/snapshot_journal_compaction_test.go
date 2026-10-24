package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func snapshotCompactionFixture(t *testing.T, count int) (snapshotJournalState, string) {
	t.Helper()
	base := testSnapshotOperation()
	base.Spec.Env = map[string]string{"PRIVATE": "deleted-private-" + strings.Repeat("v", 16<<10)}
	base.Spec.Command = []string{"private-startup-command"}
	state := snapshotJournalState{Version: 1, WorkerID: "worker-a", Current: map[string]string{}, Operations: map[string]snapshotOperation{}}
	archive := transferTestArchive([]byte("archive"))
	for i := 0; i < count; i++ {
		op := base
		op.ID = fmt.Sprintf("history-%d", i)
		op.Generation = int64(i + 1)
		op.ContainerID = fmt.Sprintf("%064x", i+1)
		op.Archive = &archive
		op.Artifact = snapshotArtifactName(op.SessionID, op.ID, op.Generation)
		op.Verified = true
		op.CacheUntilUnixMS = time.Now().Add(time.Hour).UnixMilli()
		op.LeaseTTLSec = 300
		if i%2 == 0 {
			op.Kind, op.Phase = "target", "active"
			op.ImageID = "sha256:" + fmt.Sprintf("%064x", i+100)
		} else {
			op.Phase = "finalized"
			op.Committed, op.RemoteVerified = true, true
		}
		if err := op.validate(); err != nil {
			t.Fatal(err)
		}
		state.Operations[op.ID] = op
	}
	deletion := base
	deletion.ID, deletion.Kind, deletion.Phase, deletion.Generation = "delete-session", "delete", "deleting", int64(count+1)
	deletion.ContainerID = ""
	deletion.StagingBytes = 0
	deletion.LeaseExpiresUnixMS = 0
	deletion.TombstoneID = strings.Repeat("e", 64)
	state.Operations[deletion.ID] = deletion
	state.Current[deletion.SessionID] = deletion.ID
	state.Operations["future-cancel"] = snapshotOperation{ID: "future-cancel", SessionID: deletion.SessionID, Generation: deletion.Generation + 10, Kind: "cancel", Phase: "canceled"}
	live := testSnapshotOperation()
	live.ID, live.SessionID, live.Kind, live.Phase = "live", "obx:owner:live", "adopt", "active"
	live.ContainerID = strings.Repeat("f", 64)
	live.Spec.Env = map[string]string{"PRIVATE": "live-secret-must-remain"}
	state.Operations[live.ID] = live
	state.Current[live.SessionID] = live.ID
	return state, deletion.ID
}

func TestSnapshotDeletionCompactsPrivateHistoryAndKeepsEveryCleanupLocator(t *testing.T) {
	j := testSnapshotJournal(t)
	full, id := snapshotCompactionFixture(t, 120)
	before, _ := json.Marshal(full)
	if err := j.transaction(func(s *snapshotJournalState) error { *s = full; return nil }); err != nil {
		t.Fatal(err)
	}
	// Admission/cleanup still pending: no secret, lease or cache state is retired.
	pending, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Version != 1 || pending.Operations["history-0"].Spec.Env["PRIVATE"] == "" {
		t.Fatal("deleting history was compacted before physical cleanup acknowledgement")
	}
	if err := j.Update(id, func(o *snapshotOperation) error { o.Phase = "deleted"; return nil }); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(j.root, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)*10 >= len(before) {
		t.Fatal("history compaction did not reduce duplicated configurations", len(before), len(encoded))
	}
	if bytes.Contains(encoded, []byte("deleted-private-")) || bytes.Contains(encoded, []byte("private-startup-command")) {
		t.Fatal("deleted environment/startup data retained in compacted history")
	}
	if !bytes.Contains(encoded, []byte("live-secret-must-remain")) {
		t.Fatal("another live scope lost its configuration")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = openSnapshotJournal(j.root, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	state, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 2 || state.Current[full.Operations[id].SessionID] != id || state.Operations[id].TombstoneID != full.Operations[id].TombstoneID {
		t.Fatal("compaction lost the permanent deletion fence")
	}
	if !state.Operations[id].matchesSpec(full.Operations[id].Spec) {
		t.Fatal("deletion lost its original configuration binding")
	}
	wrong := full.Operations[id].Spec
	wrong.CPUs++
	if state.Operations[id].matchesSpec(wrong) {
		t.Fatal("compacted deletion accepted changed configuration")
	}
	for key, original := range full.Operations {
		if original.Kind == "cancel" {
			if _, exists := state.Operations[key]; exists {
				t.Fatal("permanently fenced cancellation was retained")
			}
			continue
		}
		actual := state.Operations[key]
		if key == id || key == "live" {
			continue
		}
		if actual.Phase != "retired" || actual.SpecSHA256 != snapshotSpecSHA256(original.Spec) || actual.ContainerID != original.ContainerID || actual.ImageID != original.ImageID || actual.Artifact != original.Artifact || actual.Generation != original.Generation || actual.Kind != original.Kind || actual.SessionID != original.SessionID {
			t.Fatal("cleanup locator changed during compaction", key)
		}
		if original.Kind == "target" && (actual.Archive == nil || *actual.Archive != *original.Archive || !snapshotImportRetired(state, finalizedSnapshotRoots(state), actual)) {
			t.Fatal("late-import checksum/retirement proof was lost", key)
		}
		if actual.StagingBytes != 0 || actual.LeaseExpiresUnixMS != 0 || actual.Verified || actual.Committed {
			t.Fatal("retired cleanup record retained execution/transfer authority")
		}
	}
	if err := j.CancelUnknown("new-cancel", full.Operations[id].SessionID, 1000); !errors.Is(err, errSnapshotJournalConflict) {
		t.Fatal("compaction opened a permanently deleted scope", err)
	}
	if err := j.Update(id, func(o *snapshotOperation) error { o.ContainerID = strings.Repeat("c", 64); return nil }); !errors.Is(err, errSnapshotJournalConflict) {
		t.Fatal("compacted fence acquired a different native root", err)
	}
	if err := j.Update(id, func(o *snapshotOperation) error { o.Phase = "deleted"; return nil }); err != nil {
		t.Fatal(err)
	}
	replayed, err := os.ReadFile(filepath.Join(j.root, "journal.json"))
	if err != nil || !bytes.Equal(encoded, replayed) {
		t.Fatal("compaction replay was not stable", err)
	}
	t.Logf("compacted deleted history from %d to %d bytes", len(before), len(encoded))
}

func TestSnapshotCompactionWriteFailureRetainsEitherDurableHistory(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			j := testSnapshotJournal(t)
			state, id := snapshotCompactionFixture(t, 2)
			if err := j.transaction(func(s *snapshotJournalState) error { *s = state; return nil }); err != nil {
				t.Fatal(err)
			}
			persist := j.persist
			j.persist = func(next snapshotJournalState) error {
				if replace {
					if err := persist(next); err != nil {
						return err
					}
				}
				return errors.New("fixture fsync failure")
			}
			if err := j.Update(id, func(o *snapshotOperation) error { o.Phase = "deleted"; return nil }); !errors.Is(err, errSnapshotJournalUnavailable) {
				t.Fatal("failed compaction was acknowledged", err)
			}
			if _, err := j.Snapshot(); !errors.Is(err, errSnapshotJournalUnavailable) {
				t.Fatal("failed compaction did not poison admission", err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			j, err := openSnapshotJournal(j.root, "worker-a")
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			loaded, err := j.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Current[state.Operations[id].SessionID] != id || loaded.Operations["history-0"].ImageID != state.Operations["history-0"].ImageID {
				t.Fatal("durable cleanup identity lost after failed compaction")
			}
			if replace {
				if loaded.Version != 2 || loaded.Operations[id].Phase != "deleted" {
					t.Fatal("committed compacted state was not recoverable")
				}
			} else {
				if loaded.Version != 1 || loaded.Operations[id].Phase != "deleting" || loaded.Operations["history-0"].Spec.Env["PRIVATE"] == "" {
					t.Fatal("uncommitted compaction overwrote prior history")
				}
			}
		})
	}
}

func TestSnapshotCompactionCannotCreateAUsableOrUnfencedRecord(t *testing.T) {
	full, id := snapshotCompactionFixture(t, 2)
	fence := full.Operations[id]
	fence.Phase = "deleted"
	full.Operations[id] = fence
	compactDeletedSnapshotHistory(&full)
	for _, change := range []func(*snapshotJournalState){
		func(s *snapshotJournalState) { s.Version = 1 },
		func(s *snapshotJournalState) {
			op := s.Operations["history-0"]
			op.Phase = "active"
			s.Operations[op.ID] = op
		},
		func(s *snapshotJournalState) {
			op := s.Operations["history-0"]
			op.LeaseExpiresUnixMS = 1
			s.Operations[op.ID] = op
		},
		func(s *snapshotJournalState) {
			op := s.Operations["history-0"]
			op.Artifact = ".."
			s.Operations[op.ID] = op
		},
		func(s *snapshotJournalState) {
			op := s.Operations[id]
			op.Spec = wire.RestoreSpec{Runtime: "docker"}
			s.Operations[id] = op
		},
		func(s *snapshotJournalState) { s.Current[s.Operations[id].SessionID] = "history-0" },
	} {
		j := testSnapshotJournal(t)
		bad := cloneSnapshotJournal(full)
		change(&bad)
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(bad)
		path := filepath.Join(j.root, "journal.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if loaded, err := openSnapshotJournal(j.root, "worker-a"); err == nil {
			_ = loaded.Close()
			t.Fatal("unsafe compacted history was accepted")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, data) {
			t.Fatal("invalid compacted history was rewritten", err)
		}
	}
}

func TestSnapshotCompactionPreservesSuspendedAndUnknownNativeOutcomes(t *testing.T) {
	state, _ := snapshotCompactionFixture(t, 2) // Its deletion is still unfinished.
	source := testSnapshotOperation()
	source.ID, source.SessionID, source.Phase = "suspended", "obx:owner:suspended", "finalized"
	archive := transferTestArchive([]byte("snapshot"))
	source.Archive = &archive
	source.Artifact = snapshotArtifactName(source.SessionID, source.ID, source.Generation)
	source.Verified, source.RemoteVerified, source.Committed = true, true, true
	source.Spec.Env = map[string]string{"PRIVATE": "suspended-configuration"}
	pending := source
	pending.ID, pending.SessionID, pending.Kind, pending.Phase = "unknown-import", "obx:owner:unknown", "target", "importing"
	pending.ContainerID, pending.LeaseExpiresUnixMS, pending.Committed, pending.RemoteVerified = "", 0, false, false
	pending.Artifact = snapshotArtifactName(pending.SessionID, pending.ID, pending.Generation)
	for _, op := range []snapshotOperation{source, pending} {
		if err := op.validate(); err != nil {
			t.Fatal(err)
		}
		state.Operations[op.ID] = op
		state.Current[op.SessionID] = op.ID
	}
	before, _ := json.Marshal(state)
	compactDeletedSnapshotHistory(&state)
	after, _ := json.Marshal(state)
	if !bytes.Equal(before, after) {
		t.Fatal("compaction rewrote unfinished deletion, suspended cache or unknown import ownership")
	}
}
