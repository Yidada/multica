package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// Reason codes for POST /api/chat/report rejections.
const (
	chatReportReasonTaskTokenRequired = "task_token_required"
	chatReportReasonInvalidContent    = "invalid_content"
	chatReportReasonInvalidStatus     = "invalid_status"
	chatReportReasonIssueMismatch     = "issue_mismatch"
	chatReportReasonChatTask          = "chat_task"
	chatReportReasonNoIssue           = "no_issue"
	chatReportReasonNoOriginator      = "no_originator"
	chatReportReasonAgentArchived     = "agent_archived"
	chatReportReasonNoAccess          = "originator_cannot_access_agent"
)

type chatReportRequest struct {
	Status  string `json:"status"`
	Content string `json:"content"`
	// IssueID is a consistency check only; it can never redirect the report.
	IssueID string `json:"issue_id,omitempty"`
}

type chatReportResponse struct {
	MessageID      string `json:"message_id"`
	ChatSessionID  string `json:"chat_session_id"`
	SessionCreated bool   `json:"session_created"`
	Deduplicated   bool   `json:"deduplicated"`
	CreatedAt      string `json:"created_at"`
}

// ReportTaskToChat serves `multica chat report`: the running agent tells the
// task's originator, in its own words, that the issue reached in_review / done
// / blocked. It accepts only a mat_ task token. The session, the agent and the
// originator are all derived from the token's task, never from the request, so
// an agent can only write into the chat between itself and the human at the top
// of its task chain. Nothing is enqueued and no agent run starts.
func (h *Handler) ReportTaskToChat(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeErrorCode(w, http.StatusForbidden, chatReportReasonTaskTokenRequired, "chat report is only available from within an agent task")
		return
	}
	taskUUID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing task context")
		return
	}

	var req chatReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if !service.IsAgentChatReportStatus(req.Status) {
		writeErrorCode(w, http.StatusBadRequest, chatReportReasonInvalidStatus, "status must be one of in_review, done, blocked")
		return
	}

	task, err := h.Queries.GetAgentTask(r.Context(), taskUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if issueID := strings.TrimSpace(req.IssueID); issueID != "" {
		if task.IssueID.Valid && issueID != uuidToString(task.IssueID) {
			writeErrorCode(w, http.StatusBadRequest, chatReportReasonIssueMismatch, "issue_id does not match this task's issue")
			return
		}
	}
	// Defense in depth: the task must live in the token's stamped workspace.
	if ws := ctxWorkspaceID(r.Context()); ws != "" && task.IssueID.Valid {
		issue, err := h.Queries.GetIssue(r.Context(), task.IssueID)
		if err != nil {
			writeError(w, http.StatusNotFound, "issue not found")
			return
		}
		if uuidToString(issue.WorkspaceID) != ws {
			writeError(w, http.StatusForbidden, "task does not belong to this workspace")
			return
		}
	}

	res, err := h.TaskService.WriteChatReport(r.Context(), service.ChatReportRequest{
		Task:    task,
		Status:  req.Status,
		Content: req.Content,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrChatReportInvalidContent):
			writeErrorCode(w, http.StatusBadRequest, chatReportReasonInvalidContent, "content must be 1-4000 characters")
		case errors.Is(err, service.ErrChatReportChatTask):
			writeErrorCode(w, http.StatusConflict, chatReportReasonChatTask, "chat tasks reply in chat directly; nothing to report")
		case errors.Is(err, service.ErrChatReportNoIssue):
			writeErrorCode(w, http.StatusUnprocessableEntity, chatReportReasonNoIssue, "this task is not bound to an issue")
		case errors.Is(err, service.ErrChatReportNoOriginator):
			writeErrorCode(w, http.StatusUnprocessableEntity, chatReportReasonNoOriginator, "this task has no human originator to report to")
		case errors.Is(err, service.ErrChatReportAgentArchived):
			writeErrorCode(w, http.StatusConflict, chatReportReasonAgentArchived, "agent is archived")
		case errors.Is(err, service.ErrChatReportOriginatorNoAccess):
			writeErrorCode(w, http.StatusForbidden, chatReportReasonNoAccess, "the task originator can no longer use this agent")
		default:
			slog.Error("chat report failed", append(logger.RequestAttrs(r), "error", err, "task_id", uuidToString(task.ID))...)
			writeError(w, http.StatusInternalServerError, "failed to write chat report")
		}
		return
	}
	h.TaskService.PublishChatReport(res)

	status := http.StatusCreated
	if res.Deduplicated {
		status = http.StatusOK
	}
	writeJSON(w, status, chatReportResponse{
		MessageID:      uuidToString(res.Message.ID),
		ChatSessionID:  uuidToString(res.Session.ID),
		SessionCreated: res.SessionCreated,
		Deduplicated:   res.Deduplicated,
		CreatedAt:      timestampToString(res.Message.CreatedAt),
	})
}
