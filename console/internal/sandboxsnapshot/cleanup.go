package sandboxsnapshot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"
	"time"
)

type DeletionJob struct {
	OwnerID         string
	SessionID       string
	Generation      int64 // The last execution generation; the deletion fence is +1.
	Status          string
	NativeConfirmed bool
}

func (d DeletionJob) FenceGeneration() int64 { return d.Generation + 1 }
func (d DeletionJob) ID() string {
	digest := sha256.Sum256([]byte(d.OwnerID + "\x00" + d.SessionID))
	return "delete-" + hex.EncodeToString(digest[:]) + "-" + strconv.FormatInt(d.FenceGeneration(), 10)
}

type DeletionWorker struct {
	WorkerID string
	Required bool
	Done     bool
}

func (r *Repository) PendingDeletions(ctx context.Context, now time.Time, limit int) ([]DeletionJob, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id,session_id,generation,status,native_confirmed FROM sandbox_session_deletions WHERE status!='succeeded' AND retry_at_unix_ms<=? ORDER BY retry_at_unix_ms,created_at_unix_ms LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []DeletionJob
	for rows.Next() {
		var j DeletionJob
		if err := rows.Scan(&j.OwnerID, &j.SessionID, &j.Generation, &j.Status, &j.NativeConfirmed); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
func (r *Repository) DeletionWorkers(ctx context.Context, j DeletionJob) ([]DeletionWorker, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT worker_id,required,done FROM sandbox_session_deletion_workers WHERE owner_id=? AND session_id=? ORDER BY required DESC,worker_id`, j.OwnerID, j.SessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workers []DeletionWorker
	for rows.Next() {
		var w DeletionWorker
		if err := rows.Scan(&w.WorkerID, &w.Required, &w.Done); err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}
func (r *Repository) validateDeletion(ctx context.Context, tx *sql.Tx, j DeletionJob) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sandbox_session_deletions d JOIN sandbox_persistent_sessions s USING(owner_id,session_id) WHERE d.owner_id=? AND d.session_id=? AND d.generation=? AND s.state='deleting' AND s.generation=?`, j.OwnerID, j.SessionID, j.Generation, j.FenceGeneration()).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}
func (r *Repository) MarkDeletionWorkerDone(ctx context.Context, j DeletionJob, worker string) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		if err := r.validateDeletion(ctx, tx, j); err != nil {
			return err
		}
		changed, err := tx.ExecContext(ctx, `UPDATE sandbox_session_deletion_workers SET done=1,updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND worker_id=?`, r.now().UnixMilli(), j.OwnerID, j.SessionID, worker)
		return requireChanged(changed, err)
	})
}
func (r *Repository) ConfirmNativeDeletion(ctx context.Context, j DeletionJob) (bool, error) {
	confirmed := false
	err := r.transaction(ctx, func(tx *sql.Tx) error {
		if err := r.validateDeletion(ctx, tx, j); err != nil {
			return err
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_session_deletion_workers WHERE owner_id=? AND session_id=? AND required=1 AND done=0)`, j.OwnerID, j.SessionID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return nil
		}
		now := r.now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox_session_deletions SET native_confirmed=1,updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, now, j.OwnerID, j.SessionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET worker_id='',latest_snapshot_id='',retention_expires_unix_ms=0,updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, now, j.OwnerID, j.SessionID); err != nil {
			return err
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}
func (r *Repository) RetryDeletion(ctx context.Context, j DeletionJob, code string, after time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sandbox_session_deletions SET status='running',attempts=attempts+1,last_error_code=?,retry_at_unix_ms=?,updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND generation=? AND status!='succeeded'`, code, after.UnixMilli(), r.now().UnixMilli(), j.OwnerID, j.SessionID, j.Generation)
	return requireChanged(result, err)
}
func (r *Repository) FinishDeletion(ctx context.Context, j DeletionJob) (bool, error) {
	finished := false
	err := r.transaction(ctx, func(tx *sql.Tx) error {
		if err := r.validateDeletion(ctx, tx, j); err != nil {
			return err
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_session_deletion_workers WHERE owner_id=? AND session_id=? AND done=0) OR EXISTS(SELECT 1 FROM sandbox_snapshots WHERE owner_id=? AND session_id=? AND status!='deleted')`, j.OwnerID, j.SessionID, j.OwnerID, j.SessionID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE sandbox_session_deletions SET status='succeeded',last_error_code='',updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND generation=? AND native_confirmed=1`, r.now().UnixMilli(), j.OwnerID, j.SessionID, j.Generation)
		if err := requireChanged(result, err); err != nil {
			return err
		}
		finished = true
		return nil
	})
	return finished, err
}

const snapshotUnreferencedSQL = `NOT EXISTS (SELECT 1 FROM sandbox_persistent_sessions s LEFT JOIN sandbox_session_deletions d ON d.owner_id=s.owner_id AND d.session_id=s.session_id WHERE s.latest_snapshot_id=sandbox_snapshots.snapshot_id OR (s.owner_id=sandbox_snapshots.owner_id AND s.session_id=sandbox_snapshots.session_id AND (s.operation_id!='' OR (s.state='deleting' AND COALESCE(d.native_confirmed,0)=0))))`
const snapshotDeletingSessionSQL = `EXISTS (SELECT 1 FROM sandbox_persistent_sessions s JOIN sandbox_session_deletions d USING(owner_id,session_id) WHERE s.owner_id=sandbox_snapshots.owner_id AND s.session_id=sandbox_snapshots.session_id AND s.state='deleting' AND d.native_confirmed=1)`

func (r *Repository) CleanupSnapshots(ctx context.Context, olderThan, timeNow time.Time, limit int) ([]Snapshot, error) {
	return r.cleanupSnapshots(ctx, olderThan, timeNow, "", "", limit)
}
func (r *Repository) CleanupSessionSnapshots(ctx context.Context, olderThan, timeNow time.Time, owner, session string, limit int) ([]Snapshot, error) {
	return r.cleanupSnapshots(ctx, olderThan, timeNow, owner, session, limit)
}
func (r *Repository) cleanupSnapshots(ctx context.Context, olderThan, timeNow time.Time, owner, session string, limit int) ([]Snapshot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+snapshotColumns+` FROM sandbox_snapshots WHERE (?='' OR (owner_id=? AND session_id=?)) AND status!='deleted' AND cleanup_retry_at_unix_ms<=? AND (status='deleting' OR updated_at_unix_ms<? OR `+snapshotDeletingSessionSQL+`) AND `+snapshotUnreferencedSQL+` ORDER BY updated_at_unix_ms,snapshot_id LIMIT ?`, owner, owner, session, timeNow.UnixMilli(), olderThan.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}
func (r *Repository) RetrySnapshotCleanup(ctx context.Context, s Snapshot, after time.Time) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sandbox_snapshots SET cleanup_attempts=cleanup_attempts+1,last_cleanup_error='snapshot_storage_error',cleanup_retry_at_unix_ms=? WHERE snapshot_id=? AND owner_id=? AND session_id=? AND status='deleting'`, after.UnixMilli(), s.ID, s.OwnerID, s.SessionID)
	return requireChanged(result, err)
}

type ExpiredSession struct{ OwnerID, SessionID string }

func (r *Repository) ExpiredSessions(ctx context.Context, now time.Time, limit int) ([]ExpiredSession, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id,session_id FROM sandbox_persistent_sessions WHERE state='suspended' AND retention_expires_unix_ms>0 AND retention_expires_unix_ms<=? ORDER BY retention_expires_unix_ms LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []ExpiredSession
	for rows.Next() {
		var s ExpiredSession
		if err := rows.Scan(&s.OwnerID, &s.SessionID); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}
