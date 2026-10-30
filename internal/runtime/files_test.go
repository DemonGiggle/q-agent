package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/telegram"
)

type fileSink struct {
	files map[string][]byte
	fail  bool
}

func (s *fileSink) Typing(context.Context, platform.Message) error { return nil }
func (s *fileSink) DownloadFile(context.Context, string) ([]byte, error) {
	if s.fail {
		return nil, context.DeadlineExceeded
	}
	return s.files["data"], nil
}

type dirWorkspace struct{ dir string }

func (w *dirWorkspace) Ensure(context.Context, platform.Message) (string, error) {
	return w.dir, nil
}

func fileFixture(t *testing.T, data []byte) (*Runtime, *fileSink, string) {
	t.Helper()
	r, _, _ := fixture(t)
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	sink := &fileSink{files: map[string][]byte{"data": data}}
	r.Workspaces = &dirWorkspace{dir: rootfs}
	r.Sink = sink
	return r, sink, rootfs
}

func TestSaveFilesToInbox(t *testing.T) {
	r, _, rootfs := fileFixture(t, []byte("hello-world"))
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 7, Text: "use this", Files: []platform.File{{FileID: "f1", Name: "notes.txt"}}}
	if err := r.saveFiles(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 0 {
		t.Fatal("FileIDs not cleared")
	}
	if !strings.Contains(m.Text, "inbox/notes.txt") {
		t.Fatalf("path not folded: %q", m.Text)
	}
	data, err := os.ReadFile(filepath.Join(rootfs, "workspace", "inbox", "notes.txt"))
	if err != nil || string(data) != "hello-world" {
		t.Fatalf("saved=%q err=%v", string(data), err)
	}
}

func TestSaveFilesSanitizesTraversal(t *testing.T) {
	r, _, rootfs := fileFixture(t, []byte("x"))
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 8, Text: "evil", Files: []platform.File{{FileID: "f1", Name: "../../evil.sh"}}}
	if err := r.saveFiles(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "workspace", "evil.sh")); err == nil {
		t.Fatal("traversal escaped inbox")
	}
	if _, err := os.Stat(filepath.Join(rootfs, "workspace", "inbox", "evil.sh")); err != nil {
		t.Fatalf("sanitized file missing: %v", err)
	}
}

func TestSaveFilesImageDualUse(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("0", 100))
	r, sink, _ := fileFixture(t, png)
	sink.files["data"] = png
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 9, Text: "logo", Files: []platform.File{{FileID: "f1", Name: "logo.png", Mime: "image/png"}}}
	if err := r.saveFiles(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Images) != 1 {
		t.Fatalf("image document not fed to vision: %+v", m.Images)
	}
}

func TestSaveFilesTooMany(t *testing.T) {
	r, _, _ := fileFixture(t, []byte("x"))
	var files []platform.File
	for i := 0; i < telegram.MaxFilesPerMessage+1; i++ {
		files = append(files, platform.File{FileID: "f", Name: "a.txt"})
	}
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, Text: "many", Files: files}
	if err := r.saveFiles(context.Background(), &m); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("expected limit error, got %v", err)
	}
}

func TestClearInboxDirAge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldFile := filepath.Join(dir, "old.txt")
	newFile := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := now.Add(-25 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	removed, _ := clearInboxDir(dir, now, inboxMaxAge)
	if removed != 2 {
		t.Fatalf("removed=%d, want 2 (old file + old dir)", removed)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("old file not removed")
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("old dir not removed")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new file should survive: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("inbox dir itself must survive: %v", err)
	}
}

func TestClearInboxDirMissing(t *testing.T) {
	if removed, freed := clearInboxDir(filepath.Join(t.TempDir(), "nope"), time.Now(), inboxMaxAge); removed != 0 || freed != 0 {
		t.Fatalf("missing dir should be no-op, got %d/%d", removed, freed)
	}
}

type peekWorkspace struct {
	roots map[string]string
}

func (w *peekWorkspace) Ensure(_ context.Context, m platform.Message) (string, error) {
	return w.roots[m.WorkspaceKey()], nil
}

func (w *peekWorkspace) Peek(m platform.Message) (string, bool) {
	root, ok := w.roots[m.WorkspaceKey()]
	if !ok {
		return "", false
	}
	return root, true
}

func TestSweepInboxSkipsBusyAndMissing(t *testing.T) {
	r, _, _ := fixture(t)
	base := t.TempDir()
	var n int
	mkroot := func() string {
		n++
		rootfs := filepath.Join(base, fmt.Sprintf("ws%d", n), "rootfs")
		if err := os.MkdirAll(filepath.Join(rootfs, "workspace", "inbox"), 0700); err != nil {
			t.Fatal(err)
		}
		return rootfs
	}
	rootIdle := mkroot()
	rootBusy := mkroot()
	// Chat 1 idle with an expired file, chat 2 busy with an expired file,
	// chat 3 recorded but never provisioned (Peek miss).
	idle := platform.Message{Platform: "telegram", ChatID: 11, UserID: 11}
	busy := platform.Message{Platform: "telegram", ChatID: 22, UserID: 22}
	missing := platform.Message{Platform: "telegram", ChatID: 33, UserID: 33}
	for _, m := range []platform.Message{idle, busy, missing} {
		if err := r.Store.RecordChat(m, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	r.Workspaces = &peekWorkspace{roots: map[string]string{
		idle.WorkspaceKey(): rootIdle,
		busy.WorkspaceKey(): rootBusy,
		// missing intentionally absent to exercise the Peek-miss path.
	}}
	old := time.Now().Add(-25 * time.Hour)
	for _, root := range []string{rootIdle, rootBusy} {
		p := filepath.Join(inboxDirForRoot(root), "stale.txt")
		if err := os.WriteFile(p, []byte("stale"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	r.mu.Lock()
	if r.active == nil {
		r.active = map[string]task{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	r.active[busy.WorkspaceKey()] = task{cancel: cancel}
	r.mu.Unlock()
	if err := r.sweepInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inboxDirForRoot(rootIdle), "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("idle inbox should be cleared")
	}
	if _, err := os.Stat(filepath.Join(inboxDirForRoot(rootBusy), "stale.txt")); err != nil {
		t.Fatalf("busy inbox must survive to next run: %v", err)
	}
}
