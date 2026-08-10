-- +goose Up
CREATE TABLE proxy_routes (
    route_key TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    scoped_session_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at_unix_ms INTEGER NOT NULL,
    expires_at_unix_ms INTEGER NOT NULL CHECK (expires_at_unix_ms > created_at_unix_ms)
);

CREATE INDEX idx_proxy_routes_owner
    ON proxy_routes(owner_id);

CREATE INDEX idx_proxy_routes_owner_session
    ON proxy_routes(owner_id, session_id);

CREATE INDEX idx_proxy_routes_expires
    ON proxy_routes(expires_at_unix_ms);

-- +goose Down
DROP INDEX IF EXISTS idx_proxy_routes_expires;
DROP INDEX IF EXISTS idx_proxy_routes_owner_session;
DROP INDEX IF EXISTS idx_proxy_routes_owner;
DROP TABLE IF EXISTS proxy_routes;
