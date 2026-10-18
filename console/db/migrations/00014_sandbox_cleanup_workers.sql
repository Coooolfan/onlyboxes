-- +goose Up
ALTER TABLE sandbox_snapshots ADD COLUMN cleanup_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_snapshots ADD COLUMN last_cleanup_error TEXT NOT NULL DEFAULT '';
ALTER TABLE sandbox_snapshots ADD COLUMN cleanup_retry_at_unix_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandbox_session_deletions ADD COLUMN native_confirmed INTEGER NOT NULL DEFAULT 0 CHECK (native_confirmed IN (0,1));
CREATE TABLE sandbox_session_deletion_workers (
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    required INTEGER NOT NULL CHECK (required IN (0,1)),
    done INTEGER NOT NULL DEFAULT 0 CHECK (done IN (0,1)),
    updated_at_unix_ms INTEGER NOT NULL,
    PRIMARY KEY (owner_id,session_id,worker_id),
    FOREIGN KEY (owner_id,session_id) REFERENCES sandbox_session_deletions(owner_id,session_id)
);
INSERT INTO sandbox_session_deletion_workers(owner_id,session_id,worker_id,required,updated_at_unix_ms)
SELECT owner_id,session_id,worker_id,MAX(required),MAX(updated_at_unix_ms) FROM (
 SELECT owner_id,session_id,worker_id,1 AS required,updated_at_unix_ms FROM sandbox_session_deletions
 UNION ALL SELECT owner_id,session_id,source_worker_id,1,updated_at_unix_ms FROM sandbox_session_deletions
 UNION ALL SELECT owner_id,session_id,target_worker_id,1,updated_at_unix_ms FROM sandbox_session_deletions
 UNION ALL SELECT o.owner_id,o.session_id,o.source_worker_id,0,d.updated_at_unix_ms FROM sandbox_persistence_operations o JOIN sandbox_session_deletions d USING(owner_id,session_id)
 UNION ALL SELECT o.owner_id,o.session_id,o.target_worker_id,0,d.updated_at_unix_ms FROM sandbox_persistence_operations o JOIN sandbox_session_deletions d USING(owner_id,session_id)
) WHERE worker_id!='' GROUP BY owner_id,session_id,worker_id;

-- +goose Down
ALTER TABLE sandbox_snapshots DROP COLUMN cleanup_retry_at_unix_ms;
ALTER TABLE sandbox_snapshots DROP COLUMN last_cleanup_error;
ALTER TABLE sandbox_snapshots DROP COLUMN cleanup_attempts;
DROP TABLE sandbox_session_deletion_workers;
ALTER TABLE sandbox_session_deletions DROP COLUMN native_confirmed;
