-- name: MarkNonTerminalTasksFailedOnStartup :execrows
UPDATE tasks
SET status = 'failed',
    error_code = 'console_restarted',
    error_message = 'task interrupted by console restart',
    updated_at_unix_ms = ?,
    completed_at_unix_ms = ?,
    expires_at_unix_ms = ?
WHERE status IN ('queued', 'dispatched', 'running');
