package sandboxsnapshot

import (
	"context"
	"database/sql"
)

type CleanupStats struct {
	PendingSessions     int   `json:"pending_sessions"`
	PendingWorkers      int   `json:"pending_workers"`
	PendingSnapshots    int   `json:"pending_snapshots"`
	FailedSessions      int   `json:"failed_sessions"`
	FailedSnapshots     int   `json:"failed_snapshots"`
	RetryAttempts       int64 `json:"retry_attempts"`
	OldestPendingUnixMS int64 `json:"-"`
}

func (r *Repository) CleanupStats(ctx context.Context, owner string) (CleanupStats, error) {
	var stats CleanupStats
	err := r.transaction(ctx, func(tx *sql.Tx) error {
		var attempts int64
		var jobOldest, snapshotOldest int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status!='succeeded'),0),COALESCE(SUM(status!='succeeded' AND last_error_code!=''),0),COALESCE(SUM(attempts),0),COALESCE(MIN(CASE WHEN status!='succeeded' THEN created_at_unix_ms END),0) FROM sandbox_session_deletions WHERE (?='' OR owner_id=?)`, owner, owner).Scan(&stats.PendingSessions, &stats.FailedSessions, &stats.RetryAttempts, &jobOldest); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sandbox_session_deletion_workers WHERE done=0 AND (?='' OR owner_id=?)`, owner, owner).Scan(&stats.PendingWorkers); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='deleting' OR (status!='deleted' AND EXISTS (SELECT 1 FROM sandbox_persistent_sessions s WHERE s.owner_id=sandbox_snapshots.owner_id AND s.session_id=sandbox_snapshots.session_id AND s.state='deleting'))),0),COALESCE(SUM(status='deleting' AND last_cleanup_error!=''),0),COALESCE(SUM(cleanup_attempts),0),COALESCE(MIN(CASE WHEN status='deleting' THEN updated_at_unix_ms END),0) FROM sandbox_snapshots WHERE (?='' OR owner_id=?)`, owner, owner).Scan(&stats.PendingSnapshots, &stats.FailedSnapshots, &attempts, &snapshotOldest); err != nil {
			return err
		}
		stats.RetryAttempts += attempts
		stats.OldestPendingUnixMS = jobOldest
		if snapshotOldest > 0 && (jobOldest == 0 || snapshotOldest < jobOldest) {
			stats.OldestPendingUnixMS = snapshotOldest
		}
		return nil
	})
	return stats, err
}
