-- +goose Up
ALTER TABLE sandbox_persistence_operations
    ADD COLUMN parameters_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE sandbox_persistence_operations DROP COLUMN parameters_json;
