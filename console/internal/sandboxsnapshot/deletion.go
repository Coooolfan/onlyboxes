package sandboxsnapshot

import (
	"context"
	"database/sql"
	"math"
)

// DeleteSession atomically hides the stable session, revokes its execution
// route, fences previously accepted controls and records durable cleanup work.
// Snapshot references remain intact until cleanup has confirmed native removal.
func (r *Repository) DeleteSession(ctx context.Context, owner, id string) error {
	return r.deleteSession(ctx, owner, id, false)
}
func (r *Repository) DeleteExpiredSession(ctx context.Context, owner, id string) error {
	return r.deleteSession(ctx, owner, id, true)
}
func (r *Repository) deleteSession(ctx context.Context, owner, id string, expired bool) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := sessionIn(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		if s.Generation == math.MaxInt64 {
			return ErrConflict
		}
		if s.State == Deleting {
			return ErrNotFound
		}
		if expired && (s.State != Suspended || s.RetentionExpiresUnixMS <= 0 || s.RetentionExpiresUnixMS > r.now().UnixMilli()) {
			return ErrConflict
		}
		var source, target string
		if s.OperationID != "" {
			op, err := operationIn(ctx, tx, owner, id, s.OperationID)
			if err != nil {
				return err
			}
			source, target = op.SourceWorkerID, op.TargetWorkerID
		}
		now := r.now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_session_deletions (owner_id,session_id,generation,worker_id,operation_id,source_worker_id,target_worker_id,retry_at_unix_ms,created_at_unix_ms,updated_at_unix_ms) VALUES (?,?,?,?,?,?,?,?,?,?)`, owner, id, s.Generation, s.WorkerID, s.OperationID, source, target, now, now, now); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_session_deletion_workers(owner_id,session_id,worker_id,required,updated_at_unix_ms)
SELECT ?,?,worker_id,MAX(required),? FROM (
 SELECT ? AS worker_id,1 AS required UNION ALL SELECT ?,1 UNION ALL SELECT ?,1
 UNION ALL SELECT source_worker_id,0 FROM sandbox_persistence_operations WHERE owner_id=? AND session_id=?
 UNION ALL SELECT target_worker_id,0 FROM sandbox_persistence_operations WHERE owner_id=? AND session_id=?
) WHERE worker_id!='' GROUP BY worker_id`, owner, id, now, s.WorkerID, source, target, owner, id, owner, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET status='canceled',phase='cleaning',error_code='',error_message='',updated_at_unix_ms=? WHERE owner_id=? AND session_id=? AND operation_id=?`, now, owner, id, s.OperationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox_persistent_sessions SET state='deleting',generation=generation+1,operation_id='',updated_at_unix_ms=? WHERE owner_id=? AND session_id=?`, now, owner, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM terminal_session_routes WHERE scoped_session_id=?`, s.ScopedSessionID)
		return err
	})
}
