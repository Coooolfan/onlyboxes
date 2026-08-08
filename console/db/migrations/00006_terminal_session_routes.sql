-- +goose Up
CREATE TABLE terminal_session_routes (
    scoped_session_id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    lease_expires_unix_ms INTEGER NOT NULL CHECK (lease_expires_unix_ms > 0),
    last_used_unix_ms INTEGER NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX idx_terminal_session_routes_node
    ON terminal_session_routes(node_id);

CREATE INDEX idx_terminal_session_routes_lease
    ON terminal_session_routes(lease_expires_unix_ms);

-- +goose Down
DROP INDEX IF EXISTS idx_terminal_session_routes_lease;
DROP INDEX IF EXISTS idx_terminal_session_routes_node;
DROP TABLE IF EXISTS terminal_session_routes;
