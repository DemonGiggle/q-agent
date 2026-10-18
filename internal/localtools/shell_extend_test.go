//go:build linux

package localtools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
)

func TestBufferLastLines(t *testing.T) {
	var b Buffer
	for _, line := range []string{"one", "two", "three", "four"} {
		b.Write([]byte(line + "\n"))
	}
	got := b.LastLines(3)
	if len(got) != 3 || got[0] != "two" || got[1] != "three" || got[2] != "four" {
		t.Fatalf("last3=%q", got)
	}
	if got := b.LastLines(10); len(got) != 4 {
		t.Fatalf("all=%q", got)
	}
	var empty Buffer
	if got := empty.LastLines(3); len(got) != 0 {
		t.Fatalf("empty=%q", got)
	}
}

func shellCall(command string) llm.ToolCall {
	raw, _ := json.Marshal(map[string]string{"command": command})
	return llm.ToolCall{Name: "shell", Arguments: raw}
}

func TestRunShellTimeoutKillsWithoutPrompt(t *testing.T) {
	_, err := Execute(context.Background(), shellCall(`printf out; sleep 5`), Options{WorkingDir: t.TempDir(), ShellTimeout: 100 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestRunShellExtendContinuesSameProcess(t *testing.T) {
	// Prints 3 lines then sleeps; extend keeps the same process alive.
	cmd := `printf 'a\nb\nc\n'; sleep 1; printf 'done\n'`
	calls := 0
	var seen []string
	opts := Options{WorkingDir: t.TempDir(), ShellTimeout: 300 * time.Millisecond, MaxShellExtends: 10}
	opts.OnShellTimeout = func(ctx context.Context, command string, lastLines []string) bool {
		calls++
		seen = append([]string(nil), lastLines...)
		return true
	}
	out, err := Execute(context.Background(), shellCall(cmd), opts)
	if err != nil {
		t.Fatalf("extend err=%v", err)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("output lost after extend: %q", out)
	}
	if calls == 0 {
		t.Fatal("prompt was not asked")
	}
	if len(seen) != 3 || seen[0] != "a" || seen[2] != "c" {
		t.Fatalf("last3=%q", seen)
	}
}

func TestRunShellKillOnDecision(t *testing.T) {
	opts := Options{WorkingDir: t.TempDir(), ShellTimeout: 100 * time.Millisecond}
	opts.OnShellTimeout = func(context.Context, string, []string) bool { return false }
	_, err := Execute(context.Background(), shellCall(`printf 'x\ny\nz\n'; sleep 5`), opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}
