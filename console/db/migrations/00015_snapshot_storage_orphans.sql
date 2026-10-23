-- +goose Up
CREATE TABLE sandbox_snapshot_storage_orphans (
    snapshot_id TEXT PRIMARY KEY,
    store_id TEXT NOT NULL,
    archive_key TEXT NOT NULL,
    first_seen_unix_ms INTEGER NOT NULL,
    modified_unix_ms INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('observing','deleting','deleted')),
    retry_at_unix_ms INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_snapshot_storage_orphan_cleanup
    ON sandbox_snapshot_storage_orphans(status,retry_at_unix_ms,first_seen_unix_ms);

-- +goose StatementBegin
CREATE TRIGGER sandbox_snapshot_orphan_admission
BEFORE INSERT ON sandbox_snapshots
WHEN EXISTS (SELECT 1 FROM sandbox_snapshot_storage_orphans o
             WHERE o.snapshot_id=NEW.snapshot_id AND o.status!='observing')
BEGIN
    SELECT RAISE(ABORT,'snapshot ID is fenced for orphan collection');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER sandbox_snapshot_orphan_admission;
DROP TABLE sandbox_snapshot_storage_orphans;
