package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

type chatReportFixture struct {
	pool        *pgxpool.Pool
	svc         *TaskService
	bus         *events.Bus
	workspaceID string
	userID      string
	agentID     string
	runtimeID   string
	issueID     string
}

func seedChatReportFixture(t *testing.T) *chatReportFixture {
	t.Helper()
	pool := sharedTestPool(t)
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	f := &chatReportFixture{pool: pool, workspaceID: workspaceID, userID: userID, agentID: agentID, issueID: issueID}
	f.exec(t, `UPDATE workspace SET issue_prefix = 'REP' WHERE id = $1`, workspaceID)
	f.exec(t, `UPDATE issue SET number = 7, title = 'Ship the report endpoint' WHERE id = $1`, issueID)
	if err := pool.QueryRow(context.Background(), `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&f.runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	f.bus = events.New()
	f.svc = NewTaskService(db.New(pool), pool, nil, f.bus)
	return f
}

func (f *chatReportFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *chatReportFixture) scalar(t *testing.T, dest any, sql string, args ...any) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(dest); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
}

// issueTask inserts a running issue task started a minute ago, originated by
// the fixture's member.
func (f *chatReportFixture) issueTask(t *testing.T, extra string, args ...any) db.AgentTaskQueue {
	t.Helper()
	var id string
	f.scalar(t, &id, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, originator_user_id, accountable_user_id)
		VALUES ($1, $2, $3, 'running', 0, now() - interval '1 minute', $4, $4)
		RETURNING id`, f.agentID, f.runtimeID, f.issueID, f.userID)
	if extra != "" {
		f.exec(t, `UPDATE agent_task_queue SET `+extra+` WHERE id = $1`, append([]any{id}, args...)...)
	}
	task, err := f.svc.Queries.GetAgentTask(context.Background(), util.MustParseUUID(id))
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	return task
}

func (f *chatReportFixture) session(t *testing.T, extra string) string {
	t.Helper()
	var id string
	f.scalar(t, &id, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at)
		VALUES ($1, $2, $3, 'existing', now())
		RETURNING id`, f.workspaceID, f.agentID, f.userID)
	if extra != "" {
		f.exec(t, `UPDATE chat_session SET `+extra+` WHERE id = $1`, id)
	}
	return id
}

func (f *chatReportFixture) reports(t *testing.T) []db.ChatMessage {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT m.id, m.chat_session_id, m.content, m.task_id, m.message_kind, m.source_issue_id, m.source_task_id, m.report_status
		FROM chat_message m JOIN chat_session s ON s.id = m.chat_session_id
		WHERE s.workspace_id = $1 AND m.message_kind = 'task_update'
		ORDER BY m.created_at`, f.workspaceID)
	if err != nil {
		t.Fatalf("list reports: %v", err)
	}
	defer rows.Close()
	var out []db.ChatMessage
	for rows.Next() {
		var m db.ChatMessage
		if err := rows.Scan(&m.ID, &m.ChatSessionID, &m.Content, &m.TaskID, &m.MessageKind, &m.SourceIssueID, &m.SourceTaskID, &m.ReportStatus); err != nil {
			t.Fatalf("scan report: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestWriteChatReportCreatesSessionAndIsIdempotent(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	task := f.issueTask(t, "")

	var published []events.Event
	for _, typ := range []string{protocol.EventChatMessage, protocol.EventChatSessionCreated} {
		f.bus.Subscribe(typ, func(e events.Event) { published = append(published, e) })
	}

	res, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: task, Status: ChatReportStatusInReview, Content: "  Ready for review.  "})
	if err != nil {
		t.Fatalf("WriteChatReport: %v", err)
	}
	if !res.SessionCreated || res.Deduplicated {
		t.Fatalf("first report: created=%v dedup=%v, want created and not dedup", res.SessionCreated, res.Deduplicated)
	}
	if !res.Session.ExplicitlyCreatedAt.Valid {
		t.Fatal("report session must be explicitly created so it shows in the chat list")
	}
	if util.UUIDToString(res.Session.CreatorID) != f.userID || util.UUIDToString(res.Session.AgentID) != f.agentID {
		t.Fatalf("session belongs to %s/%s, want originator %s and agent %s",
			util.UUIDToString(res.Session.CreatorID), util.UUIDToString(res.Session.AgentID), f.userID, f.agentID)
	}
	f.svc.PublishChatReport(res)
	if len(published) != 2 || published[0].Type != protocol.EventChatSessionCreated || published[1].Type != protocol.EventChatMessage {
		t.Fatalf("published %d events (%v), want session_created then message", len(published), published)
	}
	for _, e := range published {
		if e.ActorID != f.userID {
			t.Errorf("%s actor = %s, want originator %s", e.Type, e.ActorID, f.userID)
		}
	}
	payload := published[1].Payload.(protocol.ChatMessagePayload)
	if payload.MessageKind != protocol.ChatMessageKindTaskUpdate || payload.SourceIssueID != f.issueID || payload.Role != "assistant" {
		t.Fatalf("chat:message payload = %+v", payload)
	}

	reports := f.reports(t)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	got := reports[0]
	if got.Content != "Ready for review." || got.TaskID.Valid || got.ReportStatus.String != ChatReportStatusInReview ||
		util.UUIDToString(got.SourceIssueID) != f.issueID || got.SourceTaskID != task.ID {
		t.Fatalf("report row = %+v", got)
	}

	again, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: task, Status: ChatReportStatusInReview, Content: "Different words"})
	if err != nil {
		t.Fatalf("repeat WriteChatReport: %v", err)
	}
	if !again.Deduplicated || again.Message.ID != res.Message.ID || again.Session.ID != res.Session.ID {
		t.Fatalf("repeat: dedup=%v message=%v, want dedup of %v", again.Deduplicated, again.Message.ID, res.Message.ID)
	}
	published = nil
	f.svc.PublishChatReport(again)
	if len(published) != 0 {
		t.Fatalf("deduplicated report published %d events", len(published))
	}

	done, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: task, Status: ChatReportStatusDone, Content: "Merged."})
	if err != nil {
		t.Fatalf("done report: %v", err)
	}
	if done.Deduplicated || done.SessionCreated || done.Session.ID != res.Session.ID {
		t.Fatalf("done report: dedup=%v created=%v session=%v, want a new row in the same session", done.Deduplicated, done.SessionCreated, done.Session.ID)
	}

	var sessions, tasks int
	f.scalar(t, &sessions, `SELECT count(*) FROM chat_session WHERE workspace_id = $1`, f.workspaceID)
	f.scalar(t, &tasks, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, res.Session.ID)
	if sessions != 1 || tasks != 0 {
		t.Fatalf("sessions=%d chat tasks=%d, want one session and no enqueued run", sessions, tasks)
	}
}

func TestWriteChatReportPicksMostRecentSessionSkippingChannelBound(t *testing.T) {
	f := seedChatReportFixture(t)
	older := f.session(t, "updated_at = now() - interval '2 days'")
	recent := f.session(t, "updated_at = now() - interval '1 hour'")
	bound := f.session(t, "updated_at = now()")
	f.session(t, "updated_at = now(), status = 'archived'")
	f.exec(t, `
		INSERT INTO channel_chat_session_binding (chat_session_id, installation_id, channel_type, channel_chat_id, chat_type)
		VALUES ($1, gen_random_uuid(), 'slack', 'C1', 'p2p')`, bound)
	// A message newer than every updated_at puts the oldest session on top of
	// the chat list, which is the order the report follows.
	f.exec(t, `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, 'user', 'hi', now() + interval '1 minute')`, older)

	res, err := f.svc.WriteChatReport(context.Background(), ChatReportRequest{Task: f.issueTask(t, ""), Status: ChatReportStatusBlocked, Content: "Blocked on credentials."})
	if err != nil {
		t.Fatalf("WriteChatReport: %v", err)
	}
	if util.UUIDToString(res.Session.ID) != older || res.SessionCreated {
		t.Fatalf("report landed in %s (created=%v), want most recently active %s (recent=%s bound=%s)",
			util.UUIDToString(res.Session.ID), res.SessionCreated, older, recent, bound)
	}
}

func TestWriteChatReportMikaUsesOldestSession(t *testing.T) {
	f := seedChatReportFixture(t)
	f.exec(t, `UPDATE agent SET system_key = 'mika', kind = 'system' WHERE id = $1`, f.agentID)
	oldest := f.session(t, "created_at = now() - interval '30 days', updated_at = now() - interval '30 days'")
	f.session(t, "updated_at = now()")

	res, err := f.svc.WriteChatReport(context.Background(), ChatReportRequest{Task: f.issueTask(t, ""), Status: ChatReportStatusDone, Content: "Done."})
	if err != nil {
		t.Fatalf("WriteChatReport: %v", err)
	}
	if util.UUIDToString(res.Session.ID) != oldest {
		t.Fatalf("Mika report landed in %s, want her oldest session %s", util.UUIDToString(res.Session.ID), oldest)
	}
}

func TestWriteChatReportRetrySharesRunIdentity(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	first := f.issueTask(t, "status = 'failed'")
	retry := f.issueTask(t, "retry_of_task_id = $2, attempt = 2", first.ID)
	second := f.issueTask(t, "retry_of_task_id = $2, attempt = 3", retry.ID)

	if _, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: retry, Status: ChatReportStatusInReview, Content: "First attempt report"}); err != nil {
		t.Fatalf("retry report: %v", err)
	}
	res, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: second, Status: ChatReportStatusInReview, Content: "Second attempt report"})
	if err != nil {
		t.Fatalf("second retry report: %v", err)
	}
	if !res.Deduplicated || res.Message.SourceTaskID != first.ID {
		t.Fatalf("second retry: dedup=%v source_task=%v, want dedup keyed on root %v", res.Deduplicated, res.Message.SourceTaskID, first.ID)
	}
}

func TestWriteChatReportRejections(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	chatSession := f.session(t, "")

	cases := []struct {
		name   string
		task   func() db.AgentTaskQueue
		status string
		body   string
		setup  func()
		want   error
	}{
		{name: "chat task", task: func() db.AgentTaskQueue { return f.issueTask(t, "chat_session_id = $2, issue_id = NULL", chatSession) }, want: ErrChatReportChatTask},
		{name: "no issue", task: func() db.AgentTaskQueue { return f.issueTask(t, "issue_id = NULL") }, want: ErrChatReportNoIssue},
		{name: "no originator", task: func() db.AgentTaskQueue { return f.issueTask(t, "originator_user_id = NULL") }, want: ErrChatReportNoOriginator},
		{name: "bad status", task: func() db.AgentTaskQueue { return f.issueTask(t, "") }, status: "in_progress", want: ErrChatReportInvalidStatus},
		{name: "empty content", task: func() db.AgentTaskQueue { return f.issueTask(t, "") }, body: "   ", want: ErrChatReportInvalidContent},
		{name: "content too long", task: func() db.AgentTaskQueue { return f.issueTask(t, "") }, body: strings.Repeat("字", ChatReportMaxContentRunes+1), want: ErrChatReportInvalidContent},
		{
			name: "originator left workspace",
			task: func() db.AgentTaskQueue {
				var outsider string
				f.scalar(t, &outsider, `INSERT INTO "user" (name, email) VALUES ('outsider', 'outsider-'||gen_random_uuid()||'@multica.test') RETURNING id`)
				t.Cleanup(func() { f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, outsider) })
				return f.issueTask(t, "originator_user_id = $2, accountable_user_id = $2", outsider)
			},
			want: ErrChatReportOriginatorNoAccess,
		},
		{
			name:  "access hook denies",
			task:  func() db.AgentTaskQueue { return f.issueTask(t, "") },
			setup: func() { f.svc.ChatReportAccess = func(context.Context, db.Agent, string, string) bool { return false } },
			want:  ErrChatReportOriginatorNoAccess,
		},
		{
			name:  "archived agent",
			task:  func() db.AgentTaskQueue { return f.issueTask(t, "") },
			setup: func() { f.exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.agentID) },
			want:  ErrChatReportAgentArchived,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.svc.ChatReportAccess = nil
			if tc.setup != nil {
				tc.setup()
			}
			status, body := tc.status, tc.body
			if status == "" {
				status = ChatReportStatusDone
			}
			if body == "" {
				body = "report"
			}
			_, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: tc.task(), Status: status, Content: body})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if n := len(f.reports(t)); n != 0 {
		t.Fatalf("rejected reports wrote %d rows", n)
	}
}

// agentMovedIssue records the activity row the issue:updated listener writes
// when the agent changes the issue's status, and applies the status.
func (f *chatReportFixture) agentMovedIssue(t *testing.T, status string) {
	t.Helper()
	f.exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, f.issueID, status)
	f.exec(t, `
		INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'agent', $3, 'status_changed', jsonb_build_object('from', 'in_progress', 'to', $4::text))`,
		f.workspaceID, f.issueID, f.agentID, status)
}

func (f *chatReportFixture) agentComment(t *testing.T, content string) {
	t.Helper()
	f.exec(t, `
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		VALUES ($1, $2, 'agent', $3, $4)`, f.workspaceID, f.issueID, f.agentID, content)
}

func TestCompleteTaskWritesFallbackReportOnce(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	task := f.issueTask(t, "")
	f.agentMovedIssue(t, "in_review")
	f.agentComment(t, "## Summary\n\nThe endpoint and the fallback are in.\nTests pass.\n\n## Details\n\nMore text.")

	if _, err := f.svc.CompleteTask(ctx, task.ID, []byte(`{"output":"done"}`), "", "", "", false, "", ""); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	reports := f.reports(t)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1 fallback", len(reports))
	}
	want := "REP-7 Ship the report endpoint: ready for review\n\nThe endpoint and the fallback are in.\nTests pass."
	if reports[0].Content != want || reports[0].ReportStatus.String != ChatReportStatusInReview {
		t.Fatalf("fallback = %q (%s), want %q", reports[0].Content, reports[0].ReportStatus.String, want)
	}

	// A second terminal callback, or any later fallback pass for the same run,
	// must not write another report.
	if _, err := f.svc.CompleteTask(ctx, task.ID, []byte(`{"output":"done"}`), "", "", "", false, "", ""); err != nil {
		t.Fatalf("replayed CompleteTask: %v", err)
	}
	f.svc.reportTaskOutcomeFallback(ctx, task, false)
	if n := len(f.reports(t)); n != 1 {
		t.Fatalf("reports after replay = %d, want 1", n)
	}
}

func TestCompleteTaskFallbackStandsDownAfterAgentReport(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	task := f.issueTask(t, "")
	f.agentMovedIssue(t, "done")
	// The agent reported a different status than the one the issue ends in;
	// any report in the run silences the fallback.
	if _, err := f.svc.WriteChatReport(ctx, ChatReportRequest{Task: task, Status: ChatReportStatusInReview, Content: "My own words."}); err != nil {
		t.Fatalf("agent report: %v", err)
	}
	if _, err := f.svc.CompleteTask(ctx, task.ID, []byte(`{"output":"done"}`), "", "", "", false, "", ""); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	reports := f.reports(t)
	if len(reports) != 1 || reports[0].Content != "My own words." {
		t.Fatalf("reports = %+v, want only the agent's own", reports)
	}
}

func TestCompleteTaskFallbackOnlyForStatusTheRunProduced(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	// The issue was already in_review before this comment-only run started.
	f.exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, f.issueID)
	f.exec(t, `
		INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		VALUES ($1, $2, 'agent', $3, 'status_changed', '{"from":"in_progress","to":"in_review"}', now() - interval '1 day')`,
		f.workspaceID, f.issueID, f.agentID)
	task := f.issueTask(t, "")
	if _, err := f.svc.CompleteTask(ctx, task.ID, []byte(`{"output":"answered"}`), "", "", "", false, "", ""); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	if n := len(f.reports(t)); n != 0 {
		t.Fatalf("reports = %d, want none for a run that did not move the issue", n)
	}

	// A member moving the issue during the run is not the agent's outcome either.
	task = f.issueTask(t, "")
	f.exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, f.issueID)
	f.exec(t, `
		INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{"from":"in_review","to":"done"}')`,
		f.workspaceID, f.issueID, f.userID)
	if _, err := f.svc.CompleteTask(ctx, task.ID, []byte(`{"output":"ok"}`), "", "", "", false, "", ""); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	if n := len(f.reports(t)); n != 0 {
		t.Fatalf("reports = %d, want none for a member's status change", n)
	}
}

func TestFailTaskWritesFailedFallbackInOriginatorLanguage(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	f.exec(t, `UPDATE "user" SET language = 'zh-Hans' WHERE id = $1`, f.userID)
	task := f.issueTask(t, "")

	if _, err := f.svc.FailTask(ctx, task.ID, "agent crashed", "", "", "", "agent_error", false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	reports := f.reports(t)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	if reports[0].ReportStatus.String != ChatReportStatusFailed || reports[0].Content != "REP-7 Ship the report endpoint：运行失败" {
		t.Fatalf("failed fallback = %q (%s)", reports[0].Content, reports[0].ReportStatus.String)
	}
}

func TestFailTaskWithRetryPendingDoesNotReport(t *testing.T) {
	f := seedChatReportFixture(t)
	ctx := context.Background()
	f.exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issueID)
	task := f.issueTask(t, "max_attempts = 3")

	if _, err := f.svc.FailTask(ctx, task.ID, "provider network blip", "", "", "", string(taskfailure.ReasonAgentProviderNetwork), false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	var retries int
	f.scalar(t, &retries, `SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id = $1`, task.ID)
	if retries != 1 {
		t.Fatalf("retries = %d, want 1 so the report is held for the retry", retries)
	}
	if n := len(f.reports(t)); n != 0 {
		t.Fatalf("reports = %d, want none while a retry is pending", n)
	}
}

func TestFirstParagraph(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"## Heading\n\nBody text.":      "Body text.",
		"Line one\nline two\n\nNext":    "Line one\nline two",
		"\r\n\r\nWindows\r\n\r\nsecond": "Windows",
		"# Only a heading":              "",
		strings.Repeat("a", 700) + "\n": strings.Repeat("a", fallbackExcerptMaxRunes) + "…",
	}
	for in, want := range cases {
		if got := firstParagraph(in); got != want {
			t.Errorf("firstParagraph(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildFallbackChatReport(t *testing.T) {
	if got := buildFallbackChatReport("en", "MUL-1", "Fix login", ChatReportStatusBlocked, ""); got != "MUL-1 Fix login: blocked" {
		t.Errorf("en = %q", got)
	}
	if got := buildFallbackChatReport("zh-CN", "", "修复登录", ChatReportStatusDone, "已合并。"); got != "修复登录：已完成\n\n已合并。" {
		t.Errorf("zh = %q", got)
	}
}
