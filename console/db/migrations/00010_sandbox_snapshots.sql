-- +goose Up
CREATE TABLE sandbox_persistent_sessions (
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running','snapshotting','suspended','restoring','deleting')),
    generation INTEGER NOT NULL CHECK (generation > 0),
    restore_spec_json TEXT NOT NULL,
    worker_id TEXT NOT NULL DEFAULT '',
    latest_snapshot_id TEXT NOT NULL DEFAULT '',
    operation_id TEXT NOT NULL DEFAULT '',
    retention_expires_unix_ms INTEGER NOT NULL DEFAULT 0,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    PRIMARY KEY (owner_id, session_id)
);

CREATE TABLE sandbox_persistence_operations (
    operation_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('suspend','resume')),
    idempotency_key TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    status TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','failed','canceled')),
    phase TEXT NOT NULL,
    source_worker_id TEXT NOT NULL DEFAULT '',
    target_worker_id TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    deadline_unix_ms INTEGER NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    UNIQUE (owner_id, session_id, kind, idempotency_key),
    FOREIGN KEY (owner_id, session_id) REFERENCES sandbox_persistent_sessions(owner_id, session_id)
);
CREATE UNIQUE INDEX idx_sandbox_operation_active
    ON sandbox_persistence_operations(owner_id, session_id)
    WHERE status IN ('pending','running');
CREATE INDEX idx_sandbox_operation_recovery
    ON sandbox_persistence_operations(status, updated_at_unix_ms);

CREATE TABLE sandbox_persistence_requests (
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    operation_id TEXT NOT NULL REFERENCES sandbox_persistence_operations(operation_id),
    PRIMARY KEY (owner_id, session_id, kind, idempotency_key)
);

CREATE TABLE sandbox_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    operation_id TEXT NOT NULL UNIQUE REFERENCES sandbox_persistence_operations(operation_id),
    generation INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('uploading','verified','ready','deleting','deleted')),
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0),
    upload_json TEXT NOT NULL,
    manifest_json TEXT NOT NULL DEFAULT '',
    manifest_version_id TEXT NOT NULL DEFAULT '',
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    FOREIGN KEY (owner_id, session_id) REFERENCES sandbox_persistent_sessions(owner_id, session_id)
);
CREATE INDEX idx_sandbox_snapshot_quota ON sandbox_snapshots(owner_id, status);
CREATE INDEX idx_sandbox_snapshot_gc ON sandbox_snapshots(status, updated_at_unix_ms);

CREATE TABLE sandbox_snapshot_parts (
    snapshot_id TEXT NOT NULL REFERENCES sandbox_snapshots(snapshot_id),
    part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    etag TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0),
    PRIMARY KEY (snapshot_id, part_number)
);

-- +goose Down
DROP TABLE sandbox_snapshot_parts;
DROP TABLE sandbox_snapshots;
DROP TABLE sandbox_persistence_requests;
DROP TABLE sandbox_persistence_operations;
DROP TABLE sandbox_persistent_sessions;
