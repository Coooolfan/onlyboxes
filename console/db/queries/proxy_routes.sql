-- name: InsertProxyRoute :execrows
INSERT INTO proxy_routes (
    route_key,
    owner_id,
    session_id,
    scoped_session_id,
    worker_id,
    port,
    created_at_unix_ms,
    expires_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(route_key) DO NOTHING;

-- name: DeleteProxyRouteByKeyAndOwner :execrows
DELETE FROM proxy_routes
WHERE route_key = ? AND owner_id = ?;

-- name: DeleteProxyRoutesByScopedSessionID :execrows
DELETE FROM proxy_routes
WHERE scoped_session_id = ?;

-- name: DeleteExpiredProxyRoutes :execrows
DELETE FROM proxy_routes
WHERE expires_at_unix_ms <= ?;

-- name: ListActiveProxyRoutes :many
SELECT
    route_key,
    owner_id,
    session_id,
    scoped_session_id,
    worker_id,
    port,
    created_at_unix_ms,
    expires_at_unix_ms
FROM proxy_routes
WHERE expires_at_unix_ms > ?
ORDER BY created_at_unix_ms ASC, route_key ASC;
