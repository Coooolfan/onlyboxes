package sandboxsnapshot

import (
	"context"
	"database/sql"
	"time"

	"github.com/onlyboxes/onlyboxes/console/internal/objectstore"
)

// ObserveStorageOrphan records only namespace-validated listing entries. Known
// snapshots, including deleted ones and active uploads, belong to ordinary GC.
func (r *Repository) ObserveStorageOrphan(ctx context.Context, entry objectstore.SnapshotOrphanEntry, now time.Time) error {
	if entry.StoreID == "" || entry.SnapshotID == "" || entry.ArchiveKey == "" || entry.Modified.IsZero() || entry.Modified.After(now) {
		return ErrConflict
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO sandbox_snapshot_storage_orphans(snapshot_id,store_id,archive_key,first_seen_unix_ms,modified_unix_ms,status)
SELECT ?,?,?,?,?,'observing' WHERE NOT EXISTS (SELECT 1 FROM sandbox_snapshots WHERE snapshot_id=?)
ON CONFLICT(snapshot_id) DO UPDATE SET modified_unix_ms=MAX(modified_unix_ms,excluded.modified_unix_ms)
WHERE status='observing' AND store_id=excluded.store_id AND archive_key=excluded.archive_key`, entry.SnapshotID, entry.StoreID, entry.ArchiveKey, now.UnixMilli(), entry.Modified.UnixMilli(), entry.SnapshotID)
	return err
}

// ClaimStorageOrphans serializes deletion fencing with ReserveUpload. A candidate
// needs both an old listing timestamp and a full locally observed grace period.
// Once deleting, the ID is permanently closed to new upload admission.
func (r *Repository) ClaimStorageOrphans(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]objectstore.SnapshotOrphan, error) {
	if grace <= 0 || limit < 1 || limit > 100 {
		return nil, ErrConflict
	}
	var candidates []objectstore.SnapshotOrphan
	err := r.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_snapshot_storage_orphans WHERE status='observing' AND EXISTS(SELECT 1 FROM sandbox_snapshots s WHERE s.snapshot_id=sandbox_snapshot_storage_orphans.snapshot_id)`); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT store_id,snapshot_id,archive_key FROM sandbox_snapshot_storage_orphans WHERE status!='deleted' AND retry_at_unix_ms<=? AND (status='deleting' OR (first_seen_unix_ms<=? AND modified_unix_ms<=?)) AND NOT EXISTS(SELECT 1 FROM sandbox_snapshots s WHERE s.snapshot_id=sandbox_snapshot_storage_orphans.snapshot_id) ORDER BY retry_at_unix_ms,first_seen_unix_ms,snapshot_id LIMIT ?`, now.UnixMilli(), now.Add(-grace).UnixMilli(), now.Add(-grace).UnixMilli(), limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var candidate objectstore.SnapshotOrphan
			if err := rows.Scan(&candidate.StoreID, &candidate.SnapshotID, &candidate.ArchiveKey); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			if _, err := tx.ExecContext(ctx, `UPDATE sandbox_snapshot_storage_orphans SET status='deleting' WHERE snapshot_id=?`, candidate.SnapshotID); err != nil {
				return err
			}
		}
		return nil
	})
	return candidates, err
}

func (r *Repository) FinishStorageOrphan(ctx context.Context, candidate objectstore.SnapshotOrphan, deleted bool, retryAt time.Time) error {
	status := "deleting"
	if deleted {
		status = "deleted"
	}
	result, err := r.db.ExecContext(ctx, `UPDATE sandbox_snapshot_storage_orphans SET status=?,attempts=attempts+1,retry_at_unix_ms=? WHERE snapshot_id=? AND store_id=? AND archive_key=? AND status='deleting'`, status, retryAt.UnixMilli(), candidate.SnapshotID, candidate.StoreID, candidate.ArchiveKey)
	return requireChanged(result, err)
}
