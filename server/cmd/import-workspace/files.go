package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/util"
)

// Archived avatar URLs carry the source server's HMAC; the target signs its own at read time.
const servedAvatarPathPrefix = "/api/avatars/"

// Mirrors handler.avatarExtContentTypes: only these keys are served through /api/avatars.
var avatarContentTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".avif": "image/avif", ".bmp": "image/bmp", ".ico": "image/x-icon",
}

var absoluteAttachmentLink = regexp.MustCompile(`https?://[^\s"'<>()\[\]\\]+(/api/attachments/[0-9a-fA-F-]{36}/download)`)

// relativizeAttachmentLinks keeps attachment links on the site-relative route the
// server persists itself, so they load whatever origin serves the app.
func relativizeAttachmentLinks(text string) string {
	return absoluteAttachmentLink.ReplaceAllString(text, "$1")
}

type storedAttachment struct {
	id, oldURL, newURL string
}

type storedAvatar struct {
	id, url, filename, contentType string
	size                           int
}

type archivedAvatar struct {
	URL  string `json:"url"`
	File string `json:"file"`
}

type fileStore struct {
	store  storage.Storage
	dryRun bool
	stats  struct{ attachments, avatars, missing int }
}

func openFileStore(opts options) (*fileStore, error) {
	if opts.skipFiles {
		return nil, nil
	}
	fs := &fileStore{dryRun: opts.dryRun}
	if s3 := storage.NewS3StorageFromEnv(); s3 != nil {
		fs.store = s3
	} else if local := storage.NewLocalStorageFromEnv(); local != nil {
		fs.store = local
	} else {
		return nil, errors.New("no attachment storage configured (S3_BUCKET or LOCAL_UPLOAD_DIR); use --skip-files to import rows only")
	}
	return fs, nil
}

func (fs *fileStore) put(ctx context.Context, key string, data []byte, contentType, name string) (string, error) {
	if fs.dryRun {
		return fs.store.ObjectURL(key), nil
	}
	return fs.store.Upload(ctx, key, data, contentType, name)
}

// uploadAttachments copies archived files into the target storage. Rows keep
// their id, so content links are rewritten to the durable download route.
func uploadAttachments(ctx context.Context, archive string, fs *fileStore) ([]storedAttachment, error) {
	metas, err := filepath.Glob(filepath.Join(archive, "attachments", "*", "meta.json"))
	if err != nil {
		return nil, err
	}
	stored := make([]storedAttachment, 0, len(metas))
	for _, metaPath := range metas {
		var meta map[string]any
		if err := readJSON(metaPath, &meta); err != nil {
			return nil, err
		}
		file := storedAttachment{id: str(meta, "id"), oldURL: str(meta, "url")}
		key := storageKey(file.oldURL)
		if fs == nil || key == "" {
			stored = append(stored, file)
			continue
		}
		name := filepath.Base(str(meta, "filename"))
		data, err := os.ReadFile(filepath.Join(filepath.Dir(metaPath), name))
		if err != nil {
			slog.Warn("attachment file missing from archive", "id", file.id, "error", err)
			fs.stats.missing++
			stored = append(stored, file)
			continue
		}
		if file.newURL, err = fs.put(ctx, key, data, str(meta, "content_type"), name); err != nil {
			return nil, fmt.Errorf("upload %s: %w", key, err)
		}
		fs.stats.attachments++
		stored = append(stored, file)
	}
	return stored, nil
}

// uploadAvatars stores archived avatars the way UploadFile does: under the
// workspace namespace, named after an unbound image attachment row.
func uploadAvatars(ctx context.Context, archive, workspaceID string, fs *fileStore) (map[string]storedAvatar, error) {
	avatars := map[string]storedAvatar{}
	if fs == nil {
		return avatars, nil
	}
	dir := filepath.Join(archive, "avatars")
	err := eachLine(dir, "index.jsonl", func(row map[string]any) error {
		var entry archivedAvatar
		if err := remarshal(row, &entry); err != nil {
			return err
		}
		name := filepath.Base(entry.File)
		ext := strings.ToLower(path.Ext(name))
		contentType := avatarContentTypes[ext]
		if contentType == "" {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			slog.Warn("avatar file missing from archive", "url", entry.URL, "error", err)
			fs.stats.missing++
			return nil
		}
		id := avatarAttachmentID(entry.URL)
		key := "workspaces/" + workspaceID + "/" + id + ext
		newURL, err := fs.put(ctx, key, data, contentType, name)
		if err != nil {
			return fmt.Errorf("upload avatar %s: %w", key, err)
		}
		fs.stats.avatars++
		avatars[entry.URL] = storedAvatar{id: id, url: newURL, filename: path.Base(urlPath(entry.URL)), contentType: contentType, size: len(data)}
		return nil
	})
	return avatars, err
}

// avatarAttachmentID reuses the source object's UUID so a re-run lands on the
// same row and file; other names get a stable UUID derived from the URL.
func avatarAttachmentID(sourceURL string) string {
	base := path.Base(urlPath(sourceURL))
	if id, err := uuid.Parse(strings.TrimSuffix(base, path.Ext(base))); err == nil {
		return id.String()
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(sourceURL)).String()
}

func isServedAvatarURL(raw string) bool {
	return strings.HasPrefix(urlPath(raw), servedAvatarPathPrefix)
}

func urlPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

func storageKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	// Local storage serves its keys under /uploads/ and adds it back on upload.
	return strings.TrimPrefix(strings.TrimPrefix(u.Path, "/"), "uploads/")
}

// contentLinkPairs maps every archived attachment URL to its download route.
func contentLinkPairs(files []storedAttachment) []string {
	pairs := make([]string, 0, 2*len(files))
	for _, f := range files {
		if f.oldURL == "" || f.id == "" || strings.Contains(f.newURL, f.oldURL) {
			continue
		}
		pairs = append(pairs, f.oldURL, util.AttachmentDownloadPath(f.id))
	}
	return pairs
}
