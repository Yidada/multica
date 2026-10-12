ALTER TABLE chat_message
    DROP COLUMN IF EXISTS report_status,
    DROP COLUMN IF EXISTS source_task_id,
    DROP COLUMN IF EXISTS source_issue_id;
