package runtime

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

// workspaceBaseForFiles maps a Workspaces.Ensure root to the agent-visible
// file root: sandbox rootfs -> <root>/workspace, host workspace -> root.
func workspaceBaseForFiles(root string) string {
	if strings.HasSuffix(filepath.Clean(root), "rootfs") {
		return filepath.Join(root, "workspace")
	}
	return root
}

func inboxDirForRoot(root string) string {
	return filepath.Join(workspaceBaseForFiles(root), "inbox")
}

func isImageData(data []byte) (string, bool) {
	ct := http.DetectContentType(data)
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return ct, true
	default:
		return "", false
	}
}

func inboxUsage(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

func uniqueInboxPath(dir, name string) string {
	base := filepath.Base(name)
	if base == "" || base == "." || base == "/" {
		base = "file.bin"
	}
	candidate := filepath.Join(dir, base)
	if _, err := os.Lstat(candidate); os.IsNotExist(err) {
		return candidate
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 1000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return candidate
}

// saveFiles downloads generic Telegram attachments into the chat workspace
// inbox and folds workspace-relative paths into the prompt. It runs before
// busy routing so follow-ups steering an active task keep their files.
func (r *Runtime) saveFiles(ctx context.Context, m *platform.Message) error {
	if len(m.Files) == 0 {
		return nil
	}
	downloader, ok := r.Sink.(interface {
		DownloadFile(context.Context, string) ([]byte, error)
	})
	if !ok {
		return fmt.Errorf("file input is unavailable in this adapter")
	}
	if len(m.Files) > telegram.MaxFilesPerMessage {
		return fmt.Errorf("too many attached files (maximum %d per message)", telegram.MaxFilesPerMessage)
	}
	root, err := r.Workspaces.Ensure(ctx, *m)
	if err != nil {
		return fmt.Errorf("workspace unavailable: %v", err)
	}
	inbox := inboxDirForRoot(root)
	if err := os.MkdirAll(inbox, 0700); err != nil {
		return fmt.Errorf("workspace unavailable: %v", err)
	}
	if inboxUsage(inbox) > telegram.MaxInboxBytes {
		return fmt.Errorf("chat inbox is full (100 MB limit); remove files from inbox/ and resend")
	}
	var saved []string
	for _, f := range m.Files {
		display := f.Name
		if display == "" {
			display = "file"
		}
		data, err := downloader.DownloadFile(ctx, f.FileID)
		if err != nil {
			return fmt.Errorf("could not load the attached file %q: %v", display, err)
		}
		if int64(len(data))+inboxUsage(inbox) > telegram.MaxInboxBytes {
			return fmt.Errorf("chat inbox is full (100 MB limit); remove files from inbox/ and resend")
		}
		name := telegram.SanitizeFileName(f.Name)
		if name == "" {
			name = "file.bin"
		}
		dst := uniqueInboxPath(inbox, name)
		// Containment check against symlink races on the directory.
		if rel, err := filepath.Rel(inbox, dst); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid file name %q", display)
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return fmt.Errorf("could not save the attached file %q: %v", display, err)
		}
		_ = state.SyncDir(inbox)
		if len(m.Images) < 2 {
			if ct, ok := isImageData(data); ok {
				m.Images = append(m.Images, llm.Image{MediaType: ct, Data: data})
			}
		}
		rel := "inbox/" + filepath.Base(dst)
		saved = append(saved, fmt.Sprintf("%s (%d bytes)", rel, len(data)))
		r.logf("[TELEGRAM] File saved | update=%d name=%q bytes=%d", m.UpdateID, filepath.Base(dst), len(data))
	}
	m.Files = nil
	m.Text += "\n\n[Uploaded file(s) saved to " + strings.Join(saved, ", ") + ". Files persist in this chat's workspace; reference them as inbox/<name> or /workspace/inbox/<name>.]"
	return nil
}
