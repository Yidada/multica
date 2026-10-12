-- One report per (task run, status): idempotency key for both the agent's
-- `multica chat report` and the server-side fallback.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS chat_message_task_report_uniq ON chat_message (source_task_id, report_status) WHERE message_kind = 'task_update';
