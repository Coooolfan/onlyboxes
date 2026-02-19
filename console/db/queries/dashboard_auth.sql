-- name: GetDashboardCredential :one
SELECT
    singleton_id,
    username,
    password_hash,
    hash_algo,
    created_at_unix_ms,
    updated_at_unix_ms
FROM dashboard_credentials
WHERE singleton_id = 1
LIMIT 1;

-- name: InsertDashboardCredential :exec
INSERT INTO dashboard_credentials (
    singleton_id,
    username,
    password_hash,
    hash_algo,
    created_at_unix_ms,
    updated_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?);

-- name: DeleteDashboardCredential :execrows
DELETE FROM dashboard_credentials
WHERE singleton_id = 1;
