//go:build linux

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/workspace"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func execute(t *testing.T, runner *Runner, ctx context.Context, name string, args map[string]string) (string, error) {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return runner.Execute(ctx, llm.ToolCall{Name: name, Arguments: data})
}

func TestHostToolsAndRestart(t *testing.T) {
	dir := t.TempDir()
	manager := &workspace.HostManager{Dir: filepath.Join(dir, "host-workspaces")}
	msg := platform.Message{Platform: "telegram", ChatID: 1, TopicID: 10}
	root, err := manager.Ensure(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	other, err := manager.Ensure(context.Background(), platform.Message{Platform: "telegram", ChatID: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("Q_AGENT_VISIBLE", "preserved-value")
	t.Setenv("Q_AGENT_CUSTOM_AUTH", "configured-test-value")
	t.Setenv("Q_AGENT_hOsT_tOkEn", "pattern-test-value")
	environment, _ := redaction.FilterEnvironment(os.Environ(), "Q_AGENT_CUSTOM_AUTH")
	runner := &Runner{Options: localtools.Options{WorkingDir: root, Environment: environment}}
	call := func(name string, args map[string]string) string {
		t.Helper()
		out, err := execute(t, runner, context.Background(), name, args)
		if err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
		return out
	}
	call("write", map[string]string{"path": "relative.txt", "content": "first"})
	call("edit", map[string]string{"path": "relative.txt", "old_text": "first", "new_text": "second"})
	if out := call("read", map[string]string{"path": "relative.txt"}); out != "second" {
		t.Fatal(out)
	}
	if out := call("list", map[string]string{}); !strings.Contains(out, "relative.txt") {
		t.Fatal(out)
	}
	if out := call("search", map[string]string{"pattern": "second"}); !strings.Contains(out, filepath.Join(root, "relative.txt")+":1:second") {
		t.Fatal(out)
	}
	// Absolute paths use the host filesystem for all file tools.
	absolute := filepath.Join(dir, "absolute.txt")
	call("write", map[string]string{"path": absolute, "content": "host"})
	call("edit", map[string]string{"path": absolute, "old_text": "host", "new_text": "absolute"})
	if out := call("read", map[string]string{"path": absolute}); out != "absolute" {
		t.Fatal(out)
	}
	if out := call("list", map[string]string{"path": dir}); !strings.Contains(out, "absolute.txt") {
		t.Fatal(out)
	}
	if out := call("search", map[string]string{"path": absolute, "pattern": "absolute"}); !strings.Contains(out, absolute+":1:absolute") {
		t.Fatal(out)
	}
	command := `test -z "$Q_AGENT_CUSTOM_AUTH$Q_AGENT_hOsT_tOkEn" && printf '%s\n' "$Q_AGENT_VISIBLE" "$HOME" "$PATH" && pwd && id -u && id -g`
	out := call("shell", map[string]string{"command": command})
	want := fmt.Sprintf("preserved-value\n%s\n%s\n%s\n%d\n%d\n", os.Getenv("HOME"), os.Getenv("PATH"), root, os.Geteuid(), os.Getegid())
	if out != want {
		t.Fatalf("shell environment/permissions/cwd mismatch: %s", out)
	}
	otherRunner := &Runner{Options: localtools.Options{WorkingDir: other, Environment: environment}}
	if _, err := execute(t, otherRunner, context.Background(), "read", map[string]string{"path": "relative.txt"}); !os.IsNotExist(err) {
		t.Fatalf("chat folders mixed: %v", err)
	}
	msg.TopicID++
	restarted := &workspace.HostManager{Dir: manager.Dir}
	again, err := restarted.Ensure(context.Background(), msg)
	if err != nil || again != root {
		t.Fatal("topic/restart lost workspace")
	}
	runner.Options.WorkingDir = again
	if out := call("read", map[string]string{"path": "relative.txt"}); out != "second" {
		t.Fatal("restart lost edits")
	}
}

func TestHostConcurrentDirectoriesAndEnvironments(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		root := filepath.Join(t.TempDir(), "folder with spaces")
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		value := fmt.Sprintf("chat-%d", i)
		runner := &Runner{Options: localtools.Options{WorkingDir: root, Environment: []string{"CHAT=" + value, "PATH=/usr/bin:/bin"}}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				out, err := execute(t, runner, context.Background(), "shell", map[string]string{"command": `printf '%s' "$CHAT" > value; cat value`})
				if err != nil || out != value {
					t.Errorf("concurrent shell=%q err=%v", out, err)
				}
				out, err = execute(t, runner, context.Background(), "read", map[string]string{"path": "value"})
				if err != nil || out != value {
					t.Errorf("concurrent read=%q err=%v", out, err)
				}
			}
		}()
	}
	wg.Wait()
	after, err := os.Getwd()
	if err != nil || after != cwd {
		t.Fatal("tools changed process-wide directory")
	}
}

func TestHostOutputBounds(t *testing.T) {
	root := t.TempDir()
	runner := &Runner{Options: localtools.Options{WorkingDir: root}}
	if err := os.WriteFile(filepath.Join(root, "large"), []byte(strings.Repeat("😀", 5000)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read", "shell"} {
		args := map[string]string{"path": "large"}
		if name == "shell" {
			args = map[string]string{"command": "cat large"}
		}
		out, err := execute(t, runner, context.Background(), name, args)
		if err != nil || len(out) > localtools.OutputLimit || !utf8.ValidString(out) || !strings.HasSuffix(out, localtools.Truncation) {
			t.Fatalf("%s output bound: len=%d err=%v", name, len(out), err)
		}
	}
}

func TestHostShellCleanup(t *testing.T) {
	for _, kind := range []string{"completion", "timeout", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			runner := &Runner{Options: localtools.Options{WorkingDir: root}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command := `(sleep 1; touch late) >/dev/null 2>&1 & printf started`
			var want error
			if kind != "completion" {
				command += `; sleep 30`
			}
			if kind == "timeout" {
				runner.Options.ShellTimeout = 100 * time.Millisecond
				want = context.DeadlineExceeded
			}
			if kind == "cancellation" {
				command = `touch started; ` + command
				want = context.Canceled
				go func() {
					deadline := time.NewTimer(3 * time.Second)
					defer deadline.Stop()
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ticker.C:
							if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
								cancel()
								return
							}
						case <-deadline.C:
							cancel()
							return
						}
					}
				}()
			}
			start := time.Now()
			_, err := execute(t, runner, ctx, "shell", map[string]string{"command": command})
			if !errors.Is(err, want) || time.Since(start) > 3*time.Second {
				t.Fatalf("cleanup err=%v duration=%s", err, time.Since(start))
			}
			time.Sleep(1100 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(root, "late")); !os.IsNotExist(err) {
				t.Fatal("background child survived process-group cleanup")
			}
		})
	}
}

func TestHostToolSchemas(t *testing.T) {
	runner := &Runner{Options: localtools.Options{WorkingDir: "/host/chat"}}
	hostTools, sandboxTools := runner.Schemas(), localtools.Schemas()
	if len(hostTools) != 6 {
		t.Fatal("six tools required")
	}
	for i := range hostTools {
		if hostTools[i].Name != sandboxTools[i].Name || strings.Contains(hostTools[i].Description, "sandbox") {
			t.Fatal("incorrect host tool description")
		}
		for _, params := range []map[string]any{hostTools[i].Parameters, sandboxTools[i].Parameters} {
			for _, field := range params["properties"].(map[string]any) {
				delete(field.(map[string]any), "description")
			}
		}
		if !reflect.DeepEqual(hostTools[i].Parameters, sandboxTools[i].Parameters) {
			t.Fatalf("changed %s argument schema", hostTools[i].Name)
		}
	}
	if !strings.Contains(runner.SystemPrompt(), "/host/chat") || !strings.Contains(runner.SystemPrompt(), "absolute paths refer to the host filesystem") {
		t.Fatal("incorrect host system prompt")
	}
}
