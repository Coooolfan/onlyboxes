-- +goose Up
DROP TABLE IF EXISTS worker_sys_owner_claims;

-- +goose Down
CREATE TABLE worker_sys_owner_claims (
    owner_id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL UNIQUE,
    claimed_at_unix_ms INTEGER NOT NULL,
    FOREIGN KEY (node_id) REFERENCES worker_nodes(node_id) ON DELETE CASCADE
);

INSERT INTO worker_sys_owner_claims (owner_id, node_id, claimed_at_unix_ms)
SELECT owner_label.label_value, wn.node_id, wn.registered_at_unix_ms
FROM worker_nodes wn
JOIN worker_labels owner_label
  ON owner_label.node_id = wn.node_id AND owner_label.label_key = 'obx.owner_id'
JOIN worker_labels type_label
  ON type_label.node_id = wn.node_id AND type_label.label_key = 'obx.worker_type'
WHERE owner_label.label_value <> '' AND LOWER(type_label.label_value) = 'worker-sys'
ORDER BY owner_label.label_value, wn.registered_at_unix_ms, wn.node_id
ON CONFLICT(owner_id) DO NOTHING;
