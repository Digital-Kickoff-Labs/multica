// Command import-workspace loads an archive produced by `multica workspace
// export` into a freshly migrated Multica database.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/issuestatus"
)

const (
	archiveFormat        = "multica-workspace-export"
	archiveFormatVersion = 1
	defaultInitialStatus = "todo"
)

type options struct {
	archive            string
	databaseURL        string
	rewrites           []string
	emailMap           map[string]string
	mergeExistingUsers bool
	dryRun             bool
	allowPartial       bool
	allowDangling      bool
	repairDangling     bool
	skipFiles          bool
	allowExisting      bool
}

type rewriteFlag struct{ values *[]string }

func (r rewriteFlag) String() string { return "" }
func (r rewriteFlag) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("--rewrite expects FROM=TO, got %q", v)
	}
	*r.values = append(*r.values, v)
	return nil
}

type emailMapFlag map[string]string

func (m emailMapFlag) String() string { return "" }
func (m emailMapFlag) Set(v string) error {
	from, to, ok := strings.Cut(v, "=")
	if !ok || !strings.Contains(from, "@") || !strings.Contains(to, "@") {
		return fmt.Errorf("--email-map expects OLD@EXAMPLE.COM=NEW@EXAMPLE.COM, got %q", v)
	}
	m[strings.ToLower(strings.TrimSpace(from))] = strings.TrimSpace(to)
	return nil
}

func main() {
	opts := options{emailMap: map[string]string{}}
	flag.StringVar(&opts.archive, "archive", "", "directory written by `multica workspace export` (required)")
	flag.Var(rewriteFlag{&opts.rewrites}, "rewrite", "FROM=TO text replacement applied to every imported row, e.g. https://multica.ai=https://multica.example.com (repeatable)")
	flag.Var(emailMapFlag(opts.emailMap), "email-map", "OLD=NEW email applied to an archived account before import (repeatable)")
	flag.BoolVar(&opts.mergeExistingUsers, "merge-existing-users", false, "attach archived accounts to target accounts that already use the same email instead of aborting")
	flag.BoolVar(&opts.dryRun, "dry-run", false, "run the whole import, report, then roll back")
	flag.BoolVar(&opts.allowPartial, "allow-partial", false, "accept an archive without manifest.json (interrupted export)")
	flag.BoolVar(&opts.allowDangling, "allow-dangling", false, "commit even when rows reference records missing from the archive")
	flag.BoolVar(&opts.repairDangling, "repair-dangling", false, "clear references to rows missing from the archive (nullable columns) or delete the referencing rows, then commit")
	flag.BoolVar(&opts.skipFiles, "skip-files", false, "import attachment rows without uploading their files")
	flag.BoolVar(&opts.allowExisting, "allow-existing", false, "import into a database that already contains this workspace (re-run)")
	flag.Parse()
	opts.databaseURL = os.Getenv("DATABASE_URL")

	if err := run(context.Background(), opts); err != nil {
		fmt.Fprintln(os.Stderr, "import failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opts options) error {
	if opts.archive == "" || opts.databaseURL == "" {
		return errors.New("--archive and DATABASE_URL are required")
	}
	manifest, err := readManifest(opts.archive, opts.allowPartial)
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, opts.databaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	var workspace struct {
		Workspace map[string]any `json:"workspace"`
	}
	if err := readJSON(filepath.Join(opts.archive, "workspace.json"), &workspace); err != nil {
		return err
	}
	workspaceID := str(workspace.Workspace, "id")
	if workspaceID == "" {
		return errors.New("workspace.json has no workspace id")
	}
	if !opts.allowExisting {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace WHERE id = $1)`, workspaceID).Scan(&exists); err != nil {
			return fmt.Errorf("check target (is the database migrated?): %w", err)
		}
		if exists {
			return fmt.Errorf("workspace %s already exists in the target; pass --allow-existing to re-run", workspaceID)
		}
	}

	members, err := readMembers(opts.archive, opts.emailMap)
	if err != nil {
		return err
	}
	conflicts, err := findAccountConflicts(ctx, conn, members)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 && !opts.mergeExistingUsers {
		return accountConflictError(conflicts)
	}

	fs, err := openFileStore(opts)
	if err != nil {
		return err
	}
	files, err := uploadAttachments(ctx, opts.archive, fs)
	if err != nil {
		return err
	}
	avatars, err := uploadAvatars(ctx, opts.archive, workspaceID, fs)
	if err != nil {
		return err
	}
	pairs := contentLinkPairs(files)
	for _, c := range conflicts {
		pairs = append(pairs, c.archivedID, c.existingID)
	}
	for _, r := range opts.rewrites {
		from, to, _ := strings.Cut(r, "=")
		pairs = append(pairs, from, to)
	}
	storedURLs := map[string]string{}
	for _, f := range files {
		if f.newURL != "" {
			storedURLs[f.id] = f.newURL
		}
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Triggers would enqueue agent wakeups and inbox notifications for every
	// historical comment; replica mode also defers FK checks to checkDangling.
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return fmt.Errorf("set session_replication_role (needs a superuser role): %w", err)
	}

	im := &importer{tx: tx, workspaceID: workspaceID, replacer: strings.NewReplacer(pairs...),
		columns: map[string]map[string]column{}, stats: map[string]*tableStats{},
		emailMap: opts.emailMap, ownerID: ownerID(members), avatars: avatars, storedURLs: storedURLs, files: fs}
	for _, c := range conflicts {
		im.manual = append(im.manual, fmt.Sprintf("account %s: merged into the existing target account %s", c.email, c.existingID))
	}
	if err := im.importArchive(ctx, opts.archive, workspace.Workspace); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace SET issue_counter = GREATEST(issue_counter,
		(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) WHERE id = $1`, workspaceID); err != nil {
		return fmt.Errorf("issue counter: %w", err)
	}
	dangling, err := im.checkDangling(ctx, false)
	if err != nil {
		return err
	}
	if len(dangling) > 0 && opts.repairDangling {
		im.repaired = dangling
		for pass := 0; len(dangling) > 0; pass++ {
			if pass == 10 {
				return errors.New("dangling references still present after 10 repair passes")
			}
			if _, err := im.checkDangling(ctx, true); err != nil {
				return err
			}
			if dangling, err = im.checkDangling(ctx, false); err != nil {
				return err
			}
		}
	}

	im.printReport(manifest, dangling)
	if len(dangling) > 0 && !opts.allowDangling {
		return fmt.Errorf("%d foreign keys point to rows missing from the archive; nothing was committed", len(dangling))
	}
	if opts.dryRun {
		fmt.Println("dry run: rolled back")
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	fmt.Println("import committed")
	return nil
}

type tableStats struct {
	inserted, existing int
	droppedKeys        map[string]bool
}

type importer struct {
	tx          pgx.Tx
	workspaceID string
	replacer    *strings.Replacer
	columns     map[string]map[string]column
	stats       map[string]*tableStats
	manual      []string
	repaired    []string
	emailMap    map[string]string
	ownerID     string
	avatars     map[string]storedAvatar
	storedURLs  map[string]string
	files       *fileStore
}

func (im *importer) importArchive(ctx context.Context, dir string, workspace map[string]any) error {
	var ws struct {
		MCPServers json.RawMessage `json:"mcp_servers"`
	}
	_ = readJSON(filepath.Join(dir, "workspace.json"), &ws)
	if err := im.importAvatar(ctx, workspace, "workspace "+str(workspace, "name"), im.ownerID); err != nil {
		return err
	}
	if err := im.insert(ctx, "workspace", workspace); err != nil {
		return err
	}
	for _, server := range items(ws.MCPServers) {
		im.manual = append(im.manual, fmt.Sprintf("workspace MCP server %q: config is not readable through the API", str(server, "name")))
	}

	if err := eachLine(dir, "members.jsonl", func(m map[string]any) error {
		m["email"] = mappedEmail(str(m, "email"), im.emailMap)
		user := map[string]any{"id": m["user_id"], "name": m["name"], "email": m["email"], "avatar_url": m["avatar_url"]}
		if err := im.importAvatar(ctx, user, "user "+str(m, "email"), m["user_id"]); err != nil {
			return err
		}
		if err := im.insert(ctx, "user", user); err != nil {
			return err
		}
		return im.insert(ctx, "member", m)
	}); err != nil {
		return err
	}

	for file, table := range map[string]string{
		"labels.jsonl": "issue_label", "properties.jsonl": "issue_property",
		"quick_actions.jsonl": "quick_action", "issue_views.jsonl": "issue_view", "pins.jsonl": "pinned_item",
		"runtimes.jsonl": "agent_runtime",
	} {
		if err := eachLine(dir, file, func(row map[string]any) error { return im.insert(ctx, table, row) }); err != nil {
			return err
		}
	}
	if err := eachLine(dir, "issue_statuses.jsonl", func(row map[string]any) error {
		if category, ok := issuestatus.ParseCategory(str(row, "category")); ok {
			row["category"] = category
		}
		return im.insert(ctx, "issue_status", row)
	}); err != nil {
		return err
	}
	if err := eachLine(dir, "issue_workflows.jsonl", func(row map[string]any) error {
		if _, ok := row["initial_status_key"]; !ok {
			row["initial_status_key"] = defaultInitialStatus
		}
		return im.insert(ctx, "issue_workflow", row)
	}); err != nil {
		return err
	}

	steps := []func(context.Context, string) error{
		im.importAgents, im.importSkills, im.importSquads, im.importProjects,
		im.importAutopilots, im.importChats, im.importAgentTasks, im.importDocs, im.importIssues, im.importAttachments,
	}
	for _, step := range steps {
		if err := step(ctx, dir); err != nil {
			return err
		}
	}
	return nil
}

// importAvatar points avatar_url at the copy stored by uploadAvatars, backed by
// the unbound image attachment row that makes it publishable on the target.
func (im *importer) importAvatar(ctx context.Context, row map[string]any, owner string, uploaderID any) error {
	raw, _ := row["avatar_url"].(string)
	if avatar, ok := im.avatars[raw]; ok {
		row["avatar_url"] = avatar.url
		return im.insert(ctx, "attachment", map[string]any{"id": avatar.id, "uploader_type": "member", "uploader_id": uploaderID,
			"filename": avatar.filename, "url": avatar.url, "content_type": avatar.contentType, "size_bytes": avatar.size})
	}
	if isServedAvatarURL(raw) {
		row["avatar_url"] = nil
		im.manual = append(im.manual, owner+": avatar is not in the archive, cleared")
	}
	return nil
}

func (im *importer) importAgents(ctx context.Context, dir string) error {
	return eachLine(dir, "agents.jsonl", func(rec map[string]any) error {
		agent := detailOrItem(rec)
		var env struct {
			CustomEnv map[string]string `json:"custom_env"`
		}
		if raw, ok := rec["env"]; ok && remarshal(raw, &env) == nil && env.CustomEnv != nil {
			agent["custom_env"] = env.CustomEnv
		}
		if err := im.importAvatar(ctx, agent, "agent "+str(agent, "name"), im.ownerID); err != nil {
			return err
		}
		if err := im.insert(ctx, "agent", agent); err != nil {
			return err
		}
		id := agent["id"]
		if err := im.insertAll(ctx, "agent_invocation_target", items(agent["invocation_targets"]), map[string]any{"agent_id": id}); err != nil {
			return err
		}
		if err := im.insertLinks(ctx, "agent_skill", rec["skills"], map[string]any{"agent_id": id}, "skill_id"); err != nil {
			return err
		}
		if err := im.insertLinks(ctx, "agent_to_label", rec["labels"], map[string]any{"agent_id": id}, "label_id"); err != nil {
			return err
		}
		if servers := items(rec["mcp_servers"]); len(servers) > 0 {
			im.manual = append(im.manual, fmt.Sprintf("agent %q: re-attach %d MCP server(s)", str(agent, "name"), len(servers)))
		}
		return nil
	})
}

func (im *importer) importSkills(ctx context.Context, dir string) error {
	return eachLine(dir, "skills.jsonl", func(rec map[string]any) error {
		skill := detailOrItem(rec)
		if err := im.insert(ctx, "skill", skill); err != nil {
			return err
		}
		parent := map[string]any{"skill_id": skill["id"]}
		if err := im.insertAll(ctx, "skill_file", items(rec["files"]), parent); err != nil {
			return err
		}
		versions := items(rec["versions"])
		for _, v := range versions {
			if _, ok := v["content_hash"]; !ok {
				v["content_hash"] = skillVersionHash(v)
			}
		}
		if err := im.insertAll(ctx, "skill_version", versions, parent); err != nil {
			return err
		}
		return im.insertLinks(ctx, "skill_to_label", rec["labels"], parent, "label_id")
	})
}

// skillVersionHash mirrors handler.skillVersionHash; the API does not expose
// the stored hash, and the server compares it to skip no-op versions.
func skillVersionHash(version map[string]any) string {
	type file struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	files := []file{}
	_ = remarshal(version["files"], &files)
	payload, _ := json.Marshal(struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
		Files       []file `json:"files"`
	}{str(version, "name"), str(version, "description"), str(version, "content"), files})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (im *importer) importSquads(ctx context.Context, dir string) error {
	return eachLine(dir, "squads.jsonl", func(rec map[string]any) error {
		squad := detailOrItem(rec)
		if err := im.importAvatar(ctx, squad, "squad "+str(squad, "name"), im.ownerID); err != nil {
			return err
		}
		if err := im.insert(ctx, "squad", squad); err != nil {
			return err
		}
		return im.insertAll(ctx, "squad_member", items(rec["members"]), map[string]any{"squad_id": squad["id"]})
	})
}

func (im *importer) importProjects(ctx context.Context, dir string) error {
	return eachLine(dir, "projects.jsonl", func(rec map[string]any) error {
		project := detailOrItem(rec)
		if err := im.insert(ctx, "project", project); err != nil {
			return err
		}
		parent := map[string]any{"project_id": project["id"]}
		if err := im.insertAll(ctx, "project_member", items(rec["members"]), parent); err != nil {
			return err
		}
		return im.insertAll(ctx, "project_resource", items(rec["resources"]), parent)
	})
}

func (im *importer) importAutopilots(ctx context.Context, dir string) error {
	return eachLine(dir, "autopilots.jsonl", func(rec map[string]any) error {
		autopilot := asMap(rec["item"])
		var detail struct {
			Autopilot     map[string]any  `json:"autopilot"`
			Triggers      json.RawMessage `json:"triggers"`
			Collaborators json.RawMessage `json:"collaborators"`
		}
		if remarshal(rec["detail"], &detail) == nil && detail.Autopilot != nil {
			autopilot = detail.Autopilot
		}
		if err := im.insert(ctx, "autopilot", autopilot); err != nil {
			return err
		}
		parent := map[string]any{"autopilot_id": autopilot["id"]}
		if err := im.insertAll(ctx, "autopilot_subscriber", items(autopilot["subscribers"]), parent); err != nil {
			return err
		}
		if err := im.insertAll(ctx, "autopilot_trigger", items(detail.Triggers), parent); err != nil {
			return err
		}
		if err := im.insertAll(ctx, "autopilot_collaborator", items(detail.Collaborators), parent); err != nil {
			return err
		}
		return im.insertAll(ctx, "autopilot_run", items(rec["runs"]), parent)
	})
}

func (im *importer) importChats(ctx context.Context, dir string) error {
	return eachLine(dir, "chat_sessions.jsonl", func(rec map[string]any) error {
		session := detailOrItem(rec)
		if err := im.insert(ctx, "chat_session", chatSessionRow(session, len(items(rec["messages"])))); err != nil {
			return err
		}
		return im.insertAll(ctx, "chat_message", items(rec["messages"]), map[string]any{"chat_session_id": session["id"]})
	})
}

// The API lists an empty session only when it was explicitly created, and
// renders pinned_at as a boolean.
func chatSessionRow(session map[string]any, messages int) map[string]any {
	row := merge(session, nil)
	if messages == 0 {
		row["explicitly_created_at"] = session["created_at"]
	}
	if session["pinned"] == true {
		row["pinned_at"] = firstNonEmpty(session["updated_at"], session["created_at"])
	}
	return row
}

func (im *importer) importAgentTasks(ctx context.Context, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "agent_tasks", "*.jsonl"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := eachLine(filepath.Dir(file), filepath.Base(file), func(rec map[string]any) error {
			task := asMap(rec["run"])
			if err := im.insertTask(ctx, task); err != nil {
				return err
			}
			return im.insertTaskMessages(ctx, task["id"], items(rec["messages"]))
		}); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) importDocs(ctx context.Context, dir string) error {
	var tree struct {
		Folders []map[string]any `json:"folders"`
	}
	if err := readJSON(filepath.Join(dir, "doc_tree.json"), &tree); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := im.insertAll(ctx, "doc_folder", tree.Folders, nil); err != nil {
		return err
	}
	return eachLine(dir, "docs.jsonl", func(rec map[string]any) error {
		doc := asMap(rec["doc"])
		if doc == nil {
			return nil
		}
		if err := im.insert(ctx, "doc", doc); err != nil {
			return err
		}
		parent := map[string]any{"doc_id": doc["id"], "workspace_id": im.workspaceID}
		if err := im.insertAll(ctx, "doc_version", items(rec["versions"]), parent); err != nil {
			return err
		}
		return im.insertAll(ctx, "doc_comment", items(rec["comments"]), parent)
	})
}

func (im *importer) importIssues(ctx context.Context, dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "issues", "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		var rec struct {
			Issue       map[string]any   `json:"issue"`
			Comments    []map[string]any `json:"comments"`
			Timeline    []map[string]any `json:"timeline"`
			Subscribers []map[string]any `json:"subscribers"`
			Wakeups     []map[string]any `json:"wakeups"`
			TaskRuns    []struct {
				Run      map[string]any   `json:"run"`
				Messages []map[string]any `json:"messages"`
			} `json:"task_runs"`
		}
		if err := readJSON(path, &rec); err != nil {
			return err
		}
		if err := im.importIssue(ctx, rec.Issue); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		issueID := rec.Issue["id"]
		scope := map[string]any{"workspace_id": im.workspaceID, "issue_id": issueID}
		for _, c := range rec.Comments {
			if err := im.insert(ctx, "comment", merge(c, scope)); err != nil {
				return err
			}
			if err := im.insertAll(ctx, "comment_reaction", items(c["reactions"]),
				map[string]any{"comment_id": c["id"], "workspace_id": im.workspaceID}); err != nil {
				return err
			}
		}
		for _, entry := range rec.Timeline {
			if entry["type"] == "activity" {
				if err := im.insert(ctx, "activity_log", merge(entry, scope)); err != nil {
					return err
				}
			}
			if err := im.insertSupplements(ctx, entry, scope); err != nil {
				return err
			}
		}
		if err := im.insertAll(ctx, "issue_subscriber", rec.Subscribers, map[string]any{"issue_id": issueID}); err != nil {
			return err
		}
		if err := im.insertAll(ctx, "issue_wakeup", rec.Wakeups, scope); err != nil {
			return err
		}
		for _, tr := range rec.TaskRuns {
			if err := im.insertTask(ctx, tr.Run); err != nil {
				return err
			}
			if err := im.insertTaskMessages(ctx, tr.Run["id"], tr.Messages); err != nil {
				return err
			}
		}
	}
	return nil
}

// Supplement receipts are rendered on their comment; the request id the
// client sent is not exposed, so a fresh one stands in for it.
func (im *importer) insertSupplements(ctx context.Context, c, scope map[string]any) error {
	for _, receipt := range items(c["supplements"]) {
		row := merge(receipt, scope)
		row["comment_id"], row["author_id"], row["client_request_id"] = c["id"], c["actor_id"], uuid.NewString()
		row["created_at"], row["updated_at"] = c["created_at"], firstNonEmpty(receipt["delivered_at"], c["created_at"])
		if err := im.insert(ctx, "task_supplement", row); err != nil {
			return err
		}
	}
	return nil
}

func firstNonEmpty(values ...any) any {
	for _, v := range values {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

func (im *importer) importIssue(ctx context.Context, issue map[string]any) error {
	if issue == nil {
		return errors.New("record has no issue")
	}
	row := merge(issue, nil)
	if v, ok := issue["requester_id"]; ok {
		row["requester_user_id"] = v
	}
	if ref := asMap(issue["duplicate_of"]); ref != nil {
		row["duplicate_of_issue_id"] = ref["id"]
	}
	if err := im.insert(ctx, "issue", row); err != nil {
		return err
	}
	return im.insertLinks(ctx, "issue_to_label", issue["labels"], map[string]any{"issue_id": issue["id"]}, "label_id")
}

func (im *importer) insertTask(ctx context.Context, task map[string]any) error {
	if err := im.insert(ctx, "agent_task_queue", taskRow(task)); err != nil {
		return err
	}
	if capability, _ := task["supplement_capability"].(string); capability != "" {
		if err := im.insert(ctx, "task_supplement_capability", map[string]any{"task_id": task["id"],
			"issue_id": task["issue_id"], "capability": capability, "created_at": task["created_at"]}); err != nil {
			return err
		}
	}
	return im.insertAll(ctx, "task_usage", items(task["usage"]), map[string]any{"task_id": task["id"]})
}

// taskRow maps the provenance the API renders as nested objects back onto
// the agent_task_queue columns it is read from.
func taskRow(task map[string]any) map[string]any {
	row := merge(task, nil)
	if attr := asMap(task["attribution"]); attr != nil {
		if source, _ := attr["source"].(string); source != "" && source != "unattributed" {
			row["originator_source"] = source
		}
		row["accountable_user_id"] = asMap(attr["initiator"])["id"]
		row["originator_user_id"] = asMap(attr["originator"])["id"]
		if evidence := asMap(attr["evidence"]); evidence != nil {
			row["trigger_evidence_kind"] = evidence["kind"]
			row["trigger_evidence_ref_id"] = evidence["ref_id"]
		}
		for _, key := range []string{"rule_version_id", "delegated_from_task_id", "retry_of_task_id", "rerun_of_task_id"} {
			row[key] = attr[key]
		}
	}
	if by := asMap(task["cancelled_by"]); by != nil {
		row["cancelled_by_type"], row["cancelled_by_id"], row["cancelled_by_name"] = by["type"], by["id"], by["name"]
	}
	if context := taskContext(task); len(context) > 0 {
		row["context"] = context
	}
	return row
}

// taskContext rebuilds the agent_task_queue.context keys the API derives
// fields from; the rest of the context is not exposed.
func taskContext(task map[string]any) map[string]any {
	context := map[string]any{}
	if wakeup, _ := task["wakeup_id"].(string); wakeup != "" {
		context["wakeup_id"] = wakeup
	}
	if rule, _ := task["wakeup_system_rule"].(string); rule != "" {
		context["wakeup_system"] = rule
	}
	if joined, _ := task["wakeup_joined"].(string); joined != "" {
		context["wakeup_joined"] = []any{map[string]any{"note": joined}}
	}
	if task["kind"] == "quick_create" {
		context["type"] = "quick_create"
	}
	if task["cancelled_by_comment_change"] == true {
		context["comment_change_cancelled_task_id"] = task["id"]
	}
	return context
}

// task_message ids are not exposed by the API, so a re-run must not append
// the transcript a second time.
func (im *importer) insertTaskMessages(ctx context.Context, taskID any, messages []map[string]any) error {
	if len(messages) == 0 {
		return nil
	}
	var exists bool
	if err := im.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_message WHERE task_id = $1)`, taskID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		im.stat("task_message").existing += len(messages)
		return nil
	}
	return im.insertAll(ctx, "task_message", messages, map[string]any{"task_id": taskID})
}

func (im *importer) importAttachments(ctx context.Context, dir string) error {
	metas, err := filepath.Glob(filepath.Join(dir, "attachments", "*", "meta.json"))
	if err != nil {
		return err
	}
	for _, path := range metas {
		var meta map[string]any
		if err := readJSON(path, &meta); err != nil {
			return err
		}
		if url, ok := im.storedURLs[str(meta, "id")]; ok {
			meta["url"] = url
		}
		if err := im.insert(ctx, "attachment", meta); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) insertLinks(ctx context.Context, table string, raw any, parent map[string]any, idColumn string) error {
	for _, item := range items(raw) {
		row := merge(parent, nil)
		row[idColumn] = item["id"]
		if err := im.insert(ctx, table, row); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) insertAll(ctx context.Context, table string, rows []map[string]any, parent map[string]any) error {
	for _, row := range rows {
		if err := im.insert(ctx, table, merge(parent, row)); err != nil {
			return err
		}
	}
	return nil
}

// insert maps JSON keys to same-named columns with jsonb_populate_record; only
// keys present in the row are listed so column defaults still apply.
func (im *importer) insert(ctx context.Context, table string, row map[string]any) error {
	if row == nil {
		return nil
	}
	columns, err := im.tableColumns(ctx, table)
	if err != nil {
		return err
	}
	stats := im.stat(table)
	kept := map[string]any{}
	for key, value := range row {
		col, ok := columns[key]
		switch {
		case !ok:
			stats.droppedKeys[key] = true
		case value == nil && col.notNullWithDefault:
		case value == "" && !textTypes[col.dataType]:
			kept[key] = nil
		default:
			kept[key] = value
		}
	}
	if len(kept) == 0 {
		return nil
	}
	if _, ok := columns["workspace_id"]; ok && kept["workspace_id"] == nil {
		kept["workspace_id"] = im.workspaceID
	}
	names := make([]string, 0, len(kept))
	for key := range kept {
		names = append(names, pgx.Identifier{key}.Sanitize())
	}
	sort.Strings(names)
	payload, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	list := strings.Join(names, ", ")
	ident := pgx.Identifier{table}.Sanitize()
	query := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM jsonb_populate_record(NULL::%s, $1::jsonb) ON CONFLICT DO NOTHING`,
		ident, list, list, ident)
	tag, err := im.tx.Exec(ctx, query, relativizeAttachmentLinks(im.replacer.Replace(string(payload))))
	if err != nil {
		return fmt.Errorf("insert %s %v: %w", table, row["id"], err)
	}
	if tag.RowsAffected() == 0 {
		stats.existing++
	} else {
		stats.inserted++
	}
	return nil
}

// The API renders NULL uuids and timestamps as "".
var textTypes = map[string]bool{"text": true, "character varying": true, "character": true, "citext": true}

type column struct {
	dataType           string
	notNullWithDefault bool
}

func (im *importer) tableColumns(ctx context.Context, table string) (map[string]column, error) {
	if cols, ok := im.columns[table]; ok {
		return cols, nil
	}
	rows, err := im.tx.Query(ctx, `SELECT column_name, data_type, is_nullable = 'NO' AND column_default IS NOT NULL FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND is_generated = 'NEVER' AND identity_generation IS DISTINCT FROM 'ALWAYS'`, table)
	if err != nil {
		return nil, err
	}
	set := map[string]column{}
	for rows.Next() {
		var name string
		var col column
		if err := rows.Scan(&name, &col.dataType, &col.notNullWithDefault); err != nil {
			return nil, err
		}
		set[name] = col
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("target has no table %q: archive and server versions differ", table)
	}
	im.columns[table] = set
	return set, nil
}

func (im *importer) stat(table string) *tableStats {
	s, ok := im.stats[table]
	if !ok {
		s = &tableStats{droppedKeys: map[string]bool{}}
		im.stats[table] = s
	}
	return s
}

func (im *importer) printReport(manifest *manifest, dangling []string) {
	if manifest != nil {
		fmt.Printf("archive: exported %s from %s by CLI %s\n", manifest.FinishedAt, manifest.ServerURL, manifest.CLIVersion)
	}
	tables := make([]string, 0, len(im.stats))
	for t := range im.stats {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	fmt.Printf("%-26s %9s %9s  %s\n", "table", "inserted", "existing", "keys without column")
	for _, t := range tables {
		s := im.stats[t]
		dropped := make([]string, 0, len(s.droppedKeys))
		for k := range s.droppedKeys {
			dropped = append(dropped, k)
		}
		sort.Strings(dropped)
		fmt.Printf("%-26s %9d %9d  %s\n", t, s.inserted, s.existing, strings.Join(dropped, ","))
	}
	if im.files != nil {
		fmt.Printf("files written: %d (%d attachments, %d avatars); missing from the archive: %d\n",
			im.files.stats.attachments+im.files.stats.avatars, im.files.stats.attachments, im.files.stats.avatars, im.files.stats.missing)
	}
	for _, m := range im.manual {
		fmt.Println("to reconfigure:", m)
	}
	for _, d := range im.repaired {
		fmt.Println("repaired:", d)
	}
	for _, d := range dangling {
		fmt.Println("dangling:", d)
	}
}

// checkDangling replays the foreign keys of every table this import wrote,
// since replica mode skipped them.
// With repair, references to missing rows are set to NULL where the column
// allows it and the referencing rows are deleted otherwise.
func (im *importer) checkDangling(ctx context.Context, repair bool) ([]string, error) {
	tx := im.tx
	rows, err := tx.Query(ctx, `
		SELECT c.conname, c.conrelid::regclass::text, c.confrelid::regclass::text,
		       array_agg(a.attname ORDER BY k.ord), array_agg(fa.attname ORDER BY k.ord),
		       bool_and(NOT a.attnotnull)
		FROM pg_constraint c
		CROSS JOIN LATERAL unnest(c.conkey, c.confkey) WITH ORDINALITY AS k(col, fcol, ord)
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.col
		JOIN pg_attribute fa ON fa.attrelid = c.confrelid AND fa.attnum = k.fcol
		WHERE c.contype = 'f' AND c.connamespace = 'public'::regnamespace
		GROUP BY c.conname, c.conrelid, c.confrelid`)
	if err != nil {
		return nil, err
	}
	type fk struct {
		name, child, parent string
		cols, parentCols    []string
		nullable            bool
	}
	var fks []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.name, &f.child, &f.parent, &f.cols, &f.parentCols, &f.nullable); err != nil {
			return nil, err
		}
		fks = append(fks, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var dangling []string
	for _, f := range fks {
		table := strings.Trim(f.child, `"`)
		if im.stats[table] == nil || im.stats[table].inserted == 0 {
			continue
		}
		var notNull, join []string
		for i := range f.cols {
			c, p := pgx.Identifier{f.cols[i]}.Sanitize(), pgx.Identifier{f.parentCols[i]}.Sanitize()
			notNull = append(notNull, "c."+c+" IS NOT NULL")
			join = append(join, "p."+p+" = c."+c)
		}
		args := []any{}
		if _, scoped := im.columns[table]["workspace_id"]; scoped {
			notNull = append(notNull, "c.workspace_id = $1")
			args = append(args, im.workspaceID)
		}
		orphans := fmt.Sprintf(`%s AND NOT EXISTS (SELECT 1 FROM %s p WHERE %s)`,
			strings.Join(notNull, " AND "), f.parent, strings.Join(join, " AND "))
		if repair {
			statement := fmt.Sprintf(`DELETE FROM %s c WHERE %s`, f.child, orphans)
			if f.nullable {
				sets := make([]string, len(f.cols))
				for i, col := range f.cols {
					sets[i] = pgx.Identifier{col}.Sanitize() + " = NULL"
				}
				statement = fmt.Sprintf(`UPDATE %s c SET %s WHERE %s`, f.child, strings.Join(sets, ", "), orphans)
			}
			if _, err := tx.Exec(ctx, statement, args...); err != nil {
				return nil, fmt.Errorf("repair %s: %w", f.name, err)
			}
			continue
		}
		var count int64
		query := fmt.Sprintf(`SELECT count(*) FROM %s c WHERE %s`, f.child, orphans)
		if err := tx.QueryRow(ctx, query, args...).Scan(&count); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		if count > 0 {
			dangling = append(dangling, fmt.Sprintf("%s: %d rows of %s(%s) missing in %s",
				f.name, count, f.child, strings.Join(f.cols, ","), f.parent))
		}
	}
	return dangling, nil
}

type manifest struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	CLIVersion    string `json:"cli_version"`
	ServerURL     string `json:"server_url"`
	FinishedAt    string `json:"finished_at"`
}

func readManifest(dir string, allowPartial bool) (*manifest, error) {
	var m manifest
	err := readJSON(filepath.Join(dir, "manifest.json"), &m)
	switch {
	case errors.Is(err, os.ErrNotExist) && allowPartial:
		slog.Warn("no manifest.json: importing a partial archive")
		return nil, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, errors.New("manifest.json missing: the export did not finish (re-run it, or pass --allow-partial)")
	case err != nil:
		return nil, err
	}
	if m.Format != archiveFormat || m.FormatVersion != archiveFormatVersion {
		return nil, fmt.Errorf("unsupported archive %s v%d (this importer reads %s v%d)", m.Format, m.FormatVersion, archiveFormat, archiveFormatVersion)
	}
	return &m, nil
}

func eachLine(dir, name string, fn func(map[string]any) error) error {
	f, err := os.Open(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 256*1024*1024)
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := fn(row); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return scanner.Err()
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func remarshal(in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// items accepts a bare list or an object wrapping one (e.g. {"labels": [...]}).
func items(raw any) []map[string]any {
	var list []map[string]any
	if remarshal(raw, &list) == nil {
		return list
	}
	var wrapped map[string]json.RawMessage
	if remarshal(raw, &wrapped) != nil {
		return nil
	}
	keys := make([]string, 0, len(wrapped))
	for k := range wrapped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if json.Unmarshal(wrapped[k], &list) == nil && list != nil {
			return list
		}
	}
	return nil
}

func detailOrItem(rec map[string]any) map[string]any {
	if detail := asMap(rec["detail"]); detail != nil {
		return detail
	}
	return asMap(rec["item"])
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func merge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
