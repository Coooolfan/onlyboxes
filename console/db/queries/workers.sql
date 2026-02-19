-- name: GetWorkerNodeByID :one
SELECT
    node_id,
    session_id,
    provisioned,
    node_name,
    executor_kind,
    version,
    registered_at_unix_ms,
    last_seen_at_unix_ms
FROM worker_nodes
WHERE node_id = ?
LIMIT 1;

-- name: ListWorkerNodesOrdered :many
SELECT
    node_id,
    session_id,
    provisioned,
    node_name,
    executor_kind,
    version,
    registered_at_unix_ms,
    last_seen_at_unix_ms
FROM worker_nodes
ORDER BY registered_at_unix_ms ASC, node_id ASC;

-- name: UpsertWorkerNode :exec
INSERT INTO worker_nodes (
    node_id,
    session_id,
    provisioned,
    node_name,
    executor_kind,
    version,
    registered_at_unix_ms,
    last_seen_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
    session_id = excluded.session_id,
    provisioned = excluded.provisioned,
    node_name = excluded.node_name,
    executor_kind = excluded.executor_kind,
    version = excluded.version,
    registered_at_unix_ms = excluded.registered_at_unix_ms,
    last_seen_at_unix_ms = excluded.last_seen_at_unix_ms;

-- name: InsertProvisionedWorkerNodeIfAbsent :execrows
INSERT INTO worker_nodes (
    node_id,
    session_id,
    provisioned,
    node_name,
    executor_kind,
    version,
    registered_at_unix_ms,
    last_seen_at_unix_ms
) VALUES (?, '', 1, ?, '', '', ?, ?)
ON CONFLICT(node_id) DO NOTHING;

-- name: DeleteWorkerNodeByID :execrows
DELETE FROM worker_nodes
WHERE node_id = ?;

-- name: UpdateWorkerHeartbeatBySession :execrows
UPDATE worker_nodes
SET last_seen_at_unix_ms = ?
WHERE node_id = ? AND session_id = ?;

-- name: ClearWorkerSessionByNode :execrows
UPDATE worker_nodes
SET session_id = ''
WHERE node_id = ?;

-- name: ClearWorkerSessionByNodeAndSession :execrows
UPDATE worker_nodes
SET session_id = ''
WHERE node_id = ? AND session_id = ?;

-- name: ClearAllWorkerSessions :execrows
UPDATE worker_nodes
SET session_id = ''
WHERE session_id <> '';

-- name: ListWorkerCapabilitiesAll :many
SELECT
    node_id,
    capability_name,
    max_inflight
FROM worker_capabilities
ORDER BY node_id ASC, capability_name ASC;

-- name: ListWorkerCapabilitiesByNode :many
SELECT
    capability_name,
    max_inflight
FROM worker_capabilities
WHERE node_id = ?
ORDER BY capability_name ASC;

-- name: DeleteWorkerCapabilitiesByNode :exec
DELETE FROM worker_capabilities
WHERE node_id = ?;

-- name: InsertWorkerCapability :exec
INSERT INTO worker_capabilities (
    node_id,
    capability_name,
    max_inflight
) VALUES (?, ?, ?)
ON CONFLICT(node_id, capability_name) DO UPDATE SET
    max_inflight = excluded.max_inflight;

-- name: ListWorkerLabelsAll :many
SELECT
    node_id,
    label_key,
    label_value
FROM worker_labels
ORDER BY node_id ASC, label_key ASC;

-- name: ListWorkerLabelsByNode :many
SELECT
    label_key,
    label_value
FROM worker_labels
WHERE node_id = ?
ORDER BY label_key ASC;

-- name: DeleteWorkerLabelsByNode :exec
DELETE FROM worker_labels
WHERE node_id = ?;

-- name: InsertWorkerLabel :exec
INSERT INTO worker_labels (
    node_id,
    label_key,
    label_value
) VALUES (?, ?, ?)
ON CONFLICT(node_id, label_key) DO UPDATE SET
    label_value = excluded.label_value;

-- name: ListOnlineWorkerNodeIDsByCapability :many
SELECT wn.node_id
FROM worker_nodes wn
JOIN worker_capabilities wc ON wc.node_id = wn.node_id
WHERE LOWER(wc.capability_name) = ?
  AND wn.last_seen_at_unix_ms >= ?
  AND wn.session_id <> ''
ORDER BY wn.node_id ASC;

-- name: DeleteOfflineRuntimeWorkers :execrows
DELETE FROM worker_nodes
WHERE provisioned = 0
  AND last_seen_at_unix_ms < ?;

-- name: GetWorkerCredentialByNode :one
SELECT
    node_id,
    secret_hash,
    hash_algo,
    created_at_unix_ms,
    updated_at_unix_ms
FROM worker_credentials
WHERE node_id = ?
LIMIT 1;

-- name: ListWorkerCredentials :many
SELECT
    node_id,
    secret_hash,
    hash_algo,
    created_at_unix_ms,
    updated_at_unix_ms
FROM worker_credentials
ORDER BY node_id ASC;

-- name: InsertWorkerCredentialIfAbsent :execrows
INSERT INTO worker_credentials (
    node_id,
    secret_hash,
    hash_algo,
    created_at_unix_ms,
    updated_at_unix_ms
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO NOTHING;

-- name: DeleteWorkerCredentialByNode :execrows
DELETE FROM worker_credentials
WHERE node_id = ?;
