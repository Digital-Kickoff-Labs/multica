package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	exportTestIssueID      = "11111111-1111-1111-1111-111111111111"
	exportTestRunID        = "22222222-2222-2222-2222-222222222222"
	exportTestAttachmentID = "33333333-3333-3333-3333-333333333333"
	exportTestChatFileID   = "44444444-4444-4444-4444-444444444444"

	exportTestAutopilotRuns = 230

	exportTestWorkspaceAvatar = "/api/avatars/c2lnLXdz/workspaces/ws-1/55555555-5555-5555-5555-555555555555.png"
	exportTestAgentAvatar     = "/api/avatars/c2lnLWFn/workspaces/ws-1/66666666-6666-6666-6666-666666666666.webp"
	exportTestOAuthAvatar     = "https://avatars.example.com/u/1.png"
)

func newExportTestServer(t *testing.T, requested *sync.Map) *httptest.Server {
	t.Helper()
	routes := map[string]any{
		"/api/config":                  map[string]any{"version": "test"},
		"/api/workspaces/ws-1":         map[string]any{"id": "ws-1", "name": "Acme", "avatar_url": exportTestWorkspaceAvatar},
		"/api/workspaces/ws-1/members": []any{map[string]any{"id": "m-1", "user_id": "u-1", "email": "a@example.com", "avatar_url": exportTestOAuthAvatar}},
		"/api/labels":                  []any{map[string]any{"id": "l-1", "name": "bug"}},
		"/api/agents":                  []any{map[string]any{"id": "a-1", "name": "Builder"}},
		"/api/agents/a-1":              map[string]any{"id": "a-1", "name": "Builder", "instructions": "Ship it", "avatar_url": exportTestAgentAvatar},
		"/api/agents/a-1/env":          map[string]any{"env": map[string]any{"TOKEN": "secret"}},
		"/api/skills":                  []any{},
		"/api/docs":                    map[string]any{"folders": []any{map[string]any{"id": "f-1"}}, "docs": []any{map[string]any{"id": "d-1"}}},
		"/api/docs/d-1":                map[string]any{"id": "d-1", "title": "Runbook"},
		"/api/chat/sessions":           []any{map[string]any{"id": "c-1"}},
		"/api/chat/sessions/c-1/messages": []any{map[string]any{
			"id": "cm-1", "content": "see /api/attachments/" + exportTestChatFileID + "/download",
		}},
		"/api/issues": map[string]any{"issues": []any{map[string]any{"id": exportTestIssueID}}, "total": 1},
		"/api/issues/" + exportTestIssueID: map[string]any{
			"id": exportTestIssueID, "number": 7, "creator_id": "u-1", "created_at": "2026-01-02T03:04:05Z",
		},
		"/api/issues/" + exportTestIssueID + "/comments": []any{map[string]any{
			"id": "co-1", "author_id": "u-1", "content": "hello",
		}},
		"/api/issues/" + exportTestIssueID + "/timeline":    []any{map[string]any{"id": "ac-1", "type": "activity"}},
		"/api/issues/" + exportTestIssueID + "/attachments": []any{map[string]any{"id": exportTestAttachmentID}},
		"/api/issues/" + exportTestIssueID + "/task-runs":   []any{map[string]any{"id": exportTestRunID, "status": "completed"}},
		"/api/tasks/" + exportTestRunID + "/messages":       []any{map[string]any{"seq": 1, "content": "working"}},
		"/api/autopilots": []any{map[string]any{"id": "ap-1"}},
		"/api/agents/a-1/tasks": []any{
			map[string]any{"id": "t-chat", "issue_id": ""},
			map[string]any{"id": exportTestRunID, "issue_id": exportTestIssueID},
		},
		"/api/tasks/t-chat/messages":                 []any{map[string]any{"seq": 1}, map[string]any{"seq": 2}},
		"/api/attachments/" + exportTestAttachmentID: map[string]any{"id": exportTestAttachmentID, "filename": "../../shot.png"},
		"/api/attachments/" + exportTestChatFileID:   map[string]any{"id": exportTestChatFileID, "filename": "chat.txt"},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path, true)
		requested.Store(r.URL.RequestURI(), true)
		switch r.URL.Path {
		case "/api/autopilots/ap-1/runs":
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			var runs []any
			for i := offset; i < exportTestAutopilotRuns && i < offset+100; i++ {
				runs = append(runs, map[string]any{"id": fmt.Sprintf("run-%d", i)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs, "total": exportTestAutopilotRuns})
			return
		case "/api/attachments/" + exportTestAttachmentID + "/download":
			_, _ = w.Write([]byte("PNGDATA"))
			return
		case exportTestWorkspaceAvatar, exportTestAgentAvatar:
			_, _ = w.Write([]byte("AVATAR:" + r.URL.Path))
			return
		case "/api/attachments/" + exportTestChatFileID + "/download":
			_, _ = w.Write([]byte("CHATDATA"))
			return
		case "/api/issues/" + exportTestIssueID + "/comments":
			w.Header().Set("X-Comments-Truncated", "true")
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func runExportForTest(t *testing.T, flags map[string]string) (string, *sync.Map) {
	t.Helper()
	requested := &sync.Map{}
	srv := newExportTestServer(t, requested)
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	out := filepath.Join(t.TempDir(), "export")
	cmd := workspaceExportCmd
	cmd.SetContext(context.Background())
	defaults := map[string]string{"out": out, "concurrency": "2", "include-secrets": "false",
		"skip-transcripts": "false", "skip-attachments": "false"}
	for name, value := range flags {
		defaults[name] = value
	}
	for name, value := range defaults {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		_ = cmd.Flags().Set("include-secrets", "false")
		_ = cmd.Flags().Set("concurrency", "4")
	})
	if err := runWorkspaceExport(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceExport: %v", err)
	}
	return out, requested
}

func readExportManifest(t *testing.T, out string) exportManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest exportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return manifest
}

func TestWorkspaceExportWritesIssueWithCommentsRunsAndAttachments(t *testing.T) {
	out, _ := runExportForTest(t, nil)

	data, err := os.ReadFile(filepath.Join(out, "issues", exportTestIssueID+".json"))
	if err != nil {
		t.Fatalf("issue file missing: %v", err)
	}
	var issue struct {
		Issue    map[string]any   `json:"issue"`
		Comments []map[string]any `json:"comments"`
		TaskRuns []struct {
			Run      map[string]any   `json:"run"`
			Messages []map[string]any `json:"messages"`
		} `json:"task_runs"`
	}
	if err := json.Unmarshal(data, &issue); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	if issue.Issue["creator_id"] != "u-1" || issue.Issue["created_at"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("issue lost authorship or dates: %v", issue.Issue)
	}
	if len(issue.Comments) != 1 || issue.Comments[0]["author_id"] != "u-1" {
		t.Fatalf("comments = %v", issue.Comments)
	}
	if len(issue.TaskRuns) != 1 || len(issue.TaskRuns[0].Messages) != 1 {
		t.Fatalf("task runs = %+v", issue.TaskRuns)
	}

	got, err := os.ReadFile(filepath.Join(out, "attachments", exportTestAttachmentID, "shot.png"))
	if err != nil || string(got) != "PNGDATA" {
		t.Fatalf("issue attachment not saved under its sanitized name: %q, %v", got, err)
	}
	chat, err := os.ReadFile(filepath.Join(out, "attachments", exportTestChatFileID, "chat.txt"))
	if err != nil || string(chat) != "CHATDATA" {
		t.Fatalf("chat-referenced attachment not saved: %q, %v", chat, err)
	}
}

func TestWorkspaceExportManifestRecordsCountsTruncationsAndErrors(t *testing.T) {
	out, _ := runExportForTest(t, nil)
	manifest := readExportManifest(t, out)

	if manifest.Format != workspaceExportFormat || manifest.FormatVersion != workspaceExportFormatVersion {
		t.Fatalf("format = %s v%d", manifest.Format, manifest.FormatVersion)
	}
	for entity, want := range map[string]int{"issues": 1, "comments": 1, "task_runs": 1, "task_messages": 3, "agent_tasks": 1,
		"members": 1, "agents": 1, "docs": 1, "chat_sessions": 1, "attachments": 2} {
		if manifest.Counts[entity] != want {
			t.Errorf("counts[%s] = %d, want %d", entity, manifest.Counts[entity], want)
		}
	}
	if len(manifest.Truncations) != 1 || !strings.HasSuffix(manifest.Truncations[0].Path, "/comments") {
		t.Fatalf("truncations = %+v", manifest.Truncations)
	}
	var sawMissingFeature bool
	for _, e := range manifest.Errors {
		if e.Path == "/api/issue-statuses" {
			sawMissingFeature = true
		}
	}
	if !sawMissingFeature {
		t.Fatalf("a 404 endpoint should be recorded, not abort the export: %+v", manifest.Errors)
	}
}

func TestWorkspaceExportReadsAgentSecretsOnlyWhenRequested(t *testing.T) {
	_, requested := runExportForTest(t, nil)
	if _, ok := requested.Load("/api/agents/a-1/env"); ok {
		t.Fatal("agent env was read without --include-secrets")
	}

	out, requested := runExportForTest(t, map[string]string{"include-secrets": "true"})
	if _, ok := requested.Load("/api/agents/a-1/env"); !ok {
		t.Fatal("agent env not read with --include-secrets")
	}
	data, _ := os.ReadFile(filepath.Join(out, "agents.jsonl"))
	if !strings.Contains(string(data), `"TOKEN":"secret"`) {
		t.Fatalf("agents.jsonl missing env: %s", data)
	}
	info, err := os.Stat(filepath.Join(out, "agents.jsonl"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("agents.jsonl must be owner-only, got %v (%v)", info.Mode().Perm(), err)
	}
}

func TestWorkspaceExportResumeSkipsExportedIssues(t *testing.T) {
	out, _ := runExportForTest(t, nil)

	requested := &sync.Map{}
	srv := newExportTestServer(t, requested)
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	_ = workspaceExportCmd.Flags().Set("out", out)
	if err := runWorkspaceExport(workspaceExportCmd, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, ok := requested.Load("/api/issues/" + exportTestIssueID + "/comments"); ok {
		t.Fatal("resume re-read an issue already exported")
	}
}

func TestWorkspaceExportResumeRetriesHalfWrittenAttachment(t *testing.T) {
	out, _ := runExportForTest(t, nil)
	dir := filepath.Join(out, "attachments", exportTestChatFileID)
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "chat.txt")); err != nil {
		t.Fatal(err)
	}

	requested := &sync.Map{}
	srv := newExportTestServer(t, requested)
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	_ = workspaceExportCmd.Flags().Set("out", out)
	if err := runWorkspaceExport(workspaceExportCmd, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "chat.txt")); err != nil || string(data) != "CHATDATA" {
		t.Fatalf("attachment without meta.json was not downloaded again: %q, %v", data, err)
	}
	if _, ok := requested.Load("/api/attachments/" + exportTestAttachmentID + "/download"); ok {
		t.Fatal("resume re-downloaded a complete attachment")
	}
}

func TestWorkspaceExportKeepsOnlyIssuelessAgentTasks(t *testing.T) {
	out, _ := runExportForTest(t, nil)
	data, err := os.ReadFile(filepath.Join(out, "agent_tasks", "a-1.jsonl"))
	if err != nil {
		t.Fatalf("agent tasks file missing: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"t-chat"`) || !strings.Contains(lines[0], `"seq":2`) {
		t.Fatalf("want only the chat task with its transcript, got %s", data)
	}
}

func TestWorkspaceExportReadsEveryAutopilotRunPage(t *testing.T) {
	out, _ := runExportForTest(t, nil)
	data, err := os.ReadFile(filepath.Join(out, "autopilots.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Runs struct {
			Runs []map[string]any `json:"runs"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Runs.Runs) != exportTestAutopilotRuns {
		t.Fatalf("exported %d autopilot runs, want all %d", len(record.Runs.Runs), exportTestAutopilotRuns)
	}
}

func TestListItemsAcceptsBareAndWrappedLists(t *testing.T) {
	if got := listItems(json.RawMessage(`[{"id":"a"}]`)); len(got) != 1 {
		t.Fatalf("bare list = %v", got)
	}
	if got := listItems(json.RawMessage(`{"total":2,"issues":[{"id":"a"},{"id":"b"}]}`)); len(got) != 2 {
		t.Fatalf("wrapped list = %v", got)
	}
	if got := listItems(json.RawMessage(`{"id":"a"}`)); got != nil {
		t.Fatalf("single object should yield no items, got %v", got)
	}
}

func TestWorkspaceExportIncludesArchivedChats(t *testing.T) {
	_, requested := runExportForTest(t, nil)
	if _, ok := requested.Load("/api/chat/sessions?status=all"); !ok {
		t.Fatal("chat sessions must be listed with status=all, or archived chats are lost")
	}
}

func TestWorkspaceExportDownloadsServedAvatarsOnly(t *testing.T) {
	out, requested := runExportForTest(t, nil)

	data, err := os.ReadFile(filepath.Join(out, "avatars", "index.jsonl"))
	if err != nil {
		t.Fatalf("avatar index missing: %v", err)
	}
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry exportedAvatar
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		files[entry.URL] = entry.File
	}
	if len(files) != 2 || files[exportTestOAuthAvatar] != "" {
		t.Fatalf("index = %v, want the workspace and agent avatars only", files)
	}
	for _, avatar := range []string{exportTestWorkspaceAvatar, exportTestAgentAvatar} {
		got, err := os.ReadFile(filepath.Join(out, "avatars", files[avatar]))
		if err != nil || string(got) != "AVATAR:"+avatar {
			t.Fatalf("avatar %s = %q, %v", avatar, got, err)
		}
		if !strings.HasSuffix(files[avatar], filepath.Base(avatar)) {
			t.Fatalf("file %q must keep the object name %q", files[avatar], filepath.Base(avatar))
		}
	}
	if _, ok := requested.Load("/u/1.png"); ok {
		t.Fatal("third-party avatars must not be downloaded")
	}
	if got := readExportManifest(t, out).Counts["avatars"]; got != 2 {
		t.Fatalf("manifest avatars count = %d", got)
	}
}
