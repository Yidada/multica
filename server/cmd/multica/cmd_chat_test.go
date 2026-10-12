package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newChatReportTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "report"}
	addChatReportFlags(cmd)
	return cmd
}

func TestRunChatReportPostsStatusAndFileContent(t *testing.T) {
	t.Chdir(t.TempDir())
	const content = "CLE-147 is ready for review.\n\n中文 works too.\n"
	if err := os.WriteFile("report.md", []byte(content), 0o644); err != nil {
		t.Fatalf("write report file: %v", err)
	}
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat/report" {
			t.Fatalf("request = %s %s, want POST /api/chat/report", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"message_id": "m1", "chat_session_id": "s1", "deduplicated": false})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_TASK_ID", "task-1")

	cmd := newChatReportTestCmd()
	_ = cmd.Flags().Set("status", "in_review")
	_ = cmd.Flags().Set("content-file", "report.md")
	out, err := captureStdout(t, func() error { return runChatReport(cmd, nil) })
	if err != nil {
		t.Fatalf("runChatReport: %v", err)
	}
	if body["status"] != "in_review" || body["content"] != strings.TrimSuffix(content, "\n") {
		t.Fatalf("body = %#v", body)
	}
	if _, ok := body["issue_id"]; ok {
		t.Fatalf("issue_id sent without --issue: %#v", body)
	}
	if !strings.Contains(out, `"message_id": "m1"`) {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunChatReportRejectsBeforeCallingServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cases := []struct {
		name    string
		inTask  bool
		status  string
		content string
		want    string
	}{
		{name: "outside a task", status: "done", content: "x", want: "only works inside an agent task"},
		{name: "missing status", inTask: true, content: "x", want: "--status is required"},
		{name: "unknown status", inTask: true, status: "in_progress", content: "x", want: "--status must be one of"},
		{name: "missing content", inTask: true, status: "done", want: "--content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MULTICA_AGENT_ID", "")
			t.Setenv("MULTICA_TASK_ID", "")
			t.Setenv("MULTICA_DAEMON_PORT", "")
			t.Chdir(t.TempDir())
			if tc.inTask {
				t.Setenv("MULTICA_TASK_ID", "task-1")
			}
			cmd := newChatReportTestCmd()
			if tc.status != "" {
				_ = cmd.Flags().Set("status", tc.status)
			}
			if tc.content != "" {
				_ = cmd.Flags().Set("content", tc.content)
			}
			err := runChatReport(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
