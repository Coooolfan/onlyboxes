-- name: UpsertTerminalSessionRoute :execrows
INSERT INTO terminal_session_routes (
    scoped_session_id,
    node_id,
    lease_expires_unix_ms,
    last_used_unix_ms,
    created_at_unix_ms,
    updated_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(scoped_session_id) DO UPDATE SET
    lease_expires_unix_ms = MAX(terminal_session_routes.lease_expires_unix_ms, excluded.lease_expires_unix_ms),
    last_used_unix_ms = MAX(terminal_session_routes.last_used_unix_ms, excluded.last_used_unix_ms),
    updated_at_unix_ms = MAX(terminal_session_routes.updated_at_unix_ms, excluded.updated_at_unix_ms)
WHERE terminal_session_routes.node_id = excluded.node_id;

-- name: DeleteTerminalSessionRouteBySessionAndNode :execrows
DELETE FROM terminal_session_routes
WHERE scoped_session_id = ? AND node_id = ?;

-- name: DeleteTerminalSessionRoutesByNode :execrows
DELETE FROM terminal_session_routes
WHERE node_id = ?;

-- name: DeleteExpiredTerminalSessionRoutes :execrows
DELETE FROM terminal_session_routes
WHERE lease_expires_unix_ms <= ?;

-- name: ListActiveTerminalSessionRoutes :many
SELECT
    scoped_session_id,
    node_id,
    lease_expires_unix_ms,
    last_used_unix_ms,
    created_at_unix_ms,
    updated_at_unix_ms
FROM terminal_session_routes
WHERE lease_expires_unix_ms > ?
ORDER BY node_id ASC, scoped_session_id ASC;

-- name: GetTerminalSessionRouteBySession :one
SELECT
    scoped_session_id,
    node_id,
    lease_expires_unix_ms,
    last_used_unix_ms,
    created_at_unix_ms,
    updated_at_unix_ms
FROM terminal_session_routes
WHERE scoped_session_id = ?
LIMIT 1;
