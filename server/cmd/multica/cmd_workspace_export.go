package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	workspaceExportFormat        = "multica-workspace-export"
	workspaceExportFormatVersion = 1
	issueExportPageSize          = 100
)

var workspaceExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export the current workspace into a portable archive directory",
	Long: "Reads the current workspace through the API and writes a versioned archive " +
		"directory: manifest.json, one JSON/JSONL file per entity, one file per issue " +
		"(comments, timeline, runs and transcripts) and the attachment files.\n\n" +
		"Read-only: nothing is modified on the server. Re-running with the same --out " +
		"resumes, skipping issues already exported. Agent environment secrets are only " +
		"exported with --include-secrets, and every secret read is audited server-side.",
	Example: "  multica workspace export --out ./workspace-export\n" +
		"  multica workspace export --out ./workspace-export --include-secrets --concurrency 8",
	Args: cobra.NoArgs,
	RunE: runWorkspaceExport,
}

func init() {
	workspaceExportCmd.Flags().String("out", "", "Archive directory to write (created if missing)")
	workspaceExportCmd.Flags().Int("concurrency", 4, "Issues exported in parallel")
	workspaceExportCmd.Flags().Bool("skip-transcripts", false, "Do not export agent run messages")
	workspaceExportCmd.Flags().Bool("skip-attachments", false, "Do not download attachment files")
	workspaceExportCmd.Flags().Bool("include-secrets", false, "Export agent custom environment values (audited)")
	_ = workspaceExportCmd.MarkFlagRequired("out")
	workspaceCmd.AddCommand(workspaceExportCmd)
}

type exportManifest struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"format_version"`
	CLIVersion    string         `json:"cli_version"`
	ServerURL     string         `json:"server_url"`
	ServerConfig  map[string]any `json:"server_config,omitempty"`
	WorkspaceID   string         `json:"workspace_id"`
	StartedAt     time.Time      `json:"started_at"`
	FinishedAt    time.Time      `json:"finished_at"`
	Options       map[string]any `json:"options"`
	Counts        map[string]int `json:"counts"`
	Truncations   []exportIssue  `json:"truncations"`
	Errors        []exportIssue  `json:"errors"`
}

type exportIssue struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

type workspaceExporter struct {
	client          *cli.APIClient
	out             string
	skipTranscripts bool
	skipAttachments bool
	includeSecrets  bool
	concurrency     int

	mu          sync.Mutex
	counts      map[string]int
	truncations []exportIssue
	errors      []exportIssue
}

func runWorkspaceExport(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	if client.WorkspaceID == "" {
		return fmt.Errorf("no workspace selected: use --workspace-id or 'multica workspace switch'")
	}
	out, _ := cmd.Flags().GetString("out")
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	if concurrency < 1 || concurrency > 32 {
		return fmt.Errorf("--concurrency must be between 1 and 32")
	}
	x := &workspaceExporter{client: client, out: out, concurrency: concurrency, counts: map[string]int{}}
	x.skipTranscripts, _ = cmd.Flags().GetBool("skip-transcripts")
	x.skipAttachments, _ = cmd.Flags().GetBool("skip-attachments")
	x.includeSecrets, _ = cmd.Flags().GetBool("include-secrets")

	if err := os.MkdirAll(filepath.Join(out, "issues"), 0o700); err != nil {
		return err
	}
	manifest := exportManifest{
		Format:        workspaceExportFormat,
		FormatVersion: workspaceExportFormatVersion,
		CLIVersion:    version,
		ServerURL:     client.BaseURL,
		WorkspaceID:   client.WorkspaceID,
		StartedAt:     time.Now().UTC(),
		Options: map[string]any{
			"skip_transcripts": x.skipTranscripts,
			"skip_attachments": x.skipAttachments,
			"include_secrets":  x.includeSecrets,
		},
	}
	ctx := cmd.Context()
	var config map[string]any
	if _, err := x.get(ctx, "/api/config", &config); err == nil {
		manifest.ServerConfig = config
	}

	steps := []func(context.Context) error{
		x.exportWorkspace,
		x.exportCatalog,
		x.exportAgents,
		x.exportSkills,
		x.exportSquads,
		x.exportAvatars,
		x.exportProjects,
		x.exportAutopilots,
		x.exportDocs,
		x.exportChats,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	if err := x.exportIssues(ctx); err != nil {
		return err
	}
	if err := x.exportAgentTasks(ctx); err != nil {
		return err
	}

	manifest.FinishedAt = time.Now().UTC()
	manifest.Counts = x.counts
	manifest.Truncations = sortedExportIssues(x.truncations)
	manifest.Errors = sortedExportIssues(x.errors)
	if err := writeJSONFile(filepath.Join(out, "manifest.json"), manifest); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Export written to %s — %d issues, %d errors, %d truncations\n",
		out, x.counts["issues"], len(manifest.Errors), len(manifest.Truncations))
	return nil
}

func (x *workspaceExporter) get(ctx context.Context, path string, out any) (http.Header, error) {
	reqCtx, cancel := cli.APIContext(ctx)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		headers, err := x.client.GetJSONWithHeaders(reqCtx, path, out)
		if err == nil {
			return headers, nil
		}
		lastErr = err
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode < 500 && httpErr.StatusCode != http.StatusTooManyRequests {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}

// getOptional records a failed read in the manifest instead of aborting: a
// disabled feature (404) or a permission gap (403) must not sink the export.
func (x *workspaceExporter) getOptional(ctx context.Context, path string) (json.RawMessage, http.Header) {
	var raw json.RawMessage
	headers, err := x.get(ctx, path, &raw)
	if err != nil {
		x.recordError(path, err.Error())
		return nil, nil
	}
	return raw, headers
}

func (x *workspaceExporter) recordError(path, detail string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.errors = append(x.errors, exportIssue{Path: path, Detail: detail})
}

func (x *workspaceExporter) recordTruncation(path, detail string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.truncations = append(x.truncations, exportIssue{Path: path, Detail: detail})
}

func (x *workspaceExporter) count(entity string, n int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.counts[entity] += n
}

func (x *workspaceExporter) exportWorkspace(ctx context.Context) error {
	ws := x.client.WorkspaceID
	record := map[string]json.RawMessage{}
	for key, path := range map[string]string{
		"workspace":   "/api/workspaces/" + ws,
		"features":    "/api/workspaces/" + ws + "/features",
		"mcp_servers": "/api/workspaces/" + ws + "/mcp-servers",
	} {
		if raw, _ := x.getOptional(ctx, path); raw != nil {
			record[key] = raw
		}
	}
	if record["workspace"] == nil {
		return fmt.Errorf("cannot read workspace %s: see the error above", ws)
	}
	if err := writeJSONFile(filepath.Join(x.out, "workspace.json"), record); err != nil {
		return err
	}
	return x.exportList(ctx, "members", "/api/workspaces/"+ws+"/members")
}

func (x *workspaceExporter) exportCatalog(ctx context.Context) error {
	for _, entity := range []struct{ name, path string }{
		{"labels", "/api/labels"},
		{"issue_statuses", "/api/issue-statuses"},
		{"issue_workflows", "/api/issue-workflows"},
		{"properties", "/api/properties"},
		{"quick_actions", "/api/quick-actions"},
		{"issue_views", "/api/issue-views?scope_type=all"},
		{"pins", "/api/pins"},
		{"runtimes", "/api/runtimes"},
	} {
		if err := x.exportList(ctx, entity.name, entity.path); err != nil {
			return err
		}
	}
	return nil
}

func (x *workspaceExporter) exportList(ctx context.Context, name, path string) error {
	raw, _ := x.getOptional(ctx, path)
	if raw == nil {
		return nil
	}
	items := listItems(raw)
	x.count(name, len(items))
	return writeJSONLines(filepath.Join(x.out, name+".jsonl"), items)
}

// exportNested writes one JSONL line per list item, each enriched with the
// sub-resources read from the item's id.
func (x *workspaceExporter) exportNested(ctx context.Context, name, listPath string, subPaths map[string]string) error {
	raw, _ := x.getOptional(ctx, listPath)
	if raw == nil {
		return nil
	}
	items := listItems(raw)
	records := make([]any, 0, len(items))
	for _, item := range items {
		id := stringField(item, "id")
		record := map[string]any{"item": item}
		for key, pattern := range subPaths {
			path := strings.ReplaceAll(pattern, "{id}", url.PathEscape(id))
			if field, paged := offsetPagedLists[pattern]; paged {
				if all := x.getAllOffsetPages(ctx, path, field); all != nil {
					record[key] = all
				}
			} else if sub, _ := x.getOptional(ctx, path); sub != nil {
				record[key] = sub
			}
		}
		if versions, ok := record["versions"].(json.RawMessage); ok {
			record["versions"] = x.versionDetails(ctx, strings.ReplaceAll(subPaths["versions"], "{id}", url.PathEscape(id)), versions, "version")
		}
		records = append(records, record)
	}
	x.count(name, len(records))
	return writeJSONLines(filepath.Join(x.out, name+".jsonl"), records)
}

// offsetPagedLists maps list routes that return one limit/offset page per
// call to the response field holding the items.
var offsetPagedLists = map[string]string{
	"/api/autopilots/{id}/runs": "runs",
}

const offsetPageSize = 100

func (x *workspaceExporter) getAllOffsetPages(ctx context.Context, path, field string) json.RawMessage {
	var all []json.RawMessage
	for offset := 0; ; offset += offsetPageSize {
		var page map[string]json.RawMessage
		pagePath := fmt.Sprintf("%s?limit=%d&offset=%d", path, offsetPageSize, offset)
		if _, err := x.get(ctx, pagePath, &page); err != nil {
			x.recordError(pagePath, err.Error())
			return nil
		}
		var items []json.RawMessage
		_ = json.Unmarshal(page[field], &items)
		all = append(all, items...)
		if len(items) < offsetPageSize {
			break
		}
	}
	data, _ := json.Marshal(map[string]any{field: all, "total": len(all)})
	return data
}

func (x *workspaceExporter) exportAgents(ctx context.Context) error {
	sub := map[string]string{
		"detail":      "/api/agents/{id}",
		"skills":      "/api/agents/{id}/skills",
		"mcp_servers": "/api/agents/{id}/mcp-servers",
		"labels":      "/api/agents/{id}/labels",
	}
	if x.includeSecrets {
		sub["env"] = "/api/agents/{id}/env"
	}
	return x.exportNested(ctx, "agents", "/api/agents?include_archived=true", sub)
}

func (x *workspaceExporter) exportSkills(ctx context.Context) error {
	return x.exportNested(ctx, "skills", "/api/skills", map[string]string{
		"detail":   "/api/skills/{id}",
		"files":    "/api/skills/{id}/files",
		"versions": "/api/skills/{id}/versions",
		"labels":   "/api/skills/{id}/labels",
	})
}

func (x *workspaceExporter) exportSquads(ctx context.Context) error {
	return x.exportNested(ctx, "squads", "/api/squads", map[string]string{
		"members": "/api/squads/{id}/members",
	})
}

func (x *workspaceExporter) exportProjects(ctx context.Context) error {
	return x.exportNested(ctx, "projects", "/api/projects?include_archived=true", map[string]string{
		"members":   "/api/projects/{id}/members",
		"resources": "/api/projects/{id}/resources",
	})
}

func (x *workspaceExporter) exportAutopilots(ctx context.Context) error {
	return x.exportNested(ctx, "autopilots", "/api/autopilots", map[string]string{
		"detail": "/api/autopilots/{id}",
		"runs":   "/api/autopilots/{id}/runs",
	})
}

func (x *workspaceExporter) exportChats(ctx context.Context) error {
	if err := x.exportNested(ctx, "chat_sessions", "/api/chat/sessions?status=all", map[string]string{
		"messages": "/api/chat/sessions/{id}/messages",
	}); err != nil {
		return err
	}
	if x.skipAttachments {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(x.out, "chat_sessions.jsonl"))
	if err != nil {
		return nil
	}
	ids := make([]string, 0)
	for id := range referencedAttachmentIDs(data) {
		ids = append(ids, id)
	}
	x.forEachParallel(len(ids), func(i int) { x.downloadAttachment(ctx, ids[i]) })
	return nil
}

func (x *workspaceExporter) exportDocs(ctx context.Context) error {
	tree, _ := x.getOptional(ctx, "/api/docs")
	if tree == nil {
		return nil
	}
	if err := writeJSONFile(filepath.Join(x.out, "doc_tree.json"), tree); err != nil {
		return err
	}
	var records []any
	for _, id := range docIDs(tree) {
		escaped := url.PathEscape(id)
		record := map[string]any{"id": id}
		for key, path := range map[string]string{
			"doc":      "/api/docs/" + escaped + "?flush=true",
			"versions": "/api/docs/" + escaped + "/versions",
			"comments": "/api/docs/" + escaped + "/comments",
		} {
			if raw, _ := x.getOptional(ctx, path); raw != nil {
				record[key] = raw
			}
		}
		if versions, ok := record["versions"].(json.RawMessage); ok {
			record["versions"] = x.versionDetails(ctx, "/api/docs/"+escaped+"/versions", versions, "number")
		}
		records = append(records, record)
	}
	x.count("docs", len(records))
	return writeJSONLines(filepath.Join(x.out, "docs.jsonl"), records)
}

func (x *workspaceExporter) exportIssues(ctx context.Context) error {
	var ids []string
	for offset := 0; ; offset += issueExportPageSize {
		var page json.RawMessage
		path := fmt.Sprintf("/api/issues?limit=%d&offset=%d", issueExportPageSize, offset)
		if _, err := x.get(ctx, path, &page); err != nil {
			return fmt.Errorf("list issues at offset %d: %w", offset, err)
		}
		items := listItems(page)
		for _, item := range items {
			ids = append(ids, stringField(item, "id"))
		}
		if len(items) < issueExportPageSize {
			break
		}
	}
	x.count("issues", len(ids))

	jobs := make(chan string)
	var wg sync.WaitGroup
	var done int
	var doneMu sync.Mutex
	for w := 0; w < x.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if err := x.exportIssue(ctx, id); err != nil {
					x.recordError("/api/issues/"+id, err.Error())
				}
				doneMu.Lock()
				done++
				if done%50 == 0 || done == len(ids) {
					fmt.Fprintf(os.Stderr, "issues: %d/%d\n", done, len(ids))
				}
				doneMu.Unlock()
			}
		}()
	}
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	return nil
}

func (x *workspaceExporter) exportIssue(ctx context.Context, id string) error {
	target := filepath.Join(x.out, "issues", id+".json")
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	base := "/api/issues/" + url.PathEscape(id)
	record := map[string]any{}

	var issue json.RawMessage
	if _, err := x.get(ctx, base, &issue); err != nil {
		return err
	}
	record["issue"] = issue

	if raw, headers := x.getOptional(ctx, base+"/comments"); raw != nil {
		record["comments"] = raw
		if v := headers.Get("X-Comments-Truncated"); v != "" {
			x.recordTruncation(base+"/comments", v)
		}
		x.count("comments", len(listItems(raw)))
	}
	if raw, headers := x.getOptional(ctx, base+"/timeline"); raw != nil {
		record["timeline"] = raw
		if v := headers.Get("X-Timeline-Truncated"); v != "" {
			x.recordTruncation(base+"/timeline", v)
		}
		x.count("timeline_entries", len(listItems(raw)))
	}
	for key, path := range map[string]string{
		"subscribers": base + "/subscribers",
		"labels":      base + "/labels",
		"wakeups":     base + "/wakeups",
		"attachments": base + "/attachments",
	} {
		if raw, _ := x.getOptional(ctx, path); raw != nil {
			record[key] = raw
		}
	}

	if runsRaw, _ := x.getOptional(ctx, base+"/task-runs"); runsRaw != nil {
		runs := listItems(runsRaw)
		x.count("task_runs", len(runs))
		enriched := make([]any, 0, len(runs))
		for _, run := range runs {
			enriched = append(enriched, x.taskEntry(ctx, run))
		}
		record["task_runs"] = enriched
	}

	if !x.skipAttachments {
		x.downloadAttachments(ctx, record)
	}
	return writeJSONFile(target, record)
}

func (x *workspaceExporter) taskEntry(ctx context.Context, run map[string]any) map[string]any {
	entry := map[string]any{"run": run}
	if !x.skipTranscripts {
		msgPath := "/api/tasks/" + url.PathEscape(stringField(run, "id")) + "/messages"
		if msgs, _ := x.getOptional(ctx, msgPath); msgs != nil {
			entry["messages"] = msgs
			x.count("task_messages", len(listItems(msgs)))
		}
	}
	return entry
}

// exportAgentTasks covers runs no issue owns (chat, autopilot without issue);
// issue runs are already stored with their issue.
func (x *workspaceExporter) exportAgentTasks(ctx context.Context) error {
	data, err := os.ReadFile(filepath.Join(x.out, "agents.jsonl"))
	if err != nil {
		return nil
	}
	dir := filepath.Join(x.out, "agent_tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record struct {
			Item map[string]any `json:"item"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		agentID := stringField(record.Item, "id")
		target := filepath.Join(dir, agentID+".jsonl")
		if _, err := os.Stat(target); agentID == "" || err == nil {
			continue
		}
		var runs []map[string]any
		before := ""
		failed := false
		for {
			path := "/api/agents/" + url.PathEscape(agentID) + "/tasks?include_usage=true&limit=200"
			if before != "" {
				path += "&before=" + url.QueryEscape(before)
			}
			var page []map[string]any
			headers, err := x.get(ctx, path, &page)
			if err != nil {
				x.recordError(path, err.Error())
				failed = true
				break
			}
			for _, task := range page {
				if stringField(task, "issue_id") == "" {
					runs = append(runs, task)
				}
			}
			before = headers.Get("X-Agent-Tasks-Next-Cursor")
			if before == "" || len(page) == 0 {
				break
			}
		}
		if failed {
			continue
		}
		entries := make([]any, len(runs))
		x.forEachParallel(len(runs), func(i int) { entries[i] = x.taskEntry(ctx, runs[i]) })
		x.count("agent_tasks", len(entries))
		if err := writeJSONLines(target, entries); err != nil {
			return err
		}
	}
	return nil
}

// Version lists carry summaries only; the content of each snapshot lives
// behind its own route.
func (x *workspaceExporter) versionDetails(ctx context.Context, listPath string, summaries json.RawMessage, numberField string) []any {
	items := listItems(summaries)
	details := make([]any, 0, len(items))
	for _, summary := range items {
		number := fmt.Sprint(summary[numberField])
		detail, _ := x.getOptional(ctx, listPath+"/"+url.PathEscape(number))
		if detail == nil {
			details = append(details, summary)
			continue
		}
		details = append(details, detail)
	}
	x.count("versions", len(details))
	return details
}

var attachmentIDPattern = regexp.MustCompile(`/api/attachments/([0-9a-fA-F-]{36})`)

// downloadAttachments fetches every attachment listed on the issue and every
// attachment referenced from the issue body, its comments or run messages.
func (x *workspaceExporter) downloadAttachments(ctx context.Context, record map[string]any) {
	ids := map[string]bool{}
	if raw, ok := record["attachments"].(json.RawMessage); ok {
		for _, item := range listItems(raw) {
			ids[stringField(item, "id")] = true
		}
	}
	encoded, _ := json.Marshal(record)
	for id := range referencedAttachmentIDs(encoded) {
		ids[id] = true
	}
	x.downloadAttachmentIDs(ctx, ids)
}

func referencedAttachmentIDs(data []byte) map[string]bool {
	ids := map[string]bool{}
	for _, match := range attachmentIDPattern.FindAllSubmatch(data, -1) {
		ids[strings.ToLower(string(match[1]))] = true
	}
	return ids
}

func (x *workspaceExporter) downloadAttachmentIDs(ctx context.Context, ids map[string]bool) {
	for id := range ids {
		x.downloadAttachment(ctx, id)
	}
}

func (x *workspaceExporter) forEachParallel(n int, fn func(i int)) {
	queue := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < x.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		queue <- i
	}
	close(queue)
	wg.Wait()
}

// meta.json is written last so a resumed export retries a half-written file.
func (x *workspaceExporter) downloadAttachment(ctx context.Context, id string) {
	if id == "" {
		return
	}
	dir := filepath.Join(x.out, "attachments", id)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		return
	}
	path := "/api/attachments/" + url.PathEscape(id)
	var meta map[string]any
	if _, err := x.get(ctx, path, &meta); err != nil {
		x.recordError(path, err.Error())
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	data, err := x.client.DownloadFile(reqCtx, path+"/download")
	cancel()
	if err != nil {
		x.recordError(path+"/download", err.Error())
		return
	}
	name := filepath.Base(stringField(meta, "filename"))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "file"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		x.recordError(path, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		x.recordError(path, err.Error())
		return
	}
	if err := writeJSONFile(filepath.Join(dir, "meta.json"), meta); err != nil {
		x.recordError(path, err.Error())
		return
	}
	x.count("attachments", 1)
}

func listItems(raw json.RawMessage) []map[string]any {
	var list []map[string]any
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(raw, &wrapper) != nil {
		return nil
	}
	keys := make([]string, 0, len(wrapper))
	for key := range wrapper {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if json.Unmarshal(wrapper[key], &list) == nil && list != nil {
			return list
		}
	}
	return nil
}

func docIDs(tree json.RawMessage) []string {
	var parsed struct {
		Docs []map[string]any `json:"docs"`
	}
	if json.Unmarshal(tree, &parsed) != nil {
		return nil
	}
	ids := make([]string, 0, len(parsed.Docs))
	for _, doc := range parsed.Docs {
		if id := stringField(doc, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func stringField(item map[string]any, key string) string {
	v, _ := item[key].(string)
	return v
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSONLines[T any](path string, items []T) error {
	var b strings.Builder
	for _, item := range items {
		line, err := json.Marshal(item)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortedExportIssues(list []exportIssue) []exportIssue {
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	if list == nil {
		return []exportIssue{}
	}
	return list
}
