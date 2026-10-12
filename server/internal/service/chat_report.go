package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

// Report statuses. The agent may report the first three; failed is written
// only by the server when an issue task fails with no retry pending.
const (
	ChatReportStatusInReview = "in_review"
	ChatReportStatusDone     = "done"
	ChatReportStatusBlocked  = "blocked"
	ChatReportStatusFailed   = "failed"
)

// ChatReportMaxContentRunes bounds a report body. A report is one or two
// sentences; anything longer belongs in the issue comment it summarizes.
const ChatReportMaxContentRunes = 4000

var (
	// ErrChatReportChatTask: the reporting task is itself a chat task. Its
	// output already is the chat reply.
	ErrChatReportChatTask = errors.New("chat report: task is a chat task")
	// ErrChatReportNoIssue: the task is not bound to an issue.
	ErrChatReportNoIssue = errors.New("chat report: task has no issue")
	// ErrChatReportNoOriginator: no human is at the top of the task's chain
	// (autopilot, system trigger), so there is nobody's chat to report into.
	ErrChatReportNoOriginator = errors.New("chat report: task has no originator")
	// ErrChatReportAgentArchived: the reporting agent has been archived.
	ErrChatReportAgentArchived = errors.New("chat report: agent is archived")
	// ErrChatReportOriginatorNoAccess: the originator left the workspace or can
	// no longer use the agent, so the agent may not write into their chat.
	ErrChatReportOriginatorNoAccess = errors.New("chat report: originator cannot access agent")
	// ErrChatReportInvalidStatus: status is not one the caller may report.
	ErrChatReportInvalidStatus = errors.New("chat report: invalid status")
	// ErrChatReportInvalidContent: content is empty or too long.
	ErrChatReportInvalidContent = errors.New("chat report: invalid content")
)

// IsAgentChatReportStatus reports whether an agent may report this status
// through `multica chat report`.
func IsAgentChatReportStatus(status string) bool {
	switch status {
	case ChatReportStatusInReview, ChatReportStatusDone, ChatReportStatusBlocked:
		return true
	}
	return false
}

func isChatReportStatus(status string) bool {
	return IsAgentChatReportStatus(status) || status == ChatReportStatusFailed
}

// ChatReportRequest is one report about an issue task, in the agent's voice or
// the server's fallback wording.
type ChatReportRequest struct {
	Task    db.AgentTaskQueue
	Status  string
	Content string
}

// ChatReportResult describes where the report landed.
type ChatReportResult struct {
	Message        db.ChatMessage
	Session        db.ChatSession
	WorkspaceID    pgtype.UUID
	OriginatorID   pgtype.UUID
	SessionCreated bool
	// Deduplicated is true when this (run, status) had already been reported;
	// Message is then the existing report and nothing new was written.
	Deduplicated bool
}

// WriteChatReport writes an assistant task_update message into the chat
// between the task's originator and the task's agent. Everything about the
// target is derived from the task row: the originator is
// task.originator_user_id (NOT the token's user, which is the runtime owner),
// the agent is task.agent_id.
//
// The row is written with CreateChatTaskReport directly, never through
// createAssistantChatMessage: a report is not the end of a chat turn, and the
// queued-input reanchor that helper performs assumes one just ended. Nothing is
// enqueued, so no agent run starts.
//
// Idempotent per (retry root of the task, status).
func (s *TaskService) WriteChatReport(ctx context.Context, req ChatReportRequest) (*ChatReportResult, error) {
	task := req.Task
	if task.ChatSessionID.Valid {
		return nil, ErrChatReportChatTask
	}
	if !task.IssueID.Valid {
		return nil, ErrChatReportNoIssue
	}
	if !task.OriginatorUserID.Valid {
		return nil, ErrChatReportNoOriginator
	}
	if !isChatReportStatus(req.Status) {
		return nil, ErrChatReportInvalidStatus
	}
	content := strings.TrimSpace(req.Content)
	if content == "" || utf8.RuneCountInString(content) > ChatReportMaxContentRunes {
		return nil, ErrChatReportInvalidContent
	}
	content = redact.Text(content)

	issue, err := s.Queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		return nil, fmt.Errorf("load issue: %w", err)
	}
	agent, err := s.Queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}
	if agent.ArchivedAt.Valid {
		return nil, ErrChatReportAgentArchived
	}
	if agent.WorkspaceID != issue.WorkspaceID {
		// The token→task binding already pins both to one workspace; this keeps
		// a wiring regression from writing across tenants.
		return nil, ErrChatReportOriginatorNoAccess
	}
	if _, err := s.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      task.OriginatorUserID,
		WorkspaceID: issue.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChatReportOriginatorNoAccess
		}
		return nil, fmt.Errorf("load originator membership: %w", err)
	}
	if s.ChatReportAccess != nil && !s.ChatReportAccess(ctx, agent, util.UUIDToString(task.OriginatorUserID), util.UUIDToString(issue.WorkspaceID)) {
		return nil, ErrChatReportOriginatorNoAccess
	}

	root, err := s.Queries.GetTaskRetryRoot(ctx, task.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve task retry root: %w", err)
	}
	status := pgtype.Text{String: req.Status, Valid: true}

	out := ChatReportResult{WorkspaceID: issue.WorkspaceID, OriginatorID: task.OriginatorUserID}
	if err := s.runInTx(ctx, func(qtx *db.Queries) error {
		// Same order as getOrCreateMikaSession: the per-target advisory lock,
		// then the workspace delete/create protocol lock, so a report racing a
		// workspace delete cannot leave an orphan session.
		if err := qtx.LockChatReportTarget(ctx, chatReportTargetKey(agent, issue.WorkspaceID, task.OriginatorUserID)); err != nil {
			return fmt.Errorf("lock report target: %w", err)
		}
		if _, err := qtx.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}

		existing, err := qtx.GetChatTaskReport(ctx, db.GetChatTaskReportParams{SourceTaskID: root, ReportStatus: status})
		if err == nil {
			session, err := qtx.GetChatSession(ctx, existing.ChatSessionID)
			if err != nil {
				return fmt.Errorf("load reported session: %w", err)
			}
			out.Message, out.Session, out.Deduplicated = existing, session, true
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load existing report: %w", err)
		}

		session, created, err := pickChatReportSession(ctx, qtx, agent, issue.WorkspaceID, task.OriginatorUserID)
		if err != nil {
			return err
		}
		if !created {
			if _, err := qtx.LockChatSessionForAppend(ctx, session.ID); err != nil {
				return fmt.Errorf("lock chat session: %w", err)
			}
		}

		msg, err := qtx.CreateChatTaskReport(ctx, db.CreateChatTaskReportParams{
			ID:            dbid.NewV7(),
			ChatSessionID: session.ID,
			Content:       content,
			SourceIssueID: issue.ID,
			SourceTaskID:  root,
			ReportStatus:  status,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Conflict on the idempotency key: a report for this run and status
			// landed outside the target lock (e.g. the agent moved sessions).
			existing, err := qtx.GetChatTaskReport(ctx, db.GetChatTaskReportParams{SourceTaskID: root, ReportStatus: status})
			if err != nil {
				return fmt.Errorf("load conflicting report: %w", err)
			}
			reported, err := qtx.GetChatSession(ctx, existing.ChatSessionID)
			if err != nil {
				return fmt.Errorf("load reported session: %w", err)
			}
			out.Message, out.Session, out.Deduplicated = existing, reported, true
			return nil
		}
		if err != nil {
			return fmt.Errorf("create report: %w", err)
		}
		if err := qtx.TouchChatSession(ctx, session.ID); err != nil {
			return fmt.Errorf("touch chat session: %w", err)
		}
		out.Message, out.Session, out.SessionCreated = msg, session, created
		return nil
	}); err != nil {
		return nil, err
	}
	return &out, nil
}

// chatReportTargetKey is the advisory-lock key for "this member's chat with
// this agent". Mika shares getOrCreateMikaSession's key so a report and the
// Mika bootstrap cannot both create her conversation.
func chatReportTargetKey(agent db.Agent, workspaceID, originatorID pgtype.UUID) string {
	ws, user := util.UUIDToString(workspaceID), util.UUIDToString(originatorID)
	if agent.SystemKey.String == MikaSystemKey {
		return "mika-session:" + ws + ":" + user
	}
	return "chat-report-session:" + ws + ":" + user + ":" + util.UUIDToString(agent.ID)
}

// pickChatReportSession chooses the session a report goes to: Mika's oldest
// active session (her single conversation identity), any other agent's most
// recently active one, skipping channel-bound sessions. With no candidate it
// creates an explicit, untitled session so the report shows up in the list.
func pickChatReportSession(ctx context.Context, qtx *db.Queries, agent db.Agent, workspaceID, originatorID pgtype.UUID) (db.ChatSession, bool, error) {
	var (
		session db.ChatSession
		err     error
	)
	if agent.SystemKey.String == MikaSystemKey {
		session, err = qtx.GetOldestReportableChatSession(ctx, db.GetOldestReportableChatSessionParams{
			WorkspaceID: workspaceID, CreatorID: originatorID, AgentID: agent.ID,
		})
	} else {
		session, err = qtx.GetMostRecentReportableChatSession(ctx, db.GetMostRecentReportableChatSessionParams{
			WorkspaceID: workspaceID, CreatorID: originatorID, AgentID: agent.ID,
		})
	}
	if err == nil {
		return session, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, false, fmt.Errorf("pick report session: %w", err)
	}
	session, err = qtx.CreateChatSession(ctx, db.CreateChatSessionParams{
		ID:          dbid.NewV7(),
		WorkspaceID: workspaceID,
		AgentID:     agent.ID,
		CreatorID:   originatorID,
	})
	if err != nil {
		return db.ChatSession{}, false, fmt.Errorf("create report session: %w", err)
	}
	session, err = qtx.MarkChatSessionExplicitlyCreated(ctx, session.ID)
	if err != nil {
		return db.ChatSession{}, false, fmt.Errorf("mark report session explicit: %w", err)
	}
	return session, true, nil
}

// PublishChatReport broadcasts a freshly written report: chat:session_created
// first when the report opened a new session, then chat:message. Both carry
// the originator as actor — chat:session_created is delivered to its ActorID
// only, so naming the agent there would reach nobody. A deduplicated result
// publishes nothing.
func (s *TaskService) PublishChatReport(res *ChatReportResult) {
	if s.Bus == nil || res == nil || res.Deduplicated {
		return
	}
	ws := util.UUIDToString(res.WorkspaceID)
	user := util.UUIDToString(res.OriginatorID)
	sessionID := util.UUIDToString(res.Session.ID)
	if res.SessionCreated {
		s.Bus.Publish(events.Event{
			Type:          protocol.EventChatSessionCreated,
			WorkspaceID:   ws,
			ActorType:     "member",
			ActorID:       user,
			ChatSessionID: sessionID,
			Payload: protocol.ChatSessionCreatedPayload{
				WorkspaceID:   ws,
				ChatSessionID: sessionID,
				AgentID:       util.UUIDToString(res.Session.AgentID),
				CreatorID:     user,
				Title:         res.Session.Title,
			},
		})
	}
	s.Bus.Publish(events.Event{
		Type:          protocol.EventChatMessage,
		WorkspaceID:   ws,
		ActorType:     "member",
		ActorID:       user,
		ChatSessionID: sessionID,
		Payload: protocol.ChatMessagePayload{
			ChatSessionID: sessionID,
			MessageID:     util.UUIDToString(res.Message.ID),
			Role:          "assistant",
			Content:       res.Message.Content,
			CreatedAt:     util.TimestampToString(res.Message.CreatedAt),
			MessageKind:   protocol.ChatMessageKindTaskUpdate,
			SourceIssueID: util.UUIDToString(res.Message.SourceIssueID),
		},
	})
}

// reportTaskOutcomeFallback is the server-side safety net behind `multica chat
// report`: when an issue task ends and its run reported nothing, write a fixed
// summary (identifier, title, status, first paragraph of the agent's latest
// comment) so the originator hears about it without asking.
//
// It fires only for an outcome this run produced:
//   - failed: the task failed with no retry pending;
//   - in_review / done / blocked: the agent itself moved the issue into that
//     status during the run. A comment-only run on an issue already sitting in
//     in_review stays silent.
//
// Best effort: failures are logged, never returned — the task outcome is
// already committed.
func (s *TaskService) reportTaskOutcomeFallback(ctx context.Context, task db.AgentTaskQueue, failed bool) {
	if !task.IssueID.Valid || task.ChatSessionID.Valid || !task.OriginatorUserID.Valid {
		return
	}
	logAttrs := []any{"task_id", util.UUIDToString(task.ID), "issue_id", util.UUIDToString(task.IssueID)}

	root, err := s.Queries.GetTaskRetryRoot(ctx, task.ID)
	if err != nil {
		slog.Warn("chat report fallback: resolve retry root failed", append(logAttrs, "error", err)...)
		return
	}
	reported, err := s.Queries.HasChatTaskReport(ctx, root)
	if err != nil {
		slog.Warn("chat report fallback: report lookup failed", append(logAttrs, "error", err)...)
		return
	}
	if reported {
		return
	}

	issue, err := s.Queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		slog.Warn("chat report fallback: load issue failed", append(logAttrs, "error", err)...)
		return
	}
	status := ChatReportStatusFailed
	if !failed {
		status = issuestatus.Effective(ctx, s.Queries, issue.WorkspaceID, issue.Status)
		if !IsAgentChatReportStatus(status) || !task.StartedAt.Valid {
			return
		}
		moved, err := s.Queries.AgentMovedIssueToStatusSince(ctx, db.AgentMovedIssueToStatusSinceParams{
			IssueID: issue.ID,
			AgentID: task.AgentID,
			Status:  issue.Status,
			Since:   task.StartedAt,
		})
		if err != nil {
			slog.Warn("chat report fallback: status history lookup failed", append(logAttrs, "error", err)...)
			return
		}
		if !moved {
			return
		}
	}

	identifier := ""
	if ws, err := s.Queries.GetWorkspace(ctx, issue.WorkspaceID); err == nil {
		identifier = IssueIdentifier(ws.IssuePrefix, issue.Number)
	}
	excerpt := ""
	if comment, err := s.Queries.GetLatestAgentCommentOnIssue(ctx, db.GetLatestAgentCommentOnIssueParams{
		IssueID: issue.ID, AuthorID: task.AgentID,
	}); err == nil {
		excerpt = firstParagraph(comment.Content)
	}
	language := ""
	if user, err := s.Queries.GetUser(ctx, task.OriginatorUserID); err == nil {
		language = user.Language.String
	}

	res, err := s.WriteChatReport(ctx, ChatReportRequest{
		Task:    task,
		Status:  status,
		Content: buildFallbackChatReport(language, identifier, issue.Title, status, excerpt),
	})
	if err != nil {
		if !errors.Is(err, ErrChatReportOriginatorNoAccess) && !errors.Is(err, ErrChatReportAgentArchived) {
			slog.Warn("chat report fallback: write failed", append(logAttrs, "status", status, "error", err)...)
		}
		return
	}
	s.PublishChatReport(res)
}

// fallbackExcerptMaxRunes keeps the quoted comment paragraph to a preview.
const fallbackExcerptMaxRunes = 600

// firstParagraph returns the first non-empty paragraph of a markdown body,
// skipping leading headings, clipped to a preview length.
func firstParagraph(body string) string {
	for _, block := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if strings.HasPrefix(block, "#") && !strings.Contains(block, "\n") {
			continue
		}
		if utf8.RuneCountInString(block) > fallbackExcerptMaxRunes {
			runes := []rune(block)
			block = strings.TrimSpace(string(runes[:fallbackExcerptMaxRunes])) + "…"
		}
		return block
	}
	return ""
}

var fallbackStatusLabels = map[string][2]string{
	// {English, Chinese}
	ChatReportStatusInReview: {"ready for review", "待验收"},
	ChatReportStatusDone:     {"done", "已完成"},
	ChatReportStatusBlocked:  {"blocked", "已阻塞"},
	ChatReportStatusFailed:   {"run failed", "运行失败"},
}

// buildFallbackChatReport renders the fixed-format server report in the
// originator's language (Chinese for zh*, English otherwise).
func buildFallbackChatReport(language, identifier, title, status, excerpt string) string {
	zh := strings.HasPrefix(strings.ToLower(strings.TrimSpace(language)), "zh")
	labels := fallbackStatusLabels[status]
	label := labels[0]
	if zh {
		label = labels[1]
	}
	subject := strings.TrimSpace(title)
	if identifier != "" {
		subject = identifier + " " + subject
	}
	var b strings.Builder
	if zh {
		fmt.Fprintf(&b, "%s：%s", subject, label)
	} else {
		fmt.Fprintf(&b, "%s: %s", subject, label)
	}
	if excerpt != "" {
		b.WriteString("\n\n")
		b.WriteString(excerpt)
	}
	return b.String()
}
