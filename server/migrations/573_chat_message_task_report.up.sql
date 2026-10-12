-- Task progress reports written into chat (`multica chat report` and the
-- server-side fallback). A report is an assistant chat_message with
-- message_kind = 'task_update'; it is never a chat turn, so it carries no
-- task_id (task_id marks the row a chat task produced and drives regenerate /
-- input-batch joins).
--
-- source_issue_id  — the issue the reported task ran on; clients hang a task
--                    card off it.
-- source_task_id   — the retry root of the reporting task, so a system
--                    auto-retry does not report the same run twice.
-- report_status    — in_review | done | blocked | failed.
--
-- No FK, matching the soft-reference convention for task lineage columns.
ALTER TABLE chat_message
    ADD COLUMN IF NOT EXISTS source_issue_id UUID,
    ADD COLUMN IF NOT EXISTS source_task_id UUID,
    ADD COLUMN IF NOT EXISTS report_status TEXT;
