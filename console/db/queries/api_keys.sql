-- name: ListAPIKeysByAccount :many
SELECT
    api_key_id,
    account_id,
    name,
    name_key,
    key_hash,
    key_masked,
    created_at_unix_ms,
    updated_at_unix_ms
FROM api_keys
WHERE account_id = ?
ORDER BY created_at_unix_ms ASC, api_key_id ASC;

-- name: GetAPIKeyByID :one
SELECT
    api_key_id,
    account_id,
    name,
    name_key,
    key_hash,
    key_masked,
    created_at_unix_ms,
    updated_at_unix_ms
FROM api_keys
WHERE api_key_id = ?
LIMIT 1;

-- name: GetAPIKeyByAccountAndNameKey :one
SELECT
    api_key_id,
    account_id,
    name,
    name_key,
    key_hash,
    key_masked,
    created_at_unix_ms,
    updated_at_unix_ms
FROM api_keys
WHERE account_id = ? AND name_key = ?
LIMIT 1;

-- name: GetAPIKeyByHash :one
SELECT
    api_key_id,
    account_id,
    name,
    name_key,
    key_hash,
    key_masked,
    created_at_unix_ms,
    updated_at_unix_ms
FROM api_keys
WHERE key_hash = ?
LIMIT 1;

-- name: InsertAPIKey :exec
INSERT INTO api_keys (
    api_key_id,
    account_id,
    name,
    name_key,
    key_hash,
    key_masked,
    created_at_unix_ms,
    updated_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteAPIKeyByIDAndAccount :execrows
DELETE FROM api_keys
WHERE api_key_id = ? AND account_id = ?;
