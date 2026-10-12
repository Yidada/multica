package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// chatReportTask builds an agent and a running issue task originated by the
// handler test member. Extra columns override the task row.
func chatReportTask(t *testing.T, agentOver testutil.Cols, taskOver testutil.Cols) (agentID, issueID, taskID string) {
	t.Helper()
	agentID = dbfx.Agent(t, "ChatReportAgent-"+uuid.NewString()[:8], handlerTestRuntimeID(t), agentOver)
	issueID = dbfx.Issue(t, "Chat report issue")
	cols := testutil.Cols{
		"runtime_id":          handlerTestRuntimeID(t),
		"issue_id":            issueID,
		"status":              "running",
		"started_at":          testutil.Raw("now()"),
		"originator_user_id":  testUserID,
		"accountable_user_id": testUserID,
	}
	for k, v := range taskOver {
		cols[k] = v
	}
	taskID = dbfx.Task(t, agentID, cols)
	// t.Cleanup is LIFO: messages go before their session.
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE agent_id = $1`, agentID)
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id IN (SELECT id FROM chat_session WHERE agent_id = $1)`, agentID)
	return agentID, issueID, taskID
}

func chatReportReq(taskID string, body any) *http.Request {
	req := newRequest(http.MethodPost, "/api/chat/report", body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	return req
}

func TestReportTaskToChat_RequiresTaskToken(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	_, _, taskID := chatReportTask(t, nil, nil)
	req := newRequest(http.MethodPost, "/api/chat/report", map[string]any{"status": "done", "content": "x"})
	req.Header.Set("X-Task-ID", taskID)
	body := testutil.Call(t, testHandler.ReportTaskToChat, req).Want(http.StatusForbidden).Map()
	if body["code"] != chatReportReasonTaskTokenRequired {
		t.Fatalf("code = %v", body["code"])
	}
}

func TestReportTaskToChat_WritesOnceAndListsAsTaskUpdate(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	agentID, issueID, taskID := chatReportTask(t, nil, nil)

	var first chatReportResponse
	testutil.Call(t, testHandler.ReportTaskToChat, chatReportReq(taskID, map[string]any{
		"status": "in_review", "content": "CLE-147 is ready for review.", "issue_id": issueID,
	})).Want(http.StatusCreated).JSON(&first)
	if !first.SessionCreated || first.Deduplicated || first.MessageID == "" {
		t.Fatalf("first report = %+v", first)
	}

	var again chatReportResponse
	testutil.Call(t, testHandler.ReportTaskToChat, chatReportReq(taskID, map[string]any{
		"status": "in_review", "content": "said twice",
	})).Want(http.StatusOK).JSON(&again)
	if !again.Deduplicated || again.MessageID != first.MessageID || again.ChatSessionID != first.ChatSessionID {
		t.Fatalf("repeat report = %+v, want dedup of %+v", again, first)
	}

	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, first.ChatSessionID); n != 0 {
		t.Fatalf("report enqueued %d chat tasks", n)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+first.ChatSessionID+"/messages", nil)
	req.Header.Set("X-User-ID", testUserID)
	req = withURLParam(req, "sessionId", first.ChatSessionID)
	req = withChatTestWorkspaceCtx(t, req)
	var messages []ChatMessageResponse
	testutil.Call(t, testHandler.ListChatMessages, req).Want(http.StatusOK).JSON(&messages)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	m := messages[0]
	if m.Role != "assistant" || m.MessageKind != protocol.ChatMessageKindTaskUpdate || m.TaskID != nil ||
		m.SourceIssueID == nil || *m.SourceIssueID != issueID || m.ReportStatus == nil || *m.ReportStatus != "in_review" {
		t.Fatalf("listed report = %+v", m)
	}

	var sessionAgent string
	dbfx.QueryRow(t, `SELECT agent_id::text FROM chat_session WHERE id = $1`, first.ChatSessionID).Scan(&sessionAgent)
	if sessionAgent != agentID {
		t.Fatalf("session agent = %s, want %s", sessionAgent, agentID)
	}
}

func TestReportTaskToChat_Rejections(t *testing.T) {
	if testHandler == nil {
		t.Skip("requires test database")
	}
	otherOwner := dbfx.User(t, "Chat Report Owner", "chat-report-owner-"+uuid.NewString()[:8]+"@multica.test")

	cases := []struct {
		name      string
		agentOver testutil.Cols
		taskOver  testutil.Cols
		body      map[string]any
		want      int
		code      string
	}{
		{name: "status not reportable", body: map[string]any{"status": "in_progress", "content": "x"}, want: http.StatusBadRequest, code: chatReportReasonInvalidStatus},
		{name: "empty content", body: map[string]any{"status": "done", "content": "  "}, want: http.StatusBadRequest, code: chatReportReasonInvalidContent},
		{name: "issue mismatch", body: map[string]any{"status": "done", "content": "x", "issue_id": uuid.NewString()}, want: http.StatusBadRequest, code: chatReportReasonIssueMismatch},
		{name: "no originator", taskOver: testutil.Cols{"originator_user_id": nil}, body: map[string]any{"status": "done", "content": "x"}, want: http.StatusUnprocessableEntity, code: chatReportReasonNoOriginator},
		{
			// A private agent owned by someone else: the originator may not
			// invoke it, so the agent may not write into their chat.
			name:      "originator cannot use agent",
			agentOver: testutil.Cols{"owner_id": otherOwner},
			body:      map[string]any{"status": "done", "content": "x"},
			want:      http.StatusForbidden,
			code:      chatReportReasonNoAccess,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, taskID := chatReportTask(t, tc.agentOver, tc.taskOver)
			body := testutil.Call(t, testHandler.ReportTaskToChat, chatReportReq(taskID, tc.body)).Want(tc.want).Map()
			if body["code"] != tc.code {
				t.Fatalf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}
}

func TestNormalizeMessageKindKeepsTaskUpdate(t *testing.T) {
	if got := normalizeMessageKind(protocol.ChatMessageKindTaskUpdate); got != protocol.ChatMessageKindTaskUpdate {
		t.Fatalf("normalizeMessageKind(task_update) = %q", got)
	}
	raw, _ := json.Marshal(chatMessageToResponse(db.ChatMessage{Role: "assistant", MessageKind: protocol.ChatMessageKindMessage}, nil))
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if _, ok := decoded["source_issue_id"]; ok {
		t.Fatalf("ordinary message must omit source_issue_id: %s", raw)
	}
}
