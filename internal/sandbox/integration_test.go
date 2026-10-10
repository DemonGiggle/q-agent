//go:build linux

package sandbox_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/platform"
	"q-agent/internal/sandbox"
	"q-agent/internal/workspace"
	"strings"
	"testing"
	"time"
)

func TestIntegrationIsolationAndPackages(t *testing.T) {
	if os.Getenv("Q_AGENT_INTEGRATION") != "1" {
		t.Skip("set Q_AGENT_INTEGRATION=1, Q_AGENT_WORKER and Q_AGENT_IMAGE for real Linux integration")
	}
	worker, image := os.Getenv("Q_AGENT_WORKER"), os.Getenv("Q_AGENT_IMAGE")
	if worker == "" || image == "" {
		t.Fatal("Q_AGENT_WORKER and Q_AGENT_IMAGE are required")
	}
	options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: worker, ShellTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err = workspace.CheckImage(image); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	hostOnly := filepath.Join(dir, "host-only-marker")
	if err := os.WriteFile(hostOnly, []byte("host-only test data"), 0600); err != nil {
		t.Fatal(err)
	}
	quotedHostOnly := "'" + strings.ReplaceAll(hostOnly, "'", "'\"'\"'") + "'"
	manager := &workspace.Manager{Dir: filepath.Join(dir, "workspaces"), Image: image, Sandbox: options}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sandbox.Remove(ctx, options, dir); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	a := platform.Message{Platform: "telegram", ChatID: 100}
	b := platform.Message{Platform: "telegram", ChatID: 200}
	start := time.Now()
	rootA, err := manager.Ensure(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("workspace_creation=%s", time.Since(start))
	rootB, err := manager.Ensure(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	run := func(root, name string, args any) (string, error) {
		data, _ := json.Marshal(args)
		return (&sandbox.Runner{Options: options, RootFS: root}).Execute(ctx, llm.ToolCall{Name: name, Arguments: data})
	}
	shell := func(root, command string) string {
		t.Helper()
		out, err := run(root, "shell", map[string]string{"command": command})
		if err != nil {
			t.Fatalf("shell: %v\n%s", err, out)
		}
		return out
	}
	t.Setenv("Q_AGENT_TEST_SECRET", "must-never-be-in-sandbox")
	t.Setenv("TELEGRAM_BOT_TOKEN", "must-never-be-in-sandbox")
	start = time.Now()
	out := shell(rootA, "id; cat /proc/self/uid_map; cat /proc/self/gid_map; grep '^CapEff:' /proc/self/status; env; test ! -e "+quotedHostOnly+" && test ! -e /target && test ! -e /image.tar && test ! -w /.q-agent-worker")
	t.Logf("tool_launch=%s", time.Since(start))
	if !strings.Contains(out, "uid=0(root)") || strings.Contains(out, "must-never") || !strings.Contains(out, "00000000000400db") {
		t.Fatalf("mapping/capabilities/environment: %s", out)
	}
	shell(rootA, "printf one > /workspace/value; printf system > /etc/q-agent-persistence; printf temp > /tmp/ephemeral; printf temp > /run/ephemeral")
	if got, err := run(rootA, "edit", map[string]string{"path": "/workspace/value", "old_text": "one", "new_text": "two"}); err != nil {
		t.Fatalf("edit: %s %v", got, err)
	}
	if got := shell(rootA, "test ! -e /tmp/ephemeral; test ! -e /run/ephemeral; cat /workspace/value /etc/q-agent-persistence"); got != "twosystem" {
		t.Fatal(got)
	}
	shell(rootB, "test ! -e /workspace/value; test ! -e /etc/q-agent-persistence; ! command -v jq")
	if got, err := run(rootA, "list", map[string]string{"path": "/workspace"}); err != nil || !strings.Contains(got, "value") {
		t.Fatalf("list: %s %v", got, err)
	}
	if got, err := run(rootA, "search", map[string]string{"path": "/workspace", "pattern": "two"}); err != nil || !strings.Contains(got, "value:1:two") {
		t.Fatalf("search: %s %v", got, err)
	}
	if got, err := run(rootA, "write", map[string]string{"path": "/workspace/written", "content": "ok"}); err != nil {
		t.Fatalf("write: %s %v", got, err)
	}
	if got, err := run(rootA, "read", map[string]string{"path": "/workspace/written"}); err != nil || got != "ok" {
		t.Fatalf("read: %s %v", got, err)
	}
	out = shell(rootA, "yes '😀\\\"' | head -c 20000")
	if len(out) > localtools.OutputLimit || !strings.Contains(out, localtools.Truncation) {
		t.Fatalf("output bound: %d", len(out))
	}
	shell(rootA, "(sleep 2; touch /workspace/late) >/dev/null 2>&1 & echo started")
	time.Sleep(2200 * time.Millisecond)
	shell(rootA, "test ! -e /workspace/late")
	shortCtx, shortCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	start = time.Now()
	_, err = (&sandbox.Runner{Options: options, RootFS: rootA}).Execute(shortCtx, llm.ToolCall{Name: "shell", Arguments: json.RawMessage(`{"command":"sleep 30; touch /workspace/cancelled"}`)})
	shortCancel()
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation: %v %s", err, time.Since(start))
	}
	shell(rootA, "test ! -e /workspace/cancelled")
	shortOptions := options
	shortOptions.ShellTimeout = 100 * time.Millisecond
	_, err = (&sandbox.Runner{Options: shortOptions, RootFS: rootA}).Execute(ctx, llm.ToolCall{Name: "shell", Arguments: json.RawMessage(`{"command":"sleep 30"}`)})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("shell timeout: %v", err)
	}
	t.Log("installing jq in workspace A")
	shell(rootA, "apt-get update && apt-get install -y jq && jq --version")
	if got := shell(rootA, "jq -n '1+1'"); strings.TrimSpace(got) != "2" {
		t.Fatal(got)
	}
	restarted := &workspace.Manager{Dir: manager.Dir, Image: image, Sandbox: options}
	again, err := restarted.Ensure(ctx, a)
	if err != nil || again != rootA {
		t.Fatalf("restart: %s %v", again, err)
	}
	if got := shell(again, "jq -n '1+1'; cat /workspace/value"); !strings.Contains(got, "2\ntwo") {
		t.Fatal(got)
	}
	shell(rootB, "! command -v jq; test ! -e /workspace/value")
	bad := options
	bad.NewUIDMap = "/bin/false"
	_, err = (&sandbox.Runner{Options: bad, RootFS: rootA}).Execute(ctx, llm.ToolCall{Name: "shell", Arguments: json.RawMessage(`{"command":"touch /workspace/unsafe"}`)})
	if err == nil {
		t.Fatal("mapping failure executed tools")
	}
	shell(rootA, "test ! -e /workspace/unsafe")
	// Fail after writing an archive entry, rather than only failing a mount.
	badImage := filepath.Join(dir, "invalid.tar")
	f, e := os.Create(badImage)
	if e != nil {
		t.Fatal(e)
	}
	tw := tar.NewWriter(f)
	for _, h := range []tar.Header{{Name: ".", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "partial", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}, {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}} {
		if e = tw.WriteHeader(&h); e != nil {
			t.Fatal(e)
		}
		if h.Size == 1 {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	f.Close()
	partial := &workspace.Manager{Dir: filepath.Join(dir, "partial"), Image: badImage, Sandbox: options}
	if _, e = partial.Ensure(ctx, a); e == nil {
		t.Fatal("partial extraction succeeded")
	}
	entries, e := os.ReadDir(partial.Dir)
	if e != nil || len(entries) != 0 {
		t.Fatalf("partial provisioning was not cleaned up: %v %v", entries, e)
	}
	broken := &workspace.Manager{Dir: filepath.Join(dir, "broken"), Image: "/missing-image", Sandbox: options}
	_, err = broken.Ensure(ctx, a)
	if err == nil {
		t.Fatal("incomplete provisioning succeeded")
	}
	if _, err = os.Stat(filepath.Join(broken.Dir, a.WorkspaceKey())); !os.IsNotExist(err) {
		t.Fatalf("failed rootfs published: %v", err)
	}
}
