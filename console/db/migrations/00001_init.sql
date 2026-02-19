-- +goose Up
CREATE TABLE worker_nodes (
    node_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL DEFAULT '',
    provisioned INTEGER NOT NULL DEFAULT 0 CHECK (provisioned IN (0, 1)),
    node_name TEXT NOT NULL DEFAULT '',
    executor_kind TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    registered_at_unix_ms INTEGER NOT NULL,
    last_seen_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX idx_worker_nodes_last_seen
    ON worker_nodes(last_seen_at_unix_ms);

CREATE INDEX idx_worker_nodes_status_probe
    ON worker_nodes(session_id, last_seen_at_unix_ms);

CREATE TABLE worker_capabilities (
    node_id TEXT NOT NULL,
    capability_name TEXT NOT NULL,
    max_inflight INTEGER NOT NULL,
    PRIMARY KEY (node_id, capability_name),
    FOREIGN KEY (node_id) REFERENCES worker_nodes(node_id) ON DELETE CASCADE
);

CREATE INDEX idx_worker_caps_lookup
    ON worker_capabilities(capability_name, node_id);

CREATE TABLE worker_labels (
    node_id TEXT NOT NULL,
    label_key TEXT NOT NULL,
    label_value TEXT NOT NULL,
    PRIMARY KEY (node_id, label_key),
    FOREIGN KEY (node_id) REFERENCES worker_nodes(node_id) ON DELETE CASCADE
);

CREATE TABLE worker_credentials (
    node_id TEXT PRIMARY KEY,
    secret_hash TEXT NOT NULL,
    hash_algo TEXT NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    FOREIGN KEY (node_id) REFERENCES worker_nodes(node_id) ON DELETE CASCADE
);

CREATE TABLE trusted_tokens (
    token_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    token_masked TEXT NOT NULL,
    generated INTEGER NOT NULL CHECK (generated IN (0, 1)),
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    UNIQUE (name_key),
    UNIQUE (token_hash)
);

CREATE INDEX idx_trusted_tokens_created
    ON trusted_tokens(created_at_unix_ms);

CREATE TABLE tasks (
    task_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    capability TEXT NOT NULL,
    input_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('queued', 'dispatched', 'running', 'succeeded', 'failed', 'timeout', 'canceled')
    ),
    command_id TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    deadline_at_unix_ms INTEGER NOT NULL,
    completed_at_unix_ms INTEGER NOT NULL DEFAULT 0,
    expires_at_unix_ms INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX idx_tasks_owner_request_unique
    ON tasks(owner_id, request_id)
    WHERE request_id <> '';

CREATE INDEX idx_tasks_owner_created
    ON tasks(owner_id, created_at_unix_ms DESC);

CREATE INDEX idx_tasks_expires
    ON tasks(expires_at_unix_ms);

CREATE INDEX idx_tasks_status
    ON tasks(status);

-- +goose Down
DROP INDEX IF EXISTS idx_tasks_status;
DROP INDEX IF EXISTS idx_tasks_expires;
DROP INDEX IF EXISTS idx_tasks_owner_created;
DROP INDEX IF EXISTS idx_tasks_owner_request_unique;
DROP TABLE IF EXISTS tasks;

DROP INDEX IF EXISTS idx_trusted_tokens_created;
DROP TABLE IF EXISTS trusted_tokens;

DROP TABLE IF EXISTS worker_credentials;
DROP TABLE IF EXISTS worker_labels;
DROP INDEX IF EXISTS idx_worker_caps_lookup;
DROP TABLE IF EXISTS worker_capabilities;
DROP INDEX IF EXISTS idx_worker_nodes_status_probe;
DROP INDEX IF EXISTS idx_worker_nodes_last_seen;
DROP TABLE IF EXISTS worker_nodes;
