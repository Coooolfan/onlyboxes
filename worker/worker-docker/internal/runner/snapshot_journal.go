package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

var (
	errSnapshotJournalConflict    = errors.New("snapshot operation or generation conflict")
	errSnapshotJournalUnavailable = errors.New("snapshot operation journal is unavailable")
	snapshotOperationID           = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// Journals retain ownership and cancellation tombstones, never transfer grants.
// Artifact is a local basename within the private artifacts directory.
type snapshotOperation struct {
	ID                 string           `json:"operation_id"`
	SessionID          string           `json:"scoped_session_id"`
	Generation         int64            `json:"generation"`
	Kind               string           `json:"kind"`
	Phase              string           `json:"phase"`
	Spec               wire.RestoreSpec `json:"restore_spec"`
	Archive            *wire.Archive    `json:"archive,omitempty"`
	Artifact           string           `json:"artifact,omitempty"`
	TombstoneID        string           `json:"tombstone_id,omitempty"`
	ContainerID        string           `json:"container_id,omitempty"`
	ImageID            string           `json:"image_id,omitempty"`
	LeaseExpiresUnixMS int64            `json:"lease_expires_unix_ms,omitempty"`
	StagingBytes       int64            `json:"staging_bytes"`
	Committed          bool             `json:"snapshot_committed,omitempty"`
	Verified           bool             `json:"verified,omitempty"`
	RemoteVerified     bool             `json:"remote_verified,omitempty"`
	CacheUntilUnixMS   int64            `json:"cache_until_unix_ms,omitempty"`
	LeaseTTLSec        int              `json:"lease_ttl_sec,omitempty"`
}

func (o snapshotOperation) active() bool { return o.Phase == "active" || o.Phase == "rolled_back" }
func (o snapshotOperation) terminal() bool {
	return o.Phase == "finalized" || o.Phase == "canceled" || o.Phase == "deleted"
}
func (o snapshotOperation) held() bool { return !o.active() && !o.terminal() }

func (o snapshotOperation) validate() error {
	if o.Kind == "cancel" {
		empty, _ := json.Marshal(wire.RestoreSpec{})
		spec, _ := json.Marshal(o.Spec)
		if o.Phase == "canceled" && snapshotOperationID.MatchString(o.ID) && validPersistenceScope(o.SessionID) && o.Generation > 0 && bytes.Equal(empty, spec) && o.Archive == nil && o.Artifact == "" && o.ContainerID == "" && o.TombstoneID == "" && o.ImageID == "" && o.StagingBytes == 0 && o.LeaseExpiresUnixMS == 0 && !o.Committed && !o.Verified && !o.RemoteVerified && o.LeaseTTLSec == 0 && o.CacheUntilUnixMS == 0 {
			return nil
		}
		return errSnapshotJournalConflict
	}
	if o.TombstoneID != "" && (o.Kind != "delete" || !dockerSnapshotDigest.MatchString("sha256:"+o.TombstoneID)) {
		return errSnapshotJournalConflict
	}
	if o.Kind == "delete" && (o.Generation <= 1 || o.Archive != nil || o.Artifact != "" || o.ImageID != "" || o.StagingBytes != 0 || o.LeaseExpiresUnixMS != 0 || o.Verified || o.RemoteVerified || o.Committed || (o.Phase == "deleted" && o.TombstoneID == "")) {
		return errSnapshotJournalConflict
	}
	if o.RemoteVerified && (o.Kind != "source" || !o.Verified || o.Archive == nil) {
		return errSnapshotJournalConflict
	}

	if !snapshotOperationID.MatchString(o.ID) || !validPersistenceScope(o.SessionID) || o.Generation <= 0 || o.Spec.Validate() != nil || o.Spec.Runtime != "docker" || o.StagingBytes < 0 || o.StagingBytes > 1<<60 {
		return errSnapshotJournalConflict
	}
	if o.Kind != "source" && o.Kind != "target" && o.Kind != "adopt" && o.Kind != "delete" {
		return errSnapshotJournalConflict
	}
	allowed := map[string]bool{"preparing": true, "frozen": true, "committed": true, "finalized": true, "rolling_back": true, "rolled_back": true, "prepared": true, "importing": true, "ready": true, "active": true, "canceling": true, "canceled": true, "deleting": true, "deleted": true}
	if !allowed[o.Phase] || (o.Archive != nil && (o.Archive.Validate() != nil || o.Archive.Format != o.Spec.Format)) || (o.ContainerID != "" && !dockerSnapshotDigest.MatchString("sha256:"+o.ContainerID)) || (o.ImageID != "" && !dockerSnapshotDigest.MatchString(o.ImageID)) {
		return errSnapshotJournalConflict
	}
	if o.Artifact != "" && (filepath.Base(o.Artifact) != o.Artifact || o.Artifact == "." || strings.ContainsAny(o.Artifact, "/\\\x00")) {
		return errSnapshotJournalConflict
	}
	if o.Committed && (o.Kind != "source" || o.Archive == nil) {
		return errSnapshotJournalConflict
	}

	phases := map[string]map[string]bool{
		"source": {"preparing": true, "frozen": true, "committed": true, "finalized": true, "rolling_back": true, "rolled_back": true},
		"target": {"prepared": true, "importing": true, "ready": true, "active": true, "canceling": true, "canceled": true},
		"adopt":  {"active": true},
		"delete": {"deleting": true, "deleted": true},
	}
	if !phases[o.Kind][o.Phase] {
		return errSnapshotJournalConflict
	}
	if (o.Kind == "source" || o.active() || o.Phase == "ready") && o.ContainerID == "" {
		return errSnapshotJournalConflict
	}
	if (o.active() || o.Phase == "ready") && o.LeaseExpiresUnixMS <= 0 {
		return errSnapshotJournalConflict
	}
	if o.Kind == "target" && (o.Archive == nil || o.StagingBytes <= 0) {
		return errSnapshotJournalConflict
	}
	if (o.Phase == "frozen" || o.Phase == "committed" || o.Phase == "finalized") && (o.Archive == nil || o.Artifact == "") {
		return errSnapshotJournalConflict
	}
	if (o.Phase == "committed" || o.Phase == "finalized") != o.Committed {
		return errSnapshotJournalConflict
	}
	return nil
}

func validPersistenceScope(id string) bool {
	parts := strings.SplitN(id, ":", 3)
	return len(id) <= 1024 && len(parts) == 3 && parts[0] == "obx" && parts[1] != "" && parts[2] != "" && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\x00\r\n")
}

type snapshotJournalState struct {
	Version    int                          `json:"version"`
	WorkerID   string                       `json:"worker_id"`
	Current    map[string]string            `json:"current"`
	Operations map[string]snapshotOperation `json:"operations"`
	Probes     map[string]string            `json:"probes,omitempty"`
}

type snapshotJournal struct {
	mu       sync.Mutex
	root     string
	lock     *os.File
	state    snapshotJournalState
	poisoned bool
	closed   bool
	persist  func(snapshotJournalState) error
}

func openSnapshotJournal(root, worker string) (*snapshotJournal, error) {
	if root == "" || worker == "" || !filepath.IsAbs(root) {
		return nil, errSnapshotJournalUnavailable
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, errSnapshotJournalUnavailable
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errSnapshotJournalUnavailable
	}
	lock, err := os.OpenFile(filepath.Join(root, "journal.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errSnapshotJournalUnavailable
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errSnapshotJournalUnavailable
	}
	j := &snapshotJournal{root: root, lock: lock, state: snapshotJournalState{Version: 1, WorkerID: worker, Current: map[string]string{}, Operations: map[string]snapshotOperation{}}}
	j.persist = j.writeState
	fail := func() (*snapshotJournal, error) { _ = j.Close(); return nil, errSnapshotJournalUnavailable }
	file, err := os.OpenFile(filepath.Join(root, "journal.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		stat, e := file.Stat()
		if e != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() > 64<<20 {
			_ = file.Close()
			return fail()
		}
		decoder := json.NewDecoder(io.LimitReader(file, 64<<20+1))
		decoder.DisallowUnknownFields()
		var loaded snapshotJournalState
		err = decoder.Decode(&loaded)
		j.state = loaded
		var extra any
		if err == nil && decoder.Decode(&extra) != io.EOF {
			err = errSnapshotJournalUnavailable
		}
		_ = file.Close()
		if err != nil || j.state.Version != 1 || j.state.WorkerID != worker || j.state.Operations == nil || j.state.Current == nil {
			return fail()
		}
		for scope, id := range j.state.Probes {
			if !validPersistenceScope(scope) || !dockerSnapshotDigest.MatchString("sha256:"+id) {
				return fail()
			}
		}
		for id, op := range j.state.Operations {
			if id != op.ID || op.validate() != nil {
				return fail()
			}
		}
		for session, id := range j.state.Current {
			op, ok := j.state.Operations[id]
			if !ok || session != op.SessionID {
				return fail()
			}
		}
	} else if !os.IsNotExist(err) {
		return fail()
	} else if j.persist(j.state) != nil {
		return fail()
	}
	if err := os.MkdirAll(filepath.Join(root, "artifacts"), 0700); err != nil {
		return fail()
	}
	artifactInfo, err := os.Lstat(filepath.Join(root, "artifacts"))
	if err != nil || !artifactInfo.IsDir() || artifactInfo.Mode().Perm()&0077 != 0 || artifactInfo.Mode()&os.ModeSymlink != 0 {
		return fail()
	}
	return j, nil
}

func (j *snapshotJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
	return j.lock.Close()
}

func (j *snapshotJournal) writeState(state snapshotJournalState) error {
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > 64<<20 {
		return errSnapshotJournalUnavailable
	}
	file, err := os.CreateTemp(j.root, "journal-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, filepath.Join(j.root, "journal.json")); err != nil {
		return err
	}
	dir, err := os.Open(j.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cloneSnapshotJournal(state snapshotJournalState) snapshotJournalState {
	encoded, _ := json.Marshal(state)
	var copy snapshotJournalState
	_ = json.Unmarshal(encoded, &copy)
	return copy
}

func (j *snapshotJournal) transaction(change func(*snapshotJournalState) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.poisoned {
		return errSnapshotJournalUnavailable
	}
	next := cloneSnapshotJournal(j.state)
	if err := change(&next); err != nil {
		return err
	}
	if err := j.persist(next); err != nil {
		j.poisoned = true
		return errSnapshotJournalUnavailable
	}
	j.state = cloneSnapshotJournal(next)
	return nil
}

func (j *snapshotJournal) Snapshot() (snapshotJournalState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.poisoned {
		return snapshotJournalState{}, errSnapshotJournalUnavailable
	}
	return cloneSnapshotJournal(j.state), nil
}

func (j *snapshotJournal) Claim(op snapshotOperation) error {
	if op.validate() != nil {
		return errSnapshotJournalConflict
	}
	if (op.Kind == "source" && op.Phase != "preparing") || (op.Kind == "target" && op.Phase != "prepared") || (op.Kind == "adopt" && op.Phase != "active") || (op.Kind == "delete" && op.Phase != "deleting") {
		return errSnapshotJournalConflict
	}
	return j.transaction(func(state *snapshotJournalState) error {
		if prior, ok := state.Operations[op.ID]; ok {
			if state.Current[op.SessionID] != op.ID || prior.SessionID != op.SessionID || prior.Generation != op.Generation || prior.Kind != op.Kind || !snapshotSpecEqual(prior.Spec, op.Spec) {
				return errSnapshotJournalConflict
			}
			return nil
		}
		if id := state.Current[op.SessionID]; id != "" {
			prior := state.Operations[id]
			if (prior.Kind == "delete" && op.Kind != "delete") || (prior.Kind != "cancel" && !snapshotSpecEqual(prior.Spec, op.Spec)) {
				return errSnapshotJournalConflict
			}
			if !((op.Kind == "delete" && prior.Generation < op.Generation) || (prior.active() && op.Kind == "source" && prior.Generation == op.Generation) || (prior.terminal() && op.Kind == "target" && prior.Generation < op.Generation)) {
				return errSnapshotJournalConflict
			}
		}
		// Lower-generation terminal tombstones can be compacted: the current
		// generation still rejects every delayed control from those generations.
		for id, prior := range state.Operations {
			if prior.SessionID == op.SessionID && prior.Generation < op.Generation && state.Current[op.SessionID] != id && prior.Artifact == "" {
				delete(state.Operations, id)
			}
		}
		state.Operations[op.ID] = op
		if op.Generation > 1 {
			delete(state.Probes, op.SessionID)
		}
		state.Current[op.SessionID] = op.ID
		return nil
	})
}

func snapshotSpecEqual(a, b wire.RestoreSpec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Update never mutates the generation, specification, committed archive or an
// already-chosen backend identity. Terminal operations cannot be restarted.
func (j *snapshotJournal) Update(id string, change func(*snapshotOperation) error) error {
	return j.transaction(func(state *snapshotJournalState) error {
		prior, ok := state.Operations[id]
		if !ok || state.Current[prior.SessionID] != id {
			return errSnapshotJournalConflict
		}
		var next snapshotOperation
		encoded, _ := json.Marshal(prior)
		_ = json.Unmarshal(encoded, &next)
		if err := change(&next); err != nil {
			return err
		}
		if next.validate() != nil || next.ID != prior.ID || next.SessionID != prior.SessionID || next.Generation != prior.Generation || next.Kind != prior.Kind || next.StagingBytes != prior.StagingBytes || !snapshotSpecEqual(next.Spec, prior.Spec) || (prior.TombstoneID != "" && next.TombstoneID != prior.TombstoneID) || (prior.ContainerID != "" && next.ContainerID != prior.ContainerID) || (prior.ImageID != "" && next.ImageID != prior.ImageID) || (prior.Artifact != "" && next.Artifact != prior.Artifact) || (prior.Committed && !next.Committed) || (prior.Archive != nil && (next.Archive == nil || *next.Archive != *prior.Archive)) {
			return errSnapshotJournalConflict
		}
		if !snapshotPhaseTransition(prior.Kind, prior.Phase, next.Phase) {
			return errSnapshotJournalConflict
		}
		state.Operations[id] = next
		return nil
	})
}

func snapshotPhaseTransition(kind, from, to string) bool {
	if from == to {
		return true
	}
	if kind == "delete" {
		return from == "deleting" && to == "deleted"
	}
	if kind == "source" {
		return (from == "preparing" && (to == "frozen" || to == "rolling_back")) || (from == "frozen" && (to == "committed" || to == "rolling_back")) || (from == "committed" && to == "finalized") || (from == "rolling_back" && to == "rolled_back")
	}
	if kind == "target" {
		return (from == "prepared" && to == "importing") || (from == "importing" && to == "ready") || (from == "ready" && to == "active") || (from != "canceled" && to == "canceling") || (from == "canceling" && to == "canceled")
	}
	return false
}

func snapshotArtifactName(session, operation string, generation int64) string {
	identity, _ := json.Marshal([]any{session, operation, generation})
	hash := sha256.Sum256(identity)
	return hex.EncodeToString(hash[:]) + ".tar"
}

func (j *snapshotJournal) CancelUnknown(id, scope string, generation int64) error {
	if !snapshotOperationID.MatchString(id) || !validPersistenceScope(scope) || generation <= 0 {
		return errSnapshotJournalConflict
	}
	return j.transaction(func(state *snapshotJournalState) error {
		if _, known := state.Operations[id]; known {
			return errSnapshotJournalConflict
		}
		op := snapshotOperation{ID: id, SessionID: scope, Generation: generation, Kind: "cancel", Phase: "canceled"}
		state.Operations[id] = op
		current := state.Operations[state.Current[scope]]
		if current.ID == "" || current.terminal() && current.Generation < generation {
			state.Current[scope] = id
		}
		return nil
	})
}

// Probe binds initial adoption to an immutable instance without freezing it or
// changing its execution generation. A delayed legacy create cannot replace it.
func (j *snapshotJournal) Probe(scope, id string) error {
	if !validPersistenceScope(scope) || !dockerSnapshotDigest.MatchString("sha256:"+id) {
		return errSnapshotJournalConflict
	}
	return j.transaction(func(state *snapshotJournalState) error {
		if prior := state.Probes[scope]; prior != "" && prior != id {
			return errSnapshotJournalConflict
		}
		if state.Probes == nil {
			state.Probes = map[string]string{}
		}
		state.Probes[scope] = id
		return nil
	})
}
