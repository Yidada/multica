-- Task progress reports into chat (`multica chat report` + server fallback).
-- A report is an assistant chat_message with message_kind = 'task_update'. It
-- never enqueues or belongs to a chat turn, so task_id stays NULL.

-- name: LockChatReportTarget :exec
-- Serializes "pick or create the session to report into" per target key, so
-- two concurrent reports for the same member/agent cannot each create a fresh
-- session. Mika uses the same key as getOrCreateMikaSession.
SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0));

-- name: GetOldestReportableChatSession :one
-- Mika's conversation identity is the member's OLDEST active session with her
-- (same rule as GetOldestActiveChatSessionForCreatorAgent). Sessions bound to
-- an IM channel are skipped: a row written here is never delivered to the
-- channel, so the web transcript and the channel would disagree.
SELECT cs.* FROM chat_session AS cs
WHERE cs.workspace_id = $1
  AND cs.creator_id = $2
  AND cs.agent_id = $3
  AND cs.status = 'active'
  AND NOT EXISTS (
    SELECT 1 FROM channel_chat_session_binding AS b
    WHERE b.chat_session_id = cs.id
  )
ORDER BY cs.created_at ASC
LIMIT 1;

-- name: GetMostRecentReportableChatSession :one
-- Every other agent reports into the member's most recently active session
-- with it, ordered the way ListChatSessionsByCreator orders the chat list, so
-- the report lands in the session the client shows on top.
SELECT cs.* FROM chat_session AS cs
LEFT JOIN LATERAL (
  SELECT m.created_at FROM chat_message AS m
   WHERE m.chat_session_id = cs.id
     AND m.message_kind != 'channel_command'
   ORDER BY m.created_at DESC
   LIMIT 1
) AS lm ON true
WHERE cs.workspace_id = $1
  AND cs.creator_id = $2
  AND cs.agent_id = $3
  AND cs.status = 'active'
  AND NOT EXISTS (
    SELECT 1 FROM channel_chat_session_binding AS b
    WHERE b.chat_session_id = cs.id
  )
ORDER BY COALESCE(lm.created_at, cs.updated_at) DESC
LIMIT 1;

-- name: CreateChatTaskReport :one
-- Idempotent on (source_task_id, report_status) through the partial unique
-- index chat_message_task_report_uniq. A conflict returns no row; the caller
-- then reads the existing report with GetChatTaskReport.
INSERT INTO chat_message (
    id, chat_session_id, role, content, message_kind,
    source_issue_id, source_task_id, report_status
)
VALUES (
    sqlc.arg(id), sqlc.arg(chat_session_id), 'assistant', sqlc.arg(content), 'task_update',
    sqlc.arg(source_issue_id), sqlc.arg(source_task_id), sqlc.arg(report_status)
)
ON CONFLICT (source_task_id, report_status) WHERE message_kind = 'task_update' DO NOTHING
RETURNING *;

-- name: GetChatTaskReport :one
SELECT * FROM chat_message
WHERE source_task_id = $1
  AND report_status = $2
  AND message_kind = 'task_update';

-- name: HasChatTaskReport :one
-- Whether the run (retry root) already reported anything. The server-side
-- fallback stands down when it did, whatever status the agent reported.
SELECT EXISTS (
    SELECT 1 FROM chat_message
    WHERE source_task_id = $1
      AND message_kind = 'task_update'
) AS reported;

-- name: GetTaskRetryRoot :one
-- Walks retry_of_task_id back to the first attempt. Retries chain parent to
-- child (each clone points at the attempt it re-runs), so the root identifies
-- the run across system auto-retries. Bounded so a corrupt cycle cannot spin.
WITH RECURSIVE chain AS (
    SELECT t.id, t.retry_of_task_id, 0 AS depth
    FROM agent_task_queue AS t
    WHERE t.id = $1
  UNION ALL
    SELECT p.id, p.retry_of_task_id, chain.depth + 1
    FROM agent_task_queue AS p
    JOIN chain ON p.id = chain.retry_of_task_id
    WHERE chain.depth < 32
)
SELECT chain.id FROM chain
ORDER BY chain.depth DESC
LIMIT 1;

-- name: GetLatestAgentCommentOnIssue :one
-- The newest live comment this agent wrote on the issue; the fallback report
-- quotes its first paragraph.
SELECT * FROM comment
WHERE issue_id = $1
  AND author_type = 'agent'
  AND author_id = $2
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: AgentMovedIssueToStatusSince :one
-- Whether this agent moved the issue into the given status during the run.
-- The fallback reports only a status the run itself produced: a comment-only
-- run on an issue that was already in_review must not re-announce it.
SELECT EXISTS (
    SELECT 1 FROM activity_log
    WHERE issue_id = sqlc.arg(issue_id)
      AND action = 'status_changed'
      AND actor_type = 'agent'
      AND actor_id = sqlc.arg(agent_id)
      AND details->>'to' = sqlc.arg(status)::text
      AND created_at >= sqlc.arg(since)
) AS moved;
