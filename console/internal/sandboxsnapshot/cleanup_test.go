package sandboxsnapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/persistence"
)

func TestCleanupNativeProofProtectsUncommittedArchiveAndSurvivesRestart(t *testing.T) {
	r, db, file := testRepository(t)
	ctx := context.Background()
	op := begin(t, r, Suspend, "suspend")
	verified(t, r, op, "snapshot")
	if err := r.DeleteSession(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	jobs, err := r.PendingDeletions(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatal(err)
	}
	job := jobs[0]
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snapshot", time.Now().Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatal("uncommitted sole archive was collectible before native proof", err)
	}
	if ready, err := r.ConfirmNativeDeletion(ctx, job); err != nil || ready {
		t.Fatal("required source cleanup was not enforced", ready, err)
	}
	if err := r.MarkDeletionWorkerDone(ctx, job, "worker-a"); err != nil {
		t.Fatal(err)
	}
	if ready, err := r.ConfirmNativeDeletion(ctx, job); err != nil || !ready {
		t.Fatal("source deletion was not confirmed", ready, err)
	}
	// Explicit deletion can reclaim a new upload without waiting GC age grace.
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snapshot", time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	if err := r.RetrySnapshotCleanup(ctx, Snapshot{ID: "snapshot", OwnerID: "owner", SessionID: "session"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	stats, err := r.CleanupStats(ctx, "owner")
	if err != nil || stats.PendingSessions != 1 || stats.PendingSnapshots != 1 || stats.FailedSnapshots != 1 || stats.RetryAttempts != 1 {
		t.Fatal("cleanup error/backlog was not durable", stats, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := persistence.Open(ctx, persistence.Options{Path: file, HashKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r = NewRepository(reopened.SQL)
	workers, err := r.DeletionWorkers(ctx, job)
	if err != nil || len(workers) != 1 || !workers[0].Done {
		t.Fatal("native acknowledgement was lost across restart", err)
	}
	if err := r.MarkSnapshotDeleted(ctx, "owner", "session", "snapshot"); err != nil {
		t.Fatal(err)
	}
	if finished, err := r.FinishDeletion(ctx, job); err != nil || !finished {
		t.Fatal("cleanup did not finish", finished, err)
	}
	stats, err = r.CleanupStats(ctx, "owner")
	if err != nil || stats.PendingSessions != 0 || stats.PendingWorkers != 0 || stats.PendingSnapshots != 0 || stats.FailedSnapshots != 0 {
		t.Fatal("completed cleanup remained failed", stats, err)
	}
	other, err := r.CleanupStats(ctx, "another-owner")
	if err != nil || other.RetryAttempts != 0 {
		t.Fatal("cleanup statistics leaked account data", other, err)
	}
}
func TestCleanupExpiryDoesNotRevokeAnAdmittedRestore(t *testing.T) {
	r, _, _ := testRepository(t)
	ctx := context.Background()
	suspend(t, r, "suspend", "snapshot")
	begin(t, r, Resume, "resume")
	future := time.Now().Add(2 * time.Hour)
	r.now = func() time.Time { return future }
	if err := r.DeleteExpiredSession(ctx, "owner", "session"); !errors.Is(err, ErrConflict) {
		t.Fatal("retention expiry revoked an admitted restore", err)
	}
	if err := r.TombstoneSnapshot(ctx, "owner", "session", "snapshot", future); !errors.Is(err, ErrConflict) {
		t.Fatal("GC removed a restore reference", err)
	}
	if expired, err := r.ExpiredSessions(ctx, future, 10); err != nil || len(expired) != 0 {
		t.Fatal("restoring session was selected for expiry", err)
	}
}
