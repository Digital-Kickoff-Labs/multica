package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type testArchive struct {
	dir         string
	workspaceID string
	userID      string
	issueID     string
	commentID   string
	attachID    string
	agentID     string
	taskID      string
}

func writeTestArchive(t *testing.T, oldURL string) testArchive {
	t.Helper()
	a := testArchive{dir: t.TempDir(), workspaceID: uuid.NewString(), userID: uuid.NewString(),
		issueID: uuid.NewString(), commentID: uuid.NewString(), attachID: uuid.NewString(),
		agentID: uuid.NewString(), taskID: uuid.NewString()}
	slug := "imp-" + a.workspaceID[:8]
	writeFile := func(name string, v any) {
		t.Helper()
		path := filepath.Join(a.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("manifest.json", map[string]any{"format": archiveFormat, "format_version": archiveFormatVersion})
	writeFile("workspace.json", map[string]any{"workspace": map[string]any{
		"id": a.workspaceID, "name": "Import test", "slug": slug, "issue_prefix": "IMP",
		"created_at": "2025-01-02T03:04:05Z",
	}})
	writeFile("members.jsonl", map[string]any{"id": uuid.NewString(), "user_id": a.userID, "role": "owner",
		"name": "Ada", "email": a.userID + "@example.com", "avatar_url": nil})
	writeFile("issues/"+a.issueID+".json", map[string]any{
		"issue": map[string]any{
			"id": a.issueID, "number": 42, "title": "Imported", "status": "todo", "priority": "none",
			"creator_type": "member", "creator_id": a.userID, "assignee_id": "", "parent_issue_id": "",
			"description": "screenshot: " + oldURL + " and https://cloud.test/" + slug + "/issues/IMP-1",
			"created_at":  "2025-02-03T04:05:06Z",
		},
		"comments": []any{map[string]any{"id": a.commentID, "author_type": "member", "author_id": a.userID,
			"content": "hello", "type": "comment", "created_at": "2025-02-04T00:00:00Z"}},
		"timeline": []any{map[string]any{"type": "comment", "id": a.commentID, "actor_id": a.userID,
			"created_at":  "2025-02-04T00:00:00Z",
			"supplements": []any{map[string]any{"task_id": a.taskID, "status": "delivered", "delivered_at": "2025-02-04T00:01:00Z"}}}},
		"task_runs": []any{map[string]any{"run": map[string]any{
			"id": a.taskID, "agent_id": a.agentID, "issue_id": a.issueID, "status": "cancelled", "completed_at": "2025-02-04T00:02:00Z",
			"created_at": "2025-02-04T00:00:00Z", "wakeup_id": a.commentID, "wakeup_system_rule": "child_done",
			"kind": "quick_create", "cancelled_by_comment_change": true,
			"supplement_capability": "task-supplement-v1",
			"cancelled_by":          map[string]any{"type": "member", "id": a.userID, "name": "Ada"},
			"attribution": map[string]any{"source": "direct_human", "precise": true,
				"initiator":  map[string]any{"id": a.userID, "name": "Ada"},
				"originator": map[string]any{"id": a.userID, "name": "Ada"},
				"evidence":   map[string]any{"kind": "comment", "ref_id": a.commentID}},
		}}},
	})
	writeFile("agents.jsonl", map[string]any{"item": map[string]any{"id": a.agentID, "name": "Bot", "runtime_mode": "local"}})
	writeFile("attachments/"+a.attachID+"/meta.json", map[string]any{"id": a.attachID, "issue_id": a.issueID,
		"uploader_type": "member", "uploader_id": a.userID, "filename": "shot.png", "url": oldURL,
		"content_type": "image/png", "size_bytes": 4})
	if err := os.WriteFile(filepath.Join(a.dir, "attachments", a.attachID, "shot.png"), []byte("PNG!"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

func testDatabase(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return dsn, conn
}

func TestImportPreservesIdsAuthorshipDatesAndRewritesLinks(t *testing.T) {
	dsn, conn := testDatabase(t)
	ctx := context.Background()
	uploads := t.TempDir()
	t.Setenv("S3_BUCKET", "")
	t.Setenv("LOCAL_UPLOAD_DIR", uploads)
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "https://self.test")

	a := writeTestArchive(t, "https://cloud.test/uploads/ws/shot.png")
	if err := run(ctx, options{archive: a.dir, databaseURL: dsn, rewrites: []string{"https://cloud.test=https://self.test"}}); err != nil {
		t.Fatalf("import: %v", err)
	}

	var number int
	var creator, description string
	var createdAt string
	var assignee *string
	if err := conn.QueryRow(ctx, `SELECT number, creator_id::text, assignee_id::text, description, to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')
		FROM issue WHERE id = $1`, a.issueID).Scan(&number, &creator, &assignee, &description, &createdAt); err != nil {
		t.Fatalf("issue not imported: %v", err)
	}
	if number != 42 || creator != a.userID || createdAt != "2025-02-03" || assignee != nil {
		t.Fatalf("issue = number %d creator %s created %s assignee %v", number, creator, createdAt, assignee)
	}
	if strings.Contains(description, "cloud.test") {
		t.Fatalf("links not rewritten: %q", description)
	}

	var counter int
	if err := conn.QueryRow(ctx, `SELECT issue_counter FROM workspace WHERE id = $1`, a.workspaceID).Scan(&counter); err != nil || counter < 42 {
		t.Fatalf("issue_counter = %d (%v), want >= 42 so new issues do not collide", counter, err)
	}
	var commentAuthor string
	if err := conn.QueryRow(ctx, `SELECT author_id::text FROM comment WHERE id = $1`, a.commentID).Scan(&commentAuthor); err != nil || commentAuthor != a.userID {
		t.Fatalf("comment author = %q (%v)", commentAuthor, err)
	}
	var attachmentURL string
	if err := conn.QueryRow(ctx, `SELECT url FROM attachment WHERE id = $1`, a.attachID).Scan(&attachmentURL); err != nil {
		t.Fatalf("attachment row: %v", err)
	}
	if attachmentURL != "https://self.test/uploads/ws/shot.png" {
		t.Fatalf("attachment url = %q", attachmentURL)
	}
	if data, err := os.ReadFile(filepath.Join(uploads, "ws", "shot.png")); err != nil || string(data) != "PNG!" {
		t.Fatalf("file not uploaded to storage: %q, %v", data, err)
	}

	var source, accountable, originator, evidenceKind, cancelledBy, wakeup, capability, supplementStatus, contextType, cancelledTask string
	if err := conn.QueryRow(ctx, `SELECT t.originator_source, t.accountable_user_id::text, t.originator_user_id::text,
		t.trigger_evidence_kind, t.cancelled_by_name, t.context->>'wakeup_system', c.capability, s.status,
		t.context->>'type', t.context->>'comment_change_cancelled_task_id'
		FROM agent_task_queue t JOIN task_supplement_capability c ON c.task_id = t.id JOIN task_supplement s ON s.task_id = t.id
		WHERE t.id = $1`, a.taskID).Scan(&source, &accountable, &originator, &evidenceKind, &cancelledBy, &wakeup, &capability, &supplementStatus,
		&contextType, &cancelledTask); err != nil {
		t.Fatalf("task provenance not imported: %v", err)
	}
	if source != "direct_human" || accountable != a.userID || originator != a.userID || evidenceKind != "comment" ||
		cancelledBy != "Ada" || wakeup != "child_done" || capability != "task-supplement-v1" || supplementStatus != "delivered" ||
		contextType != "quick_create" || cancelledTask != a.taskID {
		t.Fatalf("task provenance = %s %s %s %s %s %s %s %s", source, accountable, originator, evidenceKind, cancelledBy, wakeup, capability, supplementStatus)
	}

	if err := run(ctx, options{archive: a.dir, databaseURL: dsn}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second import must refuse an existing workspace, got %v", err)
	}
}

func TestImportDryRunAndDanglingReferencesCommitNothing(t *testing.T) {
	dsn, conn := testDatabase(t)
	ctx := context.Background()

	a := writeTestArchive(t, "https://cloud.test/uploads/ws/shot.png")
	if err := run(ctx, options{archive: a.dir, databaseURL: dsn, dryRun: true, skipFiles: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertWorkspaceAbsent(t, conn, a.workspaceID)

	orphan := writeTestArchive(t, "https://cloud.test/uploads/ws/shot.png")
	if err := os.Remove(filepath.Join(orphan.dir, "issues", orphan.issueID+".json")); err != nil {
		t.Fatal(err)
	}
	err := run(ctx, options{archive: orphan.dir, databaseURL: dsn, skipFiles: true})
	if err == nil || !strings.Contains(err.Error(), "missing from the archive") {
		t.Fatalf("attachment of a missing issue must abort the import, got %v", err)
	}
	assertWorkspaceAbsent(t, conn, orphan.workspaceID)

	if err := run(ctx, options{archive: orphan.dir, databaseURL: dsn, skipFiles: true, repairDangling: true}); err != nil {
		t.Fatalf("repair: %v", err)
	}
	var issueID *string
	if err := conn.QueryRow(ctx, `SELECT issue_id::text FROM attachment WHERE id = $1`, orphan.attachID).Scan(&issueID); err != nil {
		t.Fatalf("repair must keep the row and clear its nullable reference: %v", err)
	}
	if issueID != nil {
		t.Fatalf("attachment still points to missing issue %s", *issueID)
	}
}

func TestStorageKeyKeepsTheObjectPathOnly(t *testing.T) {
	for raw, want := range map[string]string{
		"https://static.cloud.test/workspaces/ws/a.png": "workspaces/ws/a.png",
		"https://self.test/uploads/workspaces/ws/a.png": "workspaces/ws/a.png",
		"::bad": "",
	} {
		if got := storageKey(raw); got != want {
			t.Errorf("storageKey(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestImportRefusesUnfinishedExport(t *testing.T) {
	a := writeTestArchive(t, "https://cloud.test/x.png")
	if err := os.Remove(filepath.Join(a.dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(a.dir, false); err == nil {
		t.Fatal("an archive without manifest.json must be refused")
	}
	if m, err := readManifest(a.dir, true); err != nil || m != nil {
		t.Fatalf("--allow-partial should accept it: %v, %v", m, err)
	}
}

func assertWorkspaceAbsent(t *testing.T, conn *pgx.Conn, id string) {
	t.Helper()
	var exists bool
	if err := conn.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM workspace WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("workspace %s was committed", id)
	}
}
