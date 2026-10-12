package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Work with the current chat conversation",
}

var chatHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Overview of the channel this conversation is in (messages + thread list)",
	Long: `Show the overview of the chat channel (e.g. Slack) this conversation is in: the
recent top-level messages, and for each thread its thread_id, reply_count, and
latest_reply. It does NOT expand thread contents — it is the table of contents.

To read a specific thread's messages, take a thread_id from here and run
"multica chat thread <thread_id>".

It is the SAME command regardless of which channel the conversation came from,
and it reads only the conversation you are currently running for — it cannot
read any other session or channel.`,
	Args: cobra.NoArgs,
	RunE: runChatHistory,
}

var chatThreadCmd = &cobra.Command{
	Use:   "thread [id]",
	Short: "Read one thread's messages (the current thread, or a specific id)",
	Long: `Read the messages of a single thread.

With no id, read the thread you are currently in (the one you were @mentioned in).
With an id — a thread_id from "multica chat history" — read that specific thread.
Either way the thread is within the channel you are in; you cannot read another
channel.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runChatThread,
}

var chatReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Tell the person who started this task, in chat, how it ended",
	Long: `Write a short report into your chat with the person at the top of this task
(the task's originator): one or two sentences in your own voice about the
outcome, e.g. "CLE-147 is ready for review: the endpoint and fallback are in,
tests pass."

Call it when the issue moves to in_review, done or blocked. The chat session,
the agent and the recipient all come from your task token; you cannot choose
them. It does not start a new run. Repeating the same --status for the same
task is a no-op that returns the existing report. If the run ends without a
report, the server writes a fixed-format one instead.

Only works inside an agent task on an issue (MULTICA_TOKEN must be a mat_
task token).`,
	Args: cobra.NoArgs,
	RunE: runChatReport,
}

func init() {
	addChatReportFlags(chatReportCmd)
	chatCmd.AddCommand(chatReportCmd)

	for _, c := range []*cobra.Command{chatHistoryCmd, chatThreadCmd} {
		c.Flags().Int("limit", 0, "Maximum number of messages to return (the server clamps the range)")
		c.Flags().String("before", "", "Opaque cursor (a next_cursor from a prior page) to read older messages")
		c.Flags().String("output", "json", "Output format: table or json")
	}
	chatCmd.AddCommand(chatHistoryCmd)
	chatCmd.AddCommand(chatThreadCmd)
}

func runChatHistory(cmd *cobra.Command, _ []string) error {
	resp, err := fetchChatRead(cmd, "/api/chat/history", "")
	if err != nil {
		return err
	}
	return renderChatRead(cmd, resp, true)
}

func runChatThread(cmd *cobra.Command, args []string) error {
	threadID := ""
	if len(args) == 1 {
		threadID = args[0]
	}
	resp, err := fetchChatRead(cmd, "/api/chat/thread", threadID)
	if err != nil {
		return err
	}
	return renderChatRead(cmd, resp, false)
}

// fetchChatRead builds the request (shared --limit/--before paging, plus the
// optional thread id) and decodes the response.
func fetchChatRead(cmd *cobra.Command, basePath, threadID string) (map[string]any, error) {
	client, err := newAPIClient(cmd)
	if err != nil {
		return nil, err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	limit, _ := cmd.Flags().GetInt("limit")
	before, _ := cmd.Flags().GetString("before")

	q := url.Values{}
	if threadID != "" {
		q.Set("id", threadID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		q.Set("before", before)
	}
	path := basePath
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var resp map[string]any
	if err := client.GetJSON(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("read chat: %w", err)
	}
	return resp, nil
}

// renderChatRead prints the response as JSON (default) or a table. The overview
// table adds the thread columns so the agent can pick a thread_id to drill into.
func renderChatRead(cmd *cobra.Command, resp map[string]any, overview bool) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	if note := strVal(resp, "note"); note != "" {
		fmt.Fprintln(os.Stdout, note)
		return nil
	}
	msgs, _ := resp["messages"].([]any)
	var headers []string
	if overview {
		headers = []string{"TS", "ROLE", "AUTHOR", "THREAD_ID", "REPLIES", "TEXT"}
	} else {
		headers = []string{"TS", "ROLE", "AUTHOR", "TEXT"}
	}
	rows := make([][]string, 0, len(msgs))
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		if overview {
			rows = append(rows, []string{strVal(m, "ts"), strVal(m, "role"), strVal(m, "author"), strVal(m, "thread_id"), numVal(m, "reply_count"), strVal(m, "text")})
		} else {
			rows = append(rows, []string{strVal(m, "ts"), strVal(m, "role"), strVal(m, "author"), strVal(m, "text")})
		}
	}
	cli.PrintTable(os.Stdout, headers, rows)
	return nil
}

// numVal renders a numeric JSON field as a string, blank when zero/absent.
func numVal(m map[string]any, key string) string {
	if v, ok := m[key].(float64); ok && v != 0 {
		return strconv.Itoa(int(v))
	}
	return ""
}

func addChatReportFlags(c *cobra.Command) {
	c.Flags().String("status", "", "Outcome being reported: in_review, done, or blocked (required)")
	c.Flags().String("content", "", "Report text (decodes \\n, \\r, \\t, \\\\)")
	c.Flags().Bool("content-stdin", false, "Read the report text from stdin")
	c.Flags().String("content-file", "", "Read the report text from a UTF-8 file. The path must be inside the current working directory unless --allow-external-file is set.")
	c.Flags().Bool("allow-external-file", false, "Allow --content-file to read a path outside the current working directory.")
	c.Flags().String("issue", "", "Optional consistency check: the issue UUID this task runs on")
	c.Flags().String("output", "json", "Output format: table or json")
}

func runChatReport(cmd *cobra.Command, _ []string) error {
	if !inDaemonManagedExecutionContext() {
		return fmt.Errorf("multica chat report only works inside an agent task")
	}
	status, _ := cmd.Flags().GetString("status")
	status = strings.TrimSpace(status)
	switch status {
	case "in_review", "done", "blocked":
	case "":
		return fmt.Errorf("--status is required (in_review, done, or blocked)")
	default:
		return fmt.Errorf("--status must be one of in_review, done, blocked; got %q", status)
	}
	content, hasContent, err := resolveTextFlag(cmd, "content")
	if err != nil {
		return err
	}
	if !hasContent || strings.TrimSpace(content) == "" {
		return fmt.Errorf("--content, --content-stdin, or --content-file is required")
	}
	if err := guardLocalPathLinks(content, "chat report",
		"Report the outcome in words; deliver files on the issue with `multica issue comment add <issue-id> --attachment <path>`."); err != nil {
		return err
	}
	issueID, _ := cmd.Flags().GetString("issue")

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	body := map[string]any{"status": status, "content": content}
	if issueID = strings.TrimSpace(issueID); issueID != "" {
		body["issue_id"] = issueID
	}
	var resp map[string]any
	if err := client.PostJSON(ctx, "/api/chat/report", body, &resp); err != nil {
		return fmt.Errorf("chat report: %w", err)
	}
	if dedup, _ := resp["deduplicated"].(bool); dedup {
		fmt.Fprintf(os.Stderr, "Already reported %s for this task; nothing new was written.\n", status)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "table" {
		cli.PrintTable(os.Stdout, []string{"MESSAGE_ID", "CHAT_SESSION_ID", "DEDUPLICATED"}, [][]string{{
			strVal(resp, "message_id"), strVal(resp, "chat_session_id"), fmt.Sprint(resp["deduplicated"]),
		}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, resp)
}
