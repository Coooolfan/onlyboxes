-- +goose Up
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

-- +goose StatementBegin
CREATE TRIGGER ignore_proxy_routes_without_terminal_session
BEFORE INSERT ON proxy_routes
FOR EACH ROW
WHEN NOT EXISTS (
    SELECT 1
    FROM terminal_session_routes
    WHERE terminal_session_routes.scoped_session_id = NEW.scoped_session_id
      AND terminal_session_routes.node_id = NEW.worker_id
      AND terminal_session_routes.lease_expires_unix_ms > NEW.created_at_unix_ms
)
BEGIN
    SELECT RAISE(IGNORE);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER delete_proxy_routes_after_terminal_session_route
AFTER DELETE ON terminal_session_routes
FOR EACH ROW
BEGIN
    DELETE FROM proxy_routes
    WHERE scoped_session_id = OLD.scoped_session_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS delete_proxy_routes_after_terminal_session_route;
DROP TRIGGER IF EXISTS ignore_proxy_routes_without_terminal_session;
DROP INDEX IF EXISTS idx_proxy_routes_scoped_session;
