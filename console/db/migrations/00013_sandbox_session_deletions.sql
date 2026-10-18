-- +goose Up
CREATE TABLE sandbox_session_deletions (
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    worker_id TEXT NOT NULL DEFAULT '',
    operation_id TEXT NOT NULL DEFAULT '',
    source_worker_id TEXT NOT NULL DEFAULT '',
    target_worker_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','succeeded')),
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error_code TEXT NOT NULL DEFAULT '',
    retry_at_unix_ms INTEGER NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    PRIMARY KEY (owner_id,session_id),
    FOREIGN KEY (owner_id,session_id) REFERENCES sandbox_persistent_sessions(owner_id,session_id)
);
CREATE INDEX idx_sandbox_session_deletion_retry ON sandbox_session_deletions(status,retry_at_unix_ms);

-- +goose Down
DROP TABLE sandbox_session_deletions;
