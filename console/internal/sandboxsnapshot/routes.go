package sandboxsnapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type ExecutionRoute struct {
	WorkerID           string
	LeaseExpiresUnixMS int64
	LastUsedUnixMS     int64
	CreatedAtUnixMS    int64
}

func (r *Repository) ExecutionRoute(ctx context.Context, scopedID string) (ExecutionRoute, error) {
	var route ExecutionRoute
	err := r.db.QueryRowContext(ctx, `SELECT node_id,lease_expires_unix_ms,last_used_unix_ms,created_at_unix_ms FROM terminal_session_routes WHERE scoped_session_id=?`, scopedID).Scan(&route.WorkerID, &route.LeaseExpiresUnixMS, &route.LastUsedUnixMS, &route.CreatedAtUnixMS)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return route, err
}

// AdoptRunning atomically opts an existing confirmed live terminal route into
// persistence. A stale route or an already-owned lifecycle cannot be replaced.
func (r *Repository) AdoptRunning(ctx context.Context, owner, id, worker string, spec RestoreSpec) error {
	if owner == "" || id == "" || worker == "" {
		return ErrConflict
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		scopedID := ScopeSessionID(owner, id)
		var node string
		var lease int64
		if err := tx.QueryRowContext(ctx, `SELECT node_id,lease_expires_unix_ms FROM terminal_session_routes WHERE scoped_session_id=?`, scopedID).Scan(&node, &lease); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		now := r.now().UnixMilli()
		if node != worker || lease <= now {
			return ErrConflict
		}
		prior, err := sessionIn(ctx, tx, owner, id)
		if err == nil {
			if prior.State != Running || prior.OperationID != "" || prior.WorkerID != worker || !sameSpec(prior.Spec, spec) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		payload, _ := json.Marshal(spec)
		_, err = tx.ExecContext(ctx, `INSERT INTO sandbox_persistent_sessions (`+sessionColumns+`) VALUES (?,?,'running',1,?,?,'','',0,?,?,?)`, owner, id, string(payload), worker, now, now, scopedID)
		return err
	})
}

func removeExecutionRoute(ctx context.Context, tx *sql.Tx, scopedID, expectedWorker string) error {
	var worker string
	err := tx.QueryRowContext(ctx, `SELECT node_id FROM terminal_session_routes WHERE scoped_session_id=?`, scopedID).Scan(&worker)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if worker != expectedWorker {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM terminal_session_routes WHERE scoped_session_id=? AND node_id=?`, scopedID, worker)
	return err
}

func publishExecutionRoute(ctx context.Context, tx *sql.Tx, session Session, worker string, lease, now int64) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO terminal_session_routes (scoped_session_id,node_id,lease_expires_unix_ms,last_used_unix_ms,created_at_unix_ms,updated_at_unix_ms) VALUES (?,?,?,?,?,?)
ON CONFLICT(scoped_session_id) DO UPDATE SET lease_expires_unix_ms=MAX(terminal_session_routes.lease_expires_unix_ms,excluded.lease_expires_unix_ms),last_used_unix_ms=excluded.last_used_unix_ms,updated_at_unix_ms=excluded.updated_at_unix_ms WHERE terminal_session_routes.node_id=excluded.node_id`, session.ScopedSessionID, worker, lease, now, session.CreatedAtUnixMS, now)
	if err = requireChanged(result, err); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE proxy_routes SET worker_id=? WHERE owner_id=? AND session_id=? AND scoped_session_id=?`, worker, session.OwnerID, session.SessionID, session.ScopedSessionID)
	return err
}
