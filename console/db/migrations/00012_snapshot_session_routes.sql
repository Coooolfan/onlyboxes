-- +goose Up
ALTER TABLE sandbox_persistent_sessions ADD COLUMN scoped_session_id TEXT NOT NULL DEFAULT '';
UPDATE sandbox_persistent_sessions SET scoped_session_id = 'obx:' || owner_id || ':' || session_id;
CREATE UNIQUE INDEX idx_sandbox_session_scope ON sandbox_persistent_sessions(scoped_session_id);

CREATE TABLE proxy_routes_durable (
    route_key TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    scoped_session_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at_unix_ms INTEGER NOT NULL,
    expires_at_unix_ms INTEGER NOT NULL CHECK (expires_at_unix_ms > created_at_unix_ms)
);
INSERT INTO proxy_routes_durable SELECT * FROM proxy_routes;
DROP TABLE proxy_routes;
ALTER TABLE proxy_routes_durable RENAME TO proxy_routes;
CREATE INDEX idx_proxy_routes_owner ON proxy_routes(owner_id);
CREATE INDEX idx_proxy_routes_owner_session ON proxy_routes(owner_id, session_id);
CREATE INDEX idx_proxy_routes_expires ON proxy_routes(expires_at_unix_ms);
CREATE INDEX idx_proxy_routes_scoped_session ON proxy_routes(scoped_session_id);

-- +goose StatementBegin
CREATE TRIGGER require_proxy_terminal_session_insert BEFORE INSERT ON proxy_routes
WHEN NOT EXISTS (SELECT 1 FROM terminal_session_routes r WHERE r.scoped_session_id=NEW.scoped_session_id AND r.node_id=NEW.worker_id)
BEGIN
    SELECT RAISE(ABORT, 'proxy route terminal session not found');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER require_proxy_terminal_session_update BEFORE UPDATE OF scoped_session_id,worker_id ON proxy_routes
WHEN NOT EXISTS (SELECT 1 FROM terminal_session_routes r WHERE r.scoped_session_id=NEW.scoped_session_id AND r.node_id=NEW.worker_id)
BEGIN
    SELECT RAISE(ABORT, 'proxy route terminal session not found');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER delete_transient_session_previews AFTER DELETE ON terminal_session_routes
BEGIN
    DELETE FROM proxy_routes WHERE scoped_session_id=OLD.scoped_session_id
      AND NOT EXISTS (SELECT 1 FROM sandbox_persistent_sessions s WHERE s.scoped_session_id=OLD.scoped_session_id AND s.state!='deleting');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER delete_persistent_session_previews AFTER DELETE ON sandbox_persistent_sessions
BEGIN
    DELETE FROM proxy_routes WHERE scoped_session_id=OLD.scoped_session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER delete_tombstoned_session_previews AFTER UPDATE OF state ON sandbox_persistent_sessions
WHEN NEW.state='deleting'
BEGIN
    DELETE FROM proxy_routes WHERE scoped_session_id=NEW.scoped_session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER fence_persistent_route_insert BEFORE INSERT ON terminal_session_routes
WHEN EXISTS (SELECT 1 FROM sandbox_persistent_sessions s WHERE s.scoped_session_id=NEW.scoped_session_id AND (s.state!='running' OR s.worker_id!=NEW.node_id))
BEGIN
    SELECT RAISE(ABORT, 'persistent session route conflict');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER fence_persistent_route_update BEFORE UPDATE ON terminal_session_routes
WHEN EXISTS (SELECT 1 FROM sandbox_persistent_sessions s WHERE s.scoped_session_id=NEW.scoped_session_id AND (s.state!='running' OR s.worker_id!=NEW.node_id))
BEGIN
    SELECT RAISE(ABORT, 'persistent session route conflict');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER fence_persistent_route_update;
DROP TRIGGER fence_persistent_route_insert;
DROP TRIGGER delete_tombstoned_session_previews;
DROP TRIGGER delete_persistent_session_previews;
DROP TRIGGER delete_transient_session_previews;
DROP TRIGGER require_proxy_terminal_session_update;
DROP TRIGGER require_proxy_terminal_session_insert;
CREATE TABLE proxy_routes_transient (
    route_key TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES accounts(account_id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    scoped_session_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    created_at_unix_ms INTEGER NOT NULL,
    expires_at_unix_ms INTEGER NOT NULL CHECK (expires_at_unix_ms > created_at_unix_ms),
    FOREIGN KEY (scoped_session_id,worker_id) REFERENCES terminal_session_routes(scoped_session_id,node_id) ON DELETE CASCADE
);
INSERT INTO proxy_routes_transient SELECT p.* FROM proxy_routes p
WHERE EXISTS (SELECT 1 FROM terminal_session_routes r WHERE r.scoped_session_id=p.scoped_session_id AND r.node_id=p.worker_id);
DROP TABLE proxy_routes;
ALTER TABLE proxy_routes_transient RENAME TO proxy_routes;
CREATE INDEX idx_proxy_routes_owner ON proxy_routes(owner_id);
CREATE INDEX idx_proxy_routes_owner_session ON proxy_routes(owner_id,session_id);
CREATE INDEX idx_proxy_routes_expires ON proxy_routes(expires_at_unix_ms);
CREATE INDEX idx_proxy_routes_scoped_session ON proxy_routes(scoped_session_id);
DROP INDEX idx_sandbox_session_scope;
ALTER TABLE sandbox_persistent_sessions DROP COLUMN scoped_session_id;
