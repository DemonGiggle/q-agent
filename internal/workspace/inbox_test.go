package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"q-agent/internal/platform"
)

func TestValidateInboxRoot(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Dir: dir}
	// Valid layout: <dir>/<key>/rootfs must exist as a real dir.
	msg := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1}
	base := filepath.Join(dir, msg.WorkspaceKey())
	root := filepath.Join(base, "rootfs")
	if err := m.validateInboxRoot(root); err == nil {
		t.Fatal("missing root accepted")
	}
	if err := setupRoot(base, root); err != nil {
		t.Fatal(err)
	}
	if err := m.validateInboxRoot(root); err != nil {
		t.Fatalf("valid root rejected: %v", err)
	}
	for _, bad := range []string{
		dir,
		filepath.Join(dir, "other", "rootfs"),
		filepath.Join(dir, msg.WorkspaceKey(), "workspace"),
		base, // missing /rootfs suffix
	} {
		if err := m.validateInboxRoot(bad); err == nil {
			t.Fatalf("bad root accepted: %q", bad)
		}
	}
}

func setupRoot(base, root string) error {
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	return os.MkdirAll(root, 0700)
}

func TestManagerPeekMissSkipsSweep(t *testing.T) {
	m := &Manager{Dir: t.TempDir()}
	msg := platform.Message{Platform: "telegram", ChatID: 999, UserID: 999}
	if _, ok := m.Peek(msg); ok {
		t.Fatal("peek hit on empty dir")
	}
}
