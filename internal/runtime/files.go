package runtime

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

// inboxMaxAge bounds how long an uploaded file stays in inbox/. The daily
// sweep removes entries older than this so files live at least 24h (up to ~48h
// depending on upload time) while disk stays bounded alongside the 100 MB cap.
const inboxMaxAge = 24 * time.Hour

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
	m.Text += "\n\n[Uploaded file(s) saved to " + strings.Join(saved, ", ") + ". Files persist in this chat's workspace for ~24h before the daily cleanup; reference them as inbox/<name> or /workspace/inbox/<name>.]"
	return nil
}

// savePhotos downloads Telegram photo sizes into the chat workspace inbox
// and feeds the same bytes to vision. It runs before busy routing so the
// agent can reference the persisted file with tools (for example copying a
// logo into docs/assets/) instead of losing the image after the task.
// Photos keep their existing 2-per-message vision limit; inbox quota and
// daily retention match generic files.
func (r *Runtime) savePhotos(ctx context.Context, m *platform.Message) error {
	if len(m.PhotoIDs) == 0 {
		return nil
	}
	loader, ok := r.Sink.(interface {
		DownloadPhoto(context.Context, string) (llm.Image, error)
	})
	if !ok {
		return fmt.Errorf("image input is unavailable in this adapter")
	}
	if len(m.PhotoIDs) > 2 {
		return fmt.Errorf("too many attached images (maximum 2 per message)")
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
	anchor := m.MessageID
	if anchor == 0 {
		anchor = m.UpdateID
	}
	var saved []string
	for i, id := range m.PhotoIDs {
		image, err := loader.DownloadPhoto(ctx, id)
		if err != nil {
			return fmt.Errorf("could not load the attached image: %v", err)
		}
		if int64(len(image.Data))+inboxUsage(inbox) > telegram.MaxInboxBytes {
			return fmt.Errorf("chat inbox is full (100 MB limit); remove files from inbox/ and resend")
		}
		m.Images = append(m.Images, image)
		name := fmt.Sprintf("photo-%d-%d.%s", anchor, i+1, photoExtension(image.MediaType))
		name = telegram.SanitizeFileName(name)
		if name == "" {
			name = "file.bin"
		}
		dst := uniqueInboxPath(inbox, name)
		if rel, err := filepath.Rel(inbox, dst); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid image name %q", name)
		}
		if err := os.WriteFile(dst, image.Data, 0600); err != nil {
			return fmt.Errorf("could not save the attached image %q: %v", name, err)
		}
		_ = state.SyncDir(inbox)
		rel := "inbox/" + filepath.Base(dst)
		saved = append(saved, fmt.Sprintf("%s (%d bytes)", rel, len(image.Data)))
		r.logf("[TELEGRAM] Photo saved | update=%d name=%q bytes=%d", m.UpdateID, filepath.Base(dst), len(image.Data))
	}
	m.PhotoIDs = nil
	m.Text += "\n\n[Uploaded image(s) saved to " + strings.Join(saved, ", ") + ". Files persist in this chat's workspace for ~24h before the daily cleanup; reference them as inbox/<name> or /workspace/inbox/<name>.]"
	return nil
}

func photoExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "jpg"
	}
}

// clearInboxDir removes inbox entries older than maxAge. It never follows
// symlinks and never removes the inbox directory itself. Missing directories
// are a no-op so chats without uploads cost nothing.
func clearInboxDir(dir string, now time.Time, maxAge time.Duration) (removed int, freed int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if info.IsDir() {
			if err := os.RemoveAll(p); err == nil {
				removed++
			}
			continue
		}
		size := info.Size()
		if err := os.Remove(p); err == nil {
			removed++
			freed += size
		}
	}
	if removed > 0 {
		_ = state.SyncDir(dir)
	}
	return removed, freed
}

// sweepInbox clears expired inbox files for every known chat. Busy chats and
// maintenance windows are skipped to the next daily run so a running task
// never loses a file it may still read.
func (r *Runtime) sweepInbox(ctx context.Context) error {
	chats, err := r.Store.ListChats()
	if err != nil {
		return err
	}
	for _, chat := range chats {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		_, busy := r.active[chat.Key]
		maintenance := r.Maintenance != nil && r.Maintenance.Busy()
		r.mu.Unlock()
		if busy || maintenance {
			continue
		}
		route := platform.Message{Platform: chat.Record.Platform, ChatID: chat.Record.ChatID, UserID: chat.Record.Owner}
		var root string
		if peeker, ok := r.Workspaces.(interface {
			Peek(platform.Message) (string, bool)
		}); ok {
			p, ok := peeker.Peek(route)
			if !ok {
				continue
			}
			root = p
		} else {
			var err error
			root, err = r.Workspaces.Ensure(ctx, route)
			if err != nil {
				r.logf("[INBOX] Sweep skipped, workspace unavailable | workspace=%s error=%q", chat.Key, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
				continue
			}
		}
		inbox := inboxDirForRoot(root)
		if removed, freed := clearInboxDir(inbox, time.Now(), inboxMaxAge); removed > 0 {
			r.logf("[INBOX] Cleared expired files | workspace=%s removed=%d freed=%d", chat.Key, removed, freed)
		}
	}
	return nil
}

// inboxLoop runs the daily inbox sweep alongside the memory digest schedule.
// It always runs, even when memory is disabled, so uploads never accumulate.
// The sweep time reuses Memory.Timezone/DigestAt (default local midnight) to
// keep one daily maintenance window; failures are reported, never fatal.
func (r *Runtime) inboxLoop(ctx context.Context) {
	loc := memoryLocation(r.Config.Memory.Timezone)
	hour, minute, err := parseDigestAtLocal(r.Config.Memory.DigestAt)
	if err != nil {
		r.report(err)
		hour, minute = 0, 0
	}
	next := time.Now().Add(untilNextDigest(time.Now(), loc, hour, minute))
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(next) {
			if err := r.sweepInbox(ctx); err != nil {
				r.report(err)
			}
			now = time.Now()
			next = now.Add(untilNextDigest(now, loc, hour, minute))
		}
		delay := time.Until(next)
		if delay > time.Minute {
			delay = time.Minute
		}
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
