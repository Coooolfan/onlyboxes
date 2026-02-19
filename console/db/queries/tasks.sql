-- name: InsertTask :exec
INSERT INTO tasks (
    task_id,
    owner_id,
    request_id,
    capability,
    input_json,
    status,
    command_id,
    result_json,
    error_code,
    error_message,
    created_at_unix_ms,
    updated_at_unix_ms,
    deadline_at_unix_ms,
    completed_at_unix_ms,
    expires_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTaskByID :one
SELECT
    task_id,
    owner_id,
    request_id,
    capability,
    input_json,
    status,
    command_id,
    result_json,
    error_code,
    error_message,
    created_at_unix_ms,
    updated_at_unix_ms,
    deadline_at_unix_ms,
    completed_at_unix_ms,
    expires_at_unix_ms
FROM tasks
WHERE task_id = ?
LIMIT 1;

-- name: GetTaskByOwnerAndRequest :one
SELECT
    task_id,
    owner_id,
    request_id,
    capability,
    input_json,
    status,
    command_id,
    result_json,
    error_code,
    error_message,
    created_at_unix_ms,
    updated_at_unix_ms,
    deadline_at_unix_ms,
    completed_at_unix_ms,
    expires_at_unix_ms
FROM tasks
WHERE owner_id = ? AND request_id = ?
LIMIT 1;

-- name: MarkTaskDispatched :execrows
UPDATE tasks
SET status = 'dispatched',
    updated_at_unix_ms = ?
WHERE task_id = ?
  AND status IN ('queued', 'dispatched', 'running');

-- name: MarkTaskRunning :execrows
UPDATE tasks
SET status = 'running',
    command_id = ?,
    updated_at_unix_ms = ?
WHERE task_id = ?
  AND status IN ('queued', 'dispatched', 'running');

-- name: MarkTaskTerminal :execrows
UPDATE tasks
SET status = ?,
    result_json = ?,
    error_code = ?,
    error_message = ?,
    updated_at_unix_ms = ?,
    completed_at_unix_ms = ?,
    expires_at_unix_ms = ?
WHERE task_id = ?
  AND status IN ('queued', 'dispatched', 'running');

-- name: DeleteExpiredTerminalTasks :execrows
DELETE FROM tasks
WHERE status IN ('succeeded', 'failed', 'timeout', 'canceled')
  AND expires_at_unix_ms > 0
  AND expires_at_unix_ms <= ?;
