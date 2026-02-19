-- name: ListTrustedTokens :many
SELECT
    token_id,
    name,
    name_key,
    token_hash,
    token_masked,
    generated,
    created_at_unix_ms,
    updated_at_unix_ms
FROM trusted_tokens
ORDER BY created_at_unix_ms ASC, token_id ASC;

-- name: GetTrustedTokenByID :one
SELECT
    token_id,
    name,
    name_key,
    token_hash,
    token_masked,
    generated,
    created_at_unix_ms,
    updated_at_unix_ms
FROM trusted_tokens
WHERE token_id = ?
LIMIT 1;

-- name: GetTrustedTokenByHash :one
SELECT
    token_id,
    name,
    name_key,
    token_hash,
    token_masked,
    generated,
    created_at_unix_ms,
    updated_at_unix_ms
FROM trusted_tokens
WHERE token_hash = ?
LIMIT 1;

-- name: InsertTrustedToken :exec
INSERT INTO trusted_tokens (
    token_id,
    name,
    name_key,
    token_hash,
    token_masked,
    generated,
    created_at_unix_ms,
    updated_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteTrustedTokenByID :execrows
DELETE FROM trusted_tokens
WHERE token_id = ?;
