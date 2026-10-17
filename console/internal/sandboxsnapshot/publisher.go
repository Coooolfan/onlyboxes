package sandboxsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
)

// ArchiveObjects is the Console control-plane surface. It completes uploads and
// writes small manifests; workers, not user-facing HTTP handlers, transfer archives.
type ArchiveObjects interface {
	CompleteUpload(context.Context, objectstore.SnapshotUpload, []objectstore.SnapshotPart) (objectstore.SnapshotObject, error)
	PutSnapshotManifest(context.Context, objectstore.SnapshotObject, json.RawMessage) (string, error)
}

// ArchiveVerifier must verify the full remote object against its reserved SHA-256.
// The worker integration can execute this readback remotely using a scoped GET
// grant, keeping archive bytes out of the Console's lifecycle control connection.
type ArchiveVerifier interface {
	VerifySnapshot(context.Context, objectstore.SnapshotObject) error
}

type Publisher struct {
	repository *Repository
	objects    ArchiveObjects
	verifier   ArchiveVerifier
}

func NewPublisher(repository *Repository, objects ArchiveObjects, verifier ArchiveVerifier) (*Publisher, error) {
	if repository == nil || objects == nil || verifier == nil {
		return nil, errors.New("snapshot publication requires repository, object store and verifier")
	}
	return &Publisher{repository: repository, objects: objects, verifier: verifier}, nil
}

// Publish makes the uploaded bytes the current durable recovery point. It must
// only be invoked once the source backend confirms all writers are stopped. This
// method never deletes the source instance or opens a user execution route.
func (p *Publisher) Publish(ctx context.Context, operation Operation, snapshotID string, retention time.Duration) (Snapshot, error) {
	if retention <= 0 {
		return Snapshot{}, errors.New("snapshot retention must be positive")
	}
	snapshot, err := p.repository.Snapshot(ctx, operation.OwnerID, operation.SessionID, snapshotID)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.OperationID != operation.ID || snapshot.Generation != operation.Generation || operation.Kind != Suspend {
		return Snapshot{}, ErrConflict
	}
	// A completed publication is immutable and can satisfy retries even after the
	// session moved on to another generation. Never change the session on this path.
	if snapshot.Status == "ready" {
		return snapshot, nil
	}
	session, err := p.repository.Session(ctx, operation.OwnerID, operation.SessionID)
	if err != nil {
		return Snapshot{}, err
	}
	current, err := p.repository.Operation(ctx, operation.OwnerID, operation.SessionID, operation.ID)
	if err != nil {
		return Snapshot{}, err
	}
	if session.OperationID != operation.ID || session.Generation != operation.Generation || session.State != Snapshotting || current.Kind != Suspend || (current.Status != "pending" && current.Status != "running") {
		return Snapshot{}, ErrConflict
	}
	switch snapshot.Status {
	case "uploading":
		parts, err := p.repository.Parts(ctx, operation.OwnerID, operation.SessionID, snapshotID)
		if err != nil {
			return Snapshot{}, err
		}
		object, err := p.objects.CompleteUpload(ctx, snapshot.Upload, parts)
		if err != nil {
			return Snapshot{}, err
		}
		expected := snapshot.Upload.Object
		expected.VersionID = object.VersionID
		if object != expected {
			return Snapshot{}, errors.New("completed object differs from the reserved snapshot")
		}
		if err = p.verifier.VerifySnapshot(ctx, object); err != nil {
			return Snapshot{}, err
		}
		manifest := Manifest{Version: 1, Archive: object, RestoreSpec: session.Spec}
		payload, err := json.Marshal(manifest)
		if err != nil {
			return Snapshot{}, err
		}
		descriptorVersion, err := p.objects.PutSnapshotManifest(ctx, object, payload)
		if err != nil {
			return Snapshot{}, err
		}
		if err = p.repository.MarkVerified(ctx, operation, snapshotID, manifest, descriptorVersion); err != nil {
			return Snapshot{}, err
		}
	case "verified":
		// Verification and immutable manifest creation already survived a restart.
	default:
		return Snapshot{}, ErrConflict
	}
	if err = p.repository.CommitSuspend(ctx, operation, snapshotID, retention); err != nil {
		return Snapshot{}, err
	}
	return p.repository.Snapshot(ctx, operation.OwnerID, operation.SessionID, snapshotID)
}
