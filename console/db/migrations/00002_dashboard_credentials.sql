-- +goose Up
CREATE TABLE dashboard_credentials (
    singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
    username TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    hash_algo TEXT NOT NULL,
    created_at_unix_ms INTEGER NOT NULL,
    updated_at_unix_ms INTEGER NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS dashboard_credentials;
