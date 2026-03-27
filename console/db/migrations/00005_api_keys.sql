-- +goose Up
CREATE TABLE api_keys (
    api_key_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    name TEXT NOT NULL,
    name_key TEXT NOT NULL,
    key_hash TEXT NOT NULL,
    key_masked TEXT NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL,
    UNIQUE (account_id, name_key),
    UNIQUE (key_hash),
    FOREIGN KEY (account_id) REFERENCES accounts(account_id) ON DELETE CASCADE
);

CREATE INDEX idx_api_keys_account_created
    ON api_keys(account_id, created_at_unix_ms);

-- +goose Down
DROP INDEX IF EXISTS idx_api_keys_account_created;
DROP TABLE IF EXISTS api_keys;
