package sandboxsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
)

type publicationStore struct {
	verifyError   error
	manifestError error
	calls         []string
}

func (s *publicationStore) CompleteUpload(_ context.Context, u objectstore.SnapshotUpload, parts []objectstore.SnapshotPart) (objectstore.SnapshotObject, error) {
	s.calls = append(s.calls, "complete")
	return u.Object, nil
}
func (s *publicationStore) VerifySnapshot(context.Context, objectstore.SnapshotObject) error {
	s.calls = append(s.calls, "verify")
	return s.verifyError
}
func (s *publicationStore) PutSnapshotManifest(context.Context, objectstore.SnapshotObject, json.RawMessage) (string, error) {
	s.calls = append(s.calls, "manifest")
	return "descriptor-version", s.manifestError
}

func TestPublicationFailureDoesNotReleaseSourceOrPublishSnapshot(t *testing.T) {
	for _, stage := range []string{"verify", "manifest"} {
		t.Run(stage, func(t *testing.T) {
			r, _, _ := testRepository(t)
			ctx := context.Background()
			op := begin(t, r, Suspend, "suspend")
			upload := testUpload(t, "snap-a")
			if err := r.ReserveUpload(ctx, op, upload, 100); err != nil {
				t.Fatal(err)
			}
			if err := r.RecordPart(ctx, op, "snap-a", objectstore.SnapshotPart{Number: 1, ETag: "etag", Size: 8}); err != nil {
				t.Fatal(err)
			}
			fake := &publicationStore{}
			failure := errors.New("injected failure")
			if stage == "verify" {
				fake.verifyError = failure
			} else {
				fake.manifestError = failure
			}
			publisher, err := NewPublisher(r, fake, fake)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publisher.Publish(ctx, op, "snap-a", time.Hour); !errors.Is(err, failure) {
				t.Fatalf("missing failure: %v", err)
			}
			state, _ := r.Session(ctx, "owner", "session")
			snapshot, _ := r.Snapshot(ctx, "owner", "session", "snap-a")
			if state.State != Snapshotting || state.WorkerID != "worker-a" || state.LatestSnapshotID != "" || snapshot.Status != "uploading" {
				t.Fatalf("released sole source after failure: %+v %+v", state, snapshot)
			}
			if stage == "verify" && len(fake.calls) != 2 {
				t.Fatalf("wrote manifest before verification: %v", fake.calls)
			}
			fake.verifyError = nil
			fake.manifestError = nil
			published, err := publisher.Publish(ctx, op, "snap-a", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if published.Status != "ready" || published.ManifestVersionID != "descriptor-version" {
				t.Fatalf("not committed: %+v", published)
			}
			state, _ = r.Session(ctx, "owner", "session")
			if state.OperationID != op.ID {
				t.Fatal("source cleanup acknowledgement was skipped")
			}
			count := len(fake.calls)
			if _, err = publisher.Publish(ctx, op, "snap-a", time.Hour); err != nil || len(fake.calls) != count {
				t.Fatalf("publication retry repeated side effects: %v", err)
			}
		})
	}
}

func TestPublicationIgnoresStaleOrUncertainBackendOperation(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "suspend")
	if err := r.ReserveUpload(ctx, op, testUpload(t, "snap-a"), 100); err != nil {
		t.Fatal(err)
	}
	fake := &publicationStore{}
	publisher, _ := NewPublisher(r, fake, fake)
	if err := r.FailAndHold(ctx, op, "timeout", "source status uncertain"); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(ctx, op, "snap-a", time.Hour); !errors.Is(err, ErrConflict) || len(fake.calls) != 0 {
		t.Fatalf("uncertain operation published: %v %v", err, fake.calls)
	}
}
