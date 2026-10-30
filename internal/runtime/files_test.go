package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
