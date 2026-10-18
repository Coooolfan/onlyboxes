package sandboxsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
)

func TestSessionPublicViewsKeepSuspendedIdentityWithoutPrivateMetadata(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()

	// Keep the same restore spec throughout the real repository lifecycle.
	op := begin(t, r, Suspend, "suspend")
	verified(t, r, op, "snapshot")
	if err := r.CommitSuspend(ctx, op, "snapshot", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, op); err != nil {
		t.Fatal(err)
	}
	view, err := r.SessionView(ctx, "owner", "session")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != Suspended || view.LeaseExpiresUnixMS != 0 || view.WorkerID != "" || view.SnapshotID != "snapshot" || view.Runtime != "boxlite" || view.Platform != "linux/arm64" || view.SnapshotSHA256 == "" || view.SnapshotSizeBytes != 8 {
		t.Fatalf("invalid suspended public view: %+v", view)
	}
	data, _ := json.Marshal(view)
	for _, private := range []string{"do-not-publish", "restore_spec", "upload_id", "bucket", "https://store.example.com"} {
		if strings.Contains(string(data), private) {
			t.Fatal("private data in public view", private)
		}
	}
	if _, err := r.SessionView(ctx, "other", "session"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-account session leaked", err)
	}
	if views, err := r.SessionViews(ctx, "other"); err != nil || len(views) != 0 {
		t.Fatal("cross-account listing leaked", err)
	}
}

func TestDeleteSessionFencesAcceptedOperationAndDurablyQueuesCleanup(t *testing.T) {
	r, db, file := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "suspend")
	verified(t, r, op, "snapshot")
	if err := r.DeleteSession(ctx, "other", "session"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-account deletion accepted", err)
	}
	if err := r.DeleteSession(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	session, err := r.Session(ctx, "owner", "session")
	if err != nil {
		t.Fatal(err)
	}
	if session.State != Deleting || session.Generation != op.Generation+1 || session.OperationID != "" {
		t.Fatal("deletion did not fence ownership", session)
	}
	if _, err := r.ExecutionRoute(ctx, session.ScopedSessionID); !errors.Is(err, ErrNotFound) {
		t.Fatal("logical deletion retained execution route", err)
	}
	if err := r.CommitSuspend(ctx, op, "snapshot", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatal("late snapshot completion revived deleted session", err)
	}
	if pending, err := r.PendingOperations(ctx); err != nil || len(pending) != 0 {
		t.Fatal("deleted operation remained dispatchable", err)
	}
	if _, err := r.Begin(ctx, request(Suspend, "suspend")); !errors.Is(err, ErrNotFound) {
		t.Fatal("old idempotency key revived a deleted session", err)
	}
	if err := r.DeleteSession(ctx, "owner", "session"); !errors.Is(err, ErrNotFound) {
		t.Fatal("repeated delete lost tombstone semantics", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, err := persistence.Open(ctx, persistence.Options{Path: file, HashKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedDB.Close()
	reopened := NewRepository(reopenedDB.SQL)
	var generation int64
	var worker, operation, source, status string
	if err := reopened.db.QueryRowContext(ctx, `SELECT generation,worker_id,operation_id,source_worker_id,status FROM sandbox_session_deletions WHERE owner_id='owner' AND session_id='session'`).Scan(&generation, &worker, &operation, &source, &status); err != nil {
		t.Fatal(err)
	}
	if generation != op.Generation || worker != "worker-a" || operation != op.ID || source != "worker-a" || status != "pending" {
		t.Fatal("cleanup identity was lost across restart", generation, worker, operation, source, status)
	}
}
