package sandboxsnapshot

import (
	"context"
	"database/sql"
	"errors"
)

// SessionView contains only public lifecycle metadata. Restore specifications,
// object keys, credentials and transfer grants are deliberately excluded.
type SessionView struct {
	OwnerID                 string
	SessionID               string
	ScopedSessionID         string
	State                   State
	Generation              int64
	WorkerID                string
	OperationID             string
	Runtime                 string
	Platform                string
	RetentionExpiresUnixMS  int64
	CreatedAtUnixMS         int64
	UpdatedAtUnixMS         int64
	LeaseExpiresUnixMS      int64
	LastUsedUnixMS          int64
	SnapshotID              string
	SnapshotCreatedAtUnixMS int64
	SnapshotSizeBytes       int64
	SnapshotSHA256          string
}

const sessionViewQuery = `SELECT s.owner_id,s.session_id,s.scoped_session_id,s.state,s.generation,s.worker_id,s.operation_id,
json_extract(s.restore_spec_json,'$.runtime'),json_extract(s.restore_spec_json,'$.platform'),
s.retention_expires_unix_ms,s.created_at_unix_ms,s.updated_at_unix_ms,
CASE WHEN s.state='running' AND r.node_id=s.worker_id THEN COALESCE(r.lease_expires_unix_ms,0) ELSE 0 END,
COALESCE(r.last_used_unix_ms,s.updated_at_unix_ms),COALESCE(p.snapshot_id,''),COALESCE(p.created_at_unix_ms,0),COALESCE(p.size_bytes,0),COALESCE(json_extract(p.upload_json,'$.object.sha256'),'')
FROM sandbox_persistent_sessions s
LEFT JOIN terminal_session_routes r ON r.scoped_session_id=s.scoped_session_id
LEFT JOIN sandbox_snapshots p ON p.snapshot_id=s.latest_snapshot_id AND p.owner_id=s.owner_id AND p.session_id=s.session_id AND p.status='ready'`

func scanSessionView(row rowScanner) (SessionView, error) {
	var v SessionView
	err := row.Scan(&v.OwnerID, &v.SessionID, &v.ScopedSessionID, &v.State, &v.Generation, &v.WorkerID, &v.OperationID, &v.Runtime, &v.Platform, &v.RetentionExpiresUnixMS, &v.CreatedAtUnixMS, &v.UpdatedAtUnixMS, &v.LeaseExpiresUnixMS, &v.LastUsedUnixMS, &v.SnapshotID, &v.SnapshotCreatedAtUnixMS, &v.SnapshotSizeBytes, &v.SnapshotSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

// SessionViews includes tombstones so callers can suppress stale route caches.
func (r *Repository) SessionViews(ctx context.Context, owner string) ([]SessionView, error) {
	rows, err := r.db.QueryContext(ctx, sessionViewQuery+` WHERE (?='' OR s.owner_id=?) ORDER BY s.created_at_unix_ms DESC,s.owner_id,s.session_id`, owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := make([]SessionView, 0)
	for rows.Next() {
		v, err := scanSessionView(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}
func (r *Repository) SessionView(ctx context.Context, owner, id string) (SessionView, error) {
	return scanSessionView(r.db.QueryRowContext(ctx, sessionViewQuery+` WHERE s.owner_id=? AND s.session_id=?`, owner, id))
}
