-- +goose Up
DROP TRIGGER IF EXISTS delete_proxy_routes_after_terminal_session_route;
DROP TRIGGER IF EXISTS ignore_proxy_routes_without_terminal_session;

CREATE INDEX idx_proxy_routes_scoped_session
    ON proxy_routes(scoped_session_id);

DELETE FROM proxy_routes
WHERE NOT EXISTS (
    SELECT 1
    FROM terminal_session_routes
    WHERE terminal_session_routes.scoped_session_id = proxy_routes.scoped_session_id
      AND terminal_session_routes.node_id = proxy_routes.worker_id
      AND terminal_session_routes.lease_expires_unix_ms > proxy_routes.created_at_unix_ms
);

DROP INDEX IF EXISTS idx_proxy_routes_scoped_session;
DROP INDEX IF EXISTS idx_proxy_routes_expires;
DROP INDEX IF EXISTS idx_proxy_routes_owner_session;
DROP INDEX IF EXISTS idx_proxy_routes_owner;

CREATE UNIQUE INDEX idx_terminal_session_routes_scoped_node
    ON terminal_session_routes(scoped_session_id, node_id);

CREATE TABLE proxy_routes_with_terminal_session_fk (
    route_key TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    scoped_session_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at_unix_ms INTEGER NOT NULL,
    expires_at_unix_ms INTEGER NOT NULL CHECK (expires_at_unix_ms > created_at_unix_ms),
    FOREIGN KEY (scoped_session_id, worker_id)
        REFERENCES terminal_session_routes(scoped_session_id, node_id)
        ON DELETE CASCADE
);

INSERT INTO proxy_routes_with_terminal_session_fk (
    route_key,
    owner_id,
    session_id,
    scoped_session_id,
    worker_id,
    port,
    created_at_unix_ms,
    expires_at_unix_ms
)
SELECT
    route_key,
    owner_id,
    session_id,
    scoped_session_id,
    worker_id,
    port,
    created_at_unix_ms,
    expires_at_unix_ms
FROM proxy_routes;

DROP TABLE proxy_routes;
ALTER TABLE proxy_routes_with_terminal_session_fk RENAME TO proxy_routes;

CREATE INDEX idx_proxy_routes_owner
    ON proxy_routes(owner_id);

CREATE INDEX idx_proxy_routes_owner_session
    ON proxy_routes(owner_id, session_id);

CREATE INDEX idx_proxy_routes_expires
    ON proxy_routes(expires_at_unix_ms);

CREATE INDEX idx_proxy_routes_scoped_session
    ON proxy_routes(scoped_session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_proxy_routes_scoped_session;
DROP INDEX IF EXISTS idx_proxy_routes_expires;
DROP INDEX IF EXISTS idx_proxy_routes_owner_session;
DROP INDEX IF EXISTS idx_proxy_routes_owner;

CREATE TABLE proxy_routes_without_terminal_session_fk (
    route_key TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    scoped_session_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at_unix_ms INTEGER NOT NULL,
    expires_at_unix_ms INTEGER NOT NULL CHECK (expires_at_unix_ms > created_at_unix_ms)
);

INSERT INTO proxy_routes_without_terminal_session_fk (
    route_key,
    owner_id,
    session_id,
    scoped_session_id,
    worker_id,
    port,
    created_at_unix_ms,
    expires_at_unix_ms
)
SELECT
    route_key,
    owner_id,
    session_id,
    scoped_session_id,
    worker_id,
    port,
    created_at_unix_ms,
    expires_at_unix_ms
FROM proxy_routes;

DROP TABLE proxy_routes;
ALTER TABLE proxy_routes_without_terminal_session_fk RENAME TO proxy_routes;
DROP INDEX IF EXISTS idx_terminal_session_routes_scoped_node;

CREATE INDEX idx_proxy_routes_owner
    ON proxy_routes(owner_id);

CREATE INDEX idx_proxy_routes_owner_session
    ON proxy_routes(owner_id, session_id);

CREATE INDEX idx_proxy_routes_expires
    ON proxy_routes(expires_at_unix_ms);

CREATE INDEX idx_proxy_routes_scoped_session
    ON proxy_routes(scoped_session_id);
