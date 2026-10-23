package sandboxsnapshot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
)

func orphanEntry(t *testing.T, id string, modified time.Time) objectstore.SnapshotOrphanEntry {
	t.Helper()
	object := testUpload(t, id).Object
	return objectstore.SnapshotOrphanEntry{SnapshotOrphan: objectstore.SnapshotOrphan{StoreID: object.StoreID, SnapshotID: object.SnapshotID, ArchiveKey: object.Key}, Modified: modified}
}

func TestStorageOrphanGraceFenceRetryAndRestart(t *testing.T) {
	r, db, file := testRepository(t)
	ctx := context.Background()
	now := time.Now()
	entry := orphanEntry(t, "lost", now.Add(-48*time.Hour))
	if err := r.ObserveStorageOrphan(ctx, entry, now); err != nil {
		t.Fatal(err)
	}
	if jobs, err := r.ClaimStorageOrphans(ctx, now, time.Hour, 10); err != nil || len(jobs) != 0 {
		t.Fatal("provider age bypassed observation grace", jobs, err)
	}
	// A replaced object restarts the modification grace, without losing first seen.
	entry.Modified = now.Add(30 * time.Minute)
	if err := r.ObserveStorageOrphan(ctx, entry, now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if jobs, err := r.ClaimStorageOrphans(ctx, now.Add(time.Hour), time.Hour, 10); err != nil || len(jobs) != 0 {
		t.Fatal("recent modification collected", jobs, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := persistence.Open(ctx, persistence.Options{Path: file, HashKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r = NewRepository(db.SQL)
	if required, err := r.RequiresStore(ctx); err != nil || !required {
		t.Fatal("orphan queue abandoned on disable", required, err)
	}
	if err := r.ValidateStore(ctx, "other"); err == nil {
		t.Fatal("orphan rebound to foreign store")
	}
	at := now.Add(2 * time.Hour)
	jobs, err := r.ClaimStorageOrphans(ctx, at, time.Hour, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	op := begin(t, r, Suspend, "suspend")
	if err := r.ReserveUpload(ctx, op, testUpload(t, "lost"), 100); !errors.Is(err, ErrConflict) {
		t.Fatal("late upload bypassed deletion fence", err)
	}
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO sandbox_snapshots(snapshot_id,owner_id,session_id,operation_id,generation,status,size_bytes,upload_json,created_at_unix_ms,updated_at_unix_ms) VALUES ('lost','owner','session',?,?,'uploading',8,'{}',?,?)`, op.ID, op.Generation, at.UnixMilli(), at.UnixMilli()); err == nil || !strings.Contains(err.Error(), "fenced for orphan collection") {
		t.Fatal("database trigger did not enforce admission fence", err)
	}
	if err := r.FinishStorageOrphan(ctx, jobs[0], false, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.ClaimStorageOrphans(ctx, at, time.Hour, 10); err != nil || len(pending) != 0 {
		t.Fatal("retry delay ignored", pending, err)
	}
	r = NewRepository(db.SQL)
	jobs, err = r.ClaimStorageOrphans(ctx, at.Add(time.Minute), time.Hour, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal("failed purge lost after reconstruction", jobs, err)
	}
	if err := r.FinishStorageOrphan(ctx, jobs[0], true, at); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.ClaimStorageOrphans(ctx, at.Add(2*time.Hour), time.Hour, 10); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if err := r.ReserveUpload(ctx, op, testUpload(t, "lost"), 100); !errors.Is(err, ErrConflict) {
		t.Fatal("finished orphan ID reopened", err)
	}
}

func TestStorageOrphanAdmissionWinsBeforeFence(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	now := time.Now()
	entry := orphanEntry(t, "inflight", now.Add(-48*time.Hour))
	if err := r.ObserveStorageOrphan(ctx, entry, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	op := begin(t, r, Suspend, "suspend")
	if err := r.ReserveUpload(ctx, op, testUpload(t, "inflight"), 100); err != nil {
		t.Fatal(err)
	}
	if err := r.ObserveStorageOrphan(ctx, entry, now); err != nil {
		t.Fatal(err)
	}
	if jobs, err := r.ClaimStorageOrphans(ctx, now, time.Hour, 10); err != nil || len(jobs) != 0 {
		t.Fatal("registered upload collected", jobs, err)
	}
}

func TestStorageOrphanClaimRacesUploadReservation(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		r, _, _ := testRepository(t)
		ctx := context.Background()
		now := time.Now()
		entry := orphanEntry(t, "racing", now.Add(-48*time.Hour))
		if err := r.ObserveStorageOrphan(ctx, entry, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		op := begin(t, r, Suspend, "suspend")
		var wg sync.WaitGroup
		wg.Add(2)
		var reserveErr, claimErr error
		var jobs []objectstore.SnapshotOrphan
		go func() { defer wg.Done(); reserveErr = r.ReserveUpload(ctx, op, testUpload(t, "racing"), 100) }()
		go func() { defer wg.Done(); jobs, claimErr = r.ClaimStorageOrphans(ctx, now, time.Hour, 10) }()
		wg.Wait()
		if claimErr != nil || (reserveErr != nil && !errors.Is(reserveErr, ErrConflict)) {
			t.Fatal(reserveErr, claimErr)
		}
		if (reserveErr == nil) == (len(jobs) != 0) {
			t.Fatal("admission and collection must have exactly one winner", reserveErr, jobs)
		}
	}
}
