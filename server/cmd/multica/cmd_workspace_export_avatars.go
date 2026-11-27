package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const servedAvatarPathPrefix = "/api/avatars/"

type exportedAvatar struct {
	URL  string `json:"url"`
	File string `json:"file"`
}

// exportAvatars downloads the avatars of the workspace, its members, agents
// and squads through the URLs just returned by the API, whose signature only
// the source server accepts. Third-party avatars (OAuth profiles) are kept as links.
func (x *workspaceExporter) exportAvatars(ctx context.Context) error {
	urls := map[string]bool{}
	collect := func(record map[string]any) {
		if raw := stringField(record, "avatar_url"); strings.HasPrefix(urlPathOf(raw), servedAvatarPathPrefix) {
			urls[raw] = true
		}
	}
	var ws struct {
		Workspace map[string]any `json:"workspace"`
	}
	if err := readJSONFile(filepath.Join(x.out, "workspace.json"), &ws); err == nil {
		collect(ws.Workspace)
	}
	for _, name := range []string{"members", "agents", "squads"} {
		for _, record := range readJSONLines(filepath.Join(x.out, name+".jsonl")) {
			collect(record)
			if item, ok := record["item"].(map[string]any); ok {
				collect(item)
			}
			if detail, ok := record["detail"].(map[string]any); ok {
				collect(detail)
			}
		}
	}

	dir := filepath.Join(x.out, "avatars")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var index []exportedAvatar
	for raw := range urls {
		file := avatarFileName(raw)
		reqCtx, cancel := context.WithTimeout(ctx, time.Minute)
		data, err := x.client.DownloadFile(reqCtx, raw)
		cancel()
		if err != nil {
			x.recordError(raw, err.Error())
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
			return err
		}
		index = append(index, exportedAvatar{URL: raw, File: file})
	}
	x.count("avatars", len(index))
	return writeJSONLines(filepath.Join(dir, "index.jsonl"), index)
}

// avatarFileName keeps the object's name (a UUID the importer reuses as the
// attachment id), prefixed with a hash so two keys never collide.
func avatarFileName(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:4]) + "-" + path.Base(urlPathOf(rawURL))
}

func urlPathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func readJSONLines(path string) []map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var records []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}
	return records
}
