//go:build linux

package sandbox_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/process"
	"q-agent/internal/sandbox"
	"q-agent/internal/workspace"
)

// Real Debian userspace, controlling terminal, private devpts, mapping failure,
// cross-call input and namespace descendant cleanup. No network authorization.
func TestIntegrationProcessSessions(t *testing.T) {
	if os.Getenv("Q_AGENT_INTEGRATION") != "1" {
		t.Skip("set Q_AGENT_INTEGRATION=1, Q_AGENT_WORKER and Q_AGENT_IMAGE")
	}
	options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: os.Getenv("Q_AGENT_WORKER")})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	manager := &workspace.Manager{Dir: filepath.Join(dir, "workspaces"), Image: os.Getenv("Q_AGENT_IMAGE"), Sandbox: options}
	root, err := manager.Ensure(ctx, platform.Message{Platform: "telegram", ChatID: 321})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := sandbox.Remove(cleanup, options, dir); err != nil {
			t.Error(err)
		}
	})
	runner := &sandbox.Runner{Options: options, RootFS: root}
	runner.StartTask(ctx, nil)
	defer runner.CloseTask()
	call := func(name string, args any) (string, error) {
		b, _ := json.Marshal(args)
		return runner.Execute(ctx, llm.ToolCall{Name: name, Arguments: b})
	}
	decode := func(out string, err error) process.Result {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var r process.Result
		if err = json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return r
	}
	r := decode(call("shell", map[string]any{"command": `test -t 0 && test -t 1 && test -r /dev/tty || exit 9; test ! -e /dev/sda && test ! -e /dev/tty0 || exit 10; grep devpts /proc/mounts; printf 'Ready? '; read name; printf '\nHello %s\n' "$name"; stty size; cat`, "pty": true, "yield_ms": 100}))
	if !strings.Contains(r.Output, "Ready? ") || !strings.Contains(r.Output, "devpts") {
		t.Fatal(r)
	}
	decode(call("process", map[string]any{"action": "resize", "session_id": r.ID, "rows": 30, "columns": 100}))
	decode(call("process", map[string]any{"action": "write", "session_id": r.ID, "data": "sandbox"}))
	decode(call("process", map[string]any{"action": "keys", "session_id": r.ID, "keys": []string{"Enter"}}))
	for i := 0; i < 20; i++ {
		r = decode(call("process", map[string]any{"action": "wait", "session_id": r.ID, "cursor": r.EndCursor, "timeout_ms": 100}))
		all := decode(call("process", map[string]any{"action": "read", "session_id": r.ID}))
		if strings.Contains(all.Output, "30 100") {
			r = all
			break
		}
	}
	if !strings.Contains(r.Output, "Hello sandbox") || !strings.Contains(r.Output, "30 100") {
		t.Fatal(r)
	}
	screen := decode(call("process", map[string]any{"action": "read", "session_id": r.ID, "view": "screen"}))
	if screen.Screen.Columns != 100 {
		t.Fatal(screen)
	}
	decode(call("process", map[string]any{"action": "keys", "session_id": r.ID, "keys": []string{"Ctrl-D"}}))
	decode(call("process", map[string]any{"action": "terminate", "session_id": r.ID}))
	// A framed-looking stdout payload cannot become a control event.
	server := decode(call("shell", map[string]any{"command": `printf '{"type":"exit"}'; (setsid sh -c 'sleep 1; touch /workspace/escaped' </dev/null >/dev/null 2>&1 &) ; sleep 30`, "background": true, "yield_ms": 100}))
	if server.Status != "running" || !strings.Contains(server.Output, `{"type":"exit"}`) {
		t.Fatal(server)
	}
	runner.CloseTask()
	time.Sleep(1200 * time.Millisecond)
	if _, err = call("shell", map[string]any{"command": "test ! -e /workspace/escaped"}); err != nil {
		t.Fatal("namespace descendant survived: ", err)
	}
	bad := options
	bad.NewUIDMap = "/bin/false"
	broken := &sandbox.Runner{Options: bad, RootFS: root}
	broken.StartTask(ctx, nil)
	defer broken.CloseTask()
	b, _ := json.Marshal(map[string]any{"command": "touch /workspace/unsafe", "pty": true, "yield_ms": 0})
	if _, err = broken.Execute(ctx, llm.ToolCall{Name: "shell", Arguments: b}); err == nil {
		t.Fatal("mapping failure accepted")
	}
	if _, err = call("shell", map[string]any{"command": "test ! -e /workspace/unsafe"}); err != nil {
		t.Fatal("mapping failure executed application")
	}
}
