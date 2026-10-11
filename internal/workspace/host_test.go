package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"q-agent/internal/platform"
	"q-agent/internal/state"
	"testing"
)

func TestHostWorkspaceOwnershipAndPersistence(t *testing.T) {
	dir := t.TempDir()
	m := &HostManager{Dir: filepath.Join(dir, "host-workspaces")}
	a := platform.Message{Platform: "telegram", ChatID: 1, TopicID: 10}
	root, err := m.Ensure(context.Background(), a)
	if err != nil || root != filepath.Join(m.Dir, a.WorkspaceKey(), "workspace") {
		t.Fatalf("root=%s err=%v", root, err)
	}
	if st, err := os.Stat(root); err != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("workspace permissions: %v %v", st, err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep"), []byte("persistent"), 0600); err != nil {
		t.Fatal(err)
	}
	a.TopicID++
	reopened := &HostManager{Dir: m.Dir}
	again, err := reopened.Ensure(context.Background(), a)
	if err != nil || again != root {
		t.Fatalf("topic/restart changed folder: %s %v", again, err)
	}
	data, err := os.ReadFile(filepath.Join(again, "keep"))
	if err != nil || string(data) != "persistent" {
		t.Fatal("files lost")
	}
	b := a
	b.ChatID++
	other, err := reopened.Ensure(context.Background(), b)
	if err != nil || other == root {
		t.Fatalf("chats share folder: %s %v", other, err)
	}
	if _, err := os.Stat(filepath.Join(other, "keep")); !os.IsNotExist(err) {
		t.Fatal("chat files mixed")
	}
	if err := state.Atomic(filepath.Join(filepath.Dir(root), "owner.json"), Owner{Platform: "telegram", ChatID: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Ensure(context.Background(), a); err == nil {
		t.Fatal("changed ownership accepted")
	}
}

func TestHostWorkspaceRejectsIncompleteAndUnsafeTrees(t *testing.T) {
	for _, kind := range []string{"incomplete", "symlink base", "symlink workspace", "missing workspace", "invalid owner"} {
		t.Run(kind, func(t *testing.T) {
			m := &HostManager{Dir: t.TempDir()}
			msg := platform.Message{Platform: "telegram", ChatID: 1}
			base := filepath.Join(m.Dir, msg.WorkspaceKey())
			if kind == "symlink base" {
				if err := os.Symlink(t.TempDir(), base); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(base, 0700); err != nil {
					t.Fatal(err)
				}
				if kind != "incomplete" {
					if err := state.Atomic(filepath.Join(base, "owner.json"), Owner{msg.Platform, msg.ChatID}); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "symlink workspace" {
					if err := os.Symlink(t.TempDir(), filepath.Join(base, "workspace")); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "invalid owner" {
					if err := os.WriteFile(filepath.Join(base, "owner.json"), []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := m.Ensure(context.Background(), msg); err == nil {
				t.Fatal("unsafe publication accepted")
			}
		})
	}
}

func TestHostProvisionCancellationAndAbandonedStaging(t *testing.T) {
	m := &HostManager{Dir: t.TempDir()}
	msg := platform.Message{Platform: "telegram", ChatID: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Ensure(ctx, msg); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(m.Dir); len(entries) != 0 {
		t.Fatal("cancelled provisioning published state")
	}
	if err := os.Mkdir(filepath.Join(m.Dir, ".provision-abandoned"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}
