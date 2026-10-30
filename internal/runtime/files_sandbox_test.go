package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
)

// sandboxWorkspace simulates workspace.Manager after the fix: Ensure returns
// a rootfs path the host cannot write directly (subordinate-UID-owned), and
// inbox writes/sweeps must go through WriteInboxFile/ClearInbox (sandbox
// namespace) instead of host MkdirAll/WriteFile.
type sandboxWorkspace struct {
	root        string
	writes      int
	clears      int
	lastName    string
	lastData    []byte
	maxBytes    int64
	clearCutoff time.Time
}

func (w *sandboxWorkspace) Ensure(context.Context, platform.Message) (string, error) {
	return w.root, nil
}

func (w *sandboxWorkspace) Peek(m platform.Message) (string, bool) {
	return w.root, true
}

func (w *sandboxWorkspace) WriteInboxFile(_ context.Context, root, name string, data []byte, maxBytes int64) (string, error) {
	w.writes++
	w.lastName = name
	w.lastData = append([]byte(nil), data...)
	w.maxBytes = maxBytes
	// Simulate sandbox O_EXCL dedup + 0644/0755 semantics on a host temp dir.
	inbox := filepath.Join(w.root, "workspace", "inbox")
	if err := os.MkdirAll(inbox, 0755); err != nil {
		return "", err
	}
	dst := filepath.Join(inbox, name)
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return "", err
	}
	return name, nil
}

func (w *sandboxWorkspace) ClearInbox(_ context.Context, root string, cutoff time.Time) (int, int64, error) {
	w.clears++
	w.clearCutoff = cutoff
	inbox := filepath.Join(root, "workspace", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		return 0, 0, nil
	}
	var removed int
	var freed int64
	for _, e := range entries {
		p := filepath.Join(inbox, e.Name())
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		size := info.Size()
		if err := os.Remove(p); err == nil {
			removed++
			freed += size
		}
	}
	return removed, freed, nil
}

func TestSaveFilesUsesSandboxWriter(t *testing.T) {
	r, _, _ := fixture(t)
	// Root deliberately has no inbox dir; host MkdirAll would create 0700
	// host-owned, but sandbox path must not touch the host directly.
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "workspace"), 0755); err != nil {
		t.Fatal(err)
	}
	ws := &sandboxWorkspace{root: rootfs}
	r.Workspaces = ws
	r.Sink = &fileSink{files: map[string][]byte{"data": []byte("sandbox-hello")}}
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 77, Text: "use this", Files: []platform.File{{FileID: "f1", Name: "notes.txt"}}}
	if err := r.saveFiles(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if ws.writes != 1 || ws.lastName != "notes.txt" || string(ws.lastData) != "sandbox-hello" {
		t.Fatalf("sandbox writer not used: %+v", ws)
	}
	if !strings.Contains(m.Text, "inbox/notes.txt") {
		t.Fatalf("path not folded: %q", m.Text)
	}
	// File must be visible to the agent via inbox/.
	if _, err := os.Stat(filepath.Join(rootfs, "workspace", "inbox", "notes.txt")); err != nil {
		t.Fatalf("sandbox inbox missing: %v", err)
	}
}

func TestSavePhotosUsesSandboxWriter(t *testing.T) {
	r, _, _ := fixture(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "workspace"), 0755); err != nil {
		t.Fatal(err)
	}
	ws := &sandboxWorkspace{root: rootfs}
	r.Workspaces = ws
	r.Sink = &photoSink{}
	m := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 78, MessageID: 42, Text: "pic", PhotoIDs: []string{"p1"}}
	if err := r.savePhotos(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if ws.writes != 1 || !strings.HasPrefix(ws.lastName, "photo-42-1.") {
		t.Fatalf("sandbox photo writer not used: %+v", ws)
	}
	if len(m.Images) != 1 {
		t.Fatalf("vision missing: %+v", m.Images)
	}
}

func TestSweepUsesSandboxCleaner(t *testing.T) {
	r, _, _ := fixture(t)
	base := t.TempDir()
	rootfs := filepath.Join(base, "rootfs")
	inbox := filepath.Join(rootfs, "workspace", "inbox")
	if err := os.MkdirAll(inbox, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(inbox, "stale.txt")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	ws := &sandboxWorkspace{root: rootfs}
	r.Workspaces = ws
	msg := platform.Message{Platform: "telegram", ChatID: 55, UserID: 55}
	if err := r.Store.RecordChat(msg, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Point the recorded chat at our sandbox root via Peek.
	// RecordChat keys by WorkspaceKey; override Workspaces to return rootfs
	// for any route by using a single-chat store (fixture is fresh, so only
	// this chat exists).
	if err := r.sweepInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ws.clears != 1 {
		t.Fatalf("sandbox cleaner not used: %+v", ws)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale file not cleared via sandbox")
	}
}
