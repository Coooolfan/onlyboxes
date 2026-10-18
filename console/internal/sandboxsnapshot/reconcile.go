package sandboxsnapshot

import (
	"context"
	"database/sql"
)

// ReconcileActivation resumes the committed activation phase after the worker
// confirms that the same operation/generation is already active. It never
// creates an instance or opens execution; Finish still requires a live route.
func (r *Repository) ReconcileActivation(ctx context.Context, op Operation) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		session, err := sessionIn(ctx, tx, op.OwnerID, op.SessionID)
		if err != nil {
			return err
		}
		current, err := operationIn(ctx, tx, op.OwnerID, op.SessionID, op.ID)
		if err != nil {
			return err
		}
		if current.Kind != Resume || current.Status != "failed" || current.Phase != "reconciling" || current.Generation != op.Generation || session.OperationID != op.ID || session.Generation != op.Generation || session.State != Running || session.WorkerID != current.TargetWorkerID {
			return ErrConflict
		}
		var lease int64
		if err = tx.QueryRowContext(ctx, `SELECT lease_expires_unix_ms FROM terminal_session_routes WHERE scoped_session_id=? AND node_id=?`, session.ScopedSessionID, session.WorkerID).Scan(&lease); err != nil {
			return err
		}
		if lease <= r.now().UnixMilli() {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE sandbox_persistence_operations SET status='running',phase='activating',error_code='',error_message='',updated_at_unix_ms=? WHERE operation_id=?`, r.now().UnixMilli(), op.ID)
		return err
	})
}
