package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"

	wire "github.com/onlyboxes/onlyboxes/api/sandboxpersistence"
)

func snapshotSpecSHA256(spec wire.RestoreSpec) string {
	encoded, _ := json.Marshal(spec)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (op snapshotOperation) matchesSpec(spec wire.RestoreSpec) bool {
	if op.SpecSHA256 != "" {
		return op.SpecSHA256 == snapshotSpecSHA256(spec)
	}
	return snapshotSpecEqual(op.Spec, spec)
}

// Compacted records cannot carry execution or lease authority. They belong
// only to a permanently deleted scope; loader validation checks that linkage.
func validateCompactedSnapshotOperation(op snapshotOperation) error {
	empty, _ := json.Marshal(wire.RestoreSpec{})
	spec, _ := json.Marshal(op.Spec)
	if !snapshotOperationID.MatchString(op.ID) || !validPersistenceScope(op.SessionID) || op.Generation <= 0 || !dockerSnapshotDigest.MatchString("sha256:"+op.SpecSHA256) || !bytes.Equal(spec, empty) || op.StagingBytes != 0 || op.LeaseExpiresUnixMS != 0 || op.CacheUntilUnixMS != 0 || op.LeaseTTLSec != 0 || op.Committed || op.Verified || op.RemoteVerified {
		return errSnapshotJournalConflict
	}
	if (op.ContainerID != "" && !dockerSnapshotDigest.MatchString("sha256:"+op.ContainerID)) || (op.ImageID != "" && !dockerSnapshotDigest.MatchString(op.ImageID)) || (op.Artifact != "" && (filepath.Base(op.Artifact) != op.Artifact || op.Artifact == "." || op.Artifact == ".." || strings.ContainsAny(op.Artifact, "/\\\x00"))) {
		return errSnapshotJournalConflict
	}
	if op.Kind == "delete" {
		if op.Phase != "deleted" || op.Generation <= 1 || !dockerSnapshotDigest.MatchString("sha256:"+op.TombstoneID) || op.ImageID != "" || op.Artifact != "" || op.Archive != nil {
			return errSnapshotJournalConflict
		}
		return nil
	}
	if op.Phase != "retired" || op.TombstoneID != "" {
		return errSnapshotJournalConflict
	}
	switch op.Kind {
	case "target":
		if op.Archive == nil || op.Archive.Validate() != nil || op.Archive.Format != wire.DockerRootFSFormat {
			return errSnapshotJournalConflict
		}
	case "source", "adopt":
		if op.ContainerID == "" || op.Archive != nil {
			return errSnapshotJournalConflict
		}
	default:
		return errSnapshotJournalConflict
	}
	return nil
}

// A deleted phase is persisted only after exact native removal, lease release,
// name-fence confirmation and known artifact/image cleanup. Keep every replay
// locator, including for imports/exports that finish late, but drop duplicated
// environments, startup commands and obsolete execution/reservation state.
// This runs in the same atomic journal commit as the deletion acknowledgement.
func compactDeletedSnapshotHistory(state *snapshotJournalState) {
	deleted := make(map[string]snapshotOperation)
	for scope, id := range state.Current {
		op := state.Operations[id]
		if op.Kind == "delete" && op.Phase == "deleted" {
			deleted[scope] = op
			if op.SpecSHA256 == "" {
				op.SpecSHA256 = snapshotSpecSHA256(op.Spec)
				op.Spec = wire.RestoreSpec{}
				state.Operations[id] = op
				state.Version = 2
			}
		}
	}
	for id, op := range state.Operations {
		fence, found := deleted[op.SessionID]
		if !found || id == fence.ID {
			continue
		}
		if op.Kind == "cancel" {
			delete(state.Operations, id)
			state.Version = 2
			continue
		}
		if op.Generation >= fence.Generation || op.Phase == "retired" {
			continue
		}
		retired := snapshotOperation{ID: op.ID, SessionID: op.SessionID, Generation: op.Generation, Kind: op.Kind, Phase: "retired", SpecSHA256: snapshotSpecSHA256(op.Spec), ContainerID: op.ContainerID, ImageID: op.ImageID, Artifact: op.Artifact}
		if op.Kind == "target" {
			retired.Archive = op.Archive
		}
		state.Operations[id] = retired
		state.Version = 2
	}
}
