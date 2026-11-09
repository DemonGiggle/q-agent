//go:build linux

package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/sandbox"
	"q-agent/internal/workspace"
)

// Real namespace round-trip for the MCP stdio channel: split stdout and
// stderr streams, literal argv, /workspace working directory, graceful
// shutdown with reap, and fail-closed setup errors. Requires a Debian
// image with /bin/sh.
func TestIntegrationMCPStdio(t *testing.T) {
	if os.Getenv("Q_AGENT_INTEGRATION") != "1" {
		t.Skip("set Q_AGENT_INTEGRATION=1, Q_AGENT_WORKER and Q_AGENT_IMAGE")
	}
	worker, image := os.Getenv("Q_AGENT_WORKER"), os.Getenv("Q_AGENT_IMAGE")
	if worker == "" || image == "" {
		t.Fatal("Q_AGENT_WORKER and Q_AGENT_IMAGE are required")
	}
	options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: worker, ShellTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manager := &workspace.Manager{Dir: filepath.Join(dir, "workspaces"), Image: image, Sandbox: options}
	root, err := manager.Ensure(ctx, platform.Message{Platform: "telegram", ChatID: 471})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := sandbox.Remove(cleanup, options, dir); err != nil {
			t.Error(err)
		}
	})

	proc, err := sandbox.StartMCPStdio(ctx, options, root, "/bin/sh", []string{"-c", "pwd; echo err-line >&2; cat"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer proc.Close()

	// Stdout carries the working directory; stderr stays separate.
	if out := readUntil(t, proc.Reader(), "workspace", 30*time.Second); !strings.Contains(out, "/workspace") {
		t.Fatalf("pwd = %q", out)
	}
	if _, err := proc.Writer().Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out := readUntil(t, proc.Reader(), "ping", 30*time.Second); !strings.Contains(out, "ping") {
		t.Fatalf("echo = %q", out)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if diag := proc.Diagnostic(nil); strings.Contains(diag, "err-line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing stderr diagnostic: %q", proc.Diagnostic(nil))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Host files outside the rootfs are invisible inside.
	spy, err := sandbox.StartMCPStdio(ctx, options, root, "/bin/sh", []string{"-c", "test ! -e /host-only-marker && test ! -e /proc/1/root"})
	if err != nil {
		t.Fatalf("isolation start: %v", err)
	}
	defer spy.Close()
	if code := waitExit(t, spy, 30*time.Second); code != 0 {
		t.Fatalf("isolation probe exit = %d", code)
	}

	// Graceful shutdown reaps the namespace.
	if err := proc.Writer().Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if code := waitExit(t, proc, 30*time.Second); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("close must be idempotent: %v", err)
	}
}

// TestIntegrationMCPStdioFailClosed asserts sandbox setup failures are
// fatal boundary errors, never host fallbacks.
func TestIntegrationMCPStdioFailClosed(t *testing.T) {
	if os.Getenv("Q_AGENT_INTEGRATION") != "1" {
		t.Skip("set Q_AGENT_INTEGRATION=1, Q_AGENT_WORKER and Q_AGENT_IMAGE")
	}
	worker, image := os.Getenv("Q_AGENT_WORKER"), os.Getenv("Q_AGENT_IMAGE")
	if worker == "" || image == "" {
		t.Fatal("Q_AGENT_WORKER and Q_AGENT_IMAGE are required")
	}
	options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: worker, ShellTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	manager := &workspace.Manager{Dir: filepath.Join(dir, "workspaces"), Image: image, Sandbox: options}
	root, err := manager.Ensure(ctx, platform.Message{Platform: "telegram", ChatID: 472})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := sandbox.Remove(cleanup, options, dir); err != nil {
			t.Error(err)
		}
	})

	bad := options
	bad.NewUIDMap = "/bin/false"
	_, err = sandbox.StartMCPStdio(ctx, bad, root, "/bin/sh", []string{"-c", "true"})
	if err == nil {
		t.Fatal("expected sandbox setup failure")
	}
	var fatal interface{ Fatal() bool }
	if !errors.As(err, &fatal) || !fatal.Fatal() {
		t.Fatalf("setup failure must be fatal: %v", err)
	}

	// A missing server executable is ordinary, not fatal.
	_, err = sandbox.StartMCPStdio(ctx, options, root, "/nonexistent-mcp-server", nil)
	if err == nil {
		t.Fatal("expected server start failure")
	}
	if errors.As(err, &fatal) && fatal.Fatal() {
		t.Fatalf("server start failure must not be fatal: %v", err)
	}
}

func readUntil(t *testing.T, r interface{ Read([]byte) (int, error) }, substr string, timeout time.Duration) string {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	var out strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	for !strings.Contains(out.String(), substr) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for %q, got %q", substr, out.String())
		}
		ch := make(chan result, 1)
		go func() {
			n, err := r.Read(buf)
			ch <- result{n: n, err: err}
		}()
		select {
		case res := <-ch:
			if res.n > 0 {
				out.Write(buf[:res.n])
			}
			if res.err != nil {
				t.Fatalf("read: %v (got %q)", res.err, out.String())
			}
		case <-time.After(remaining):
			t.Fatalf("timed out waiting for %q, got %q", substr, out.String())
		}
	}
	return out.String()
}

func waitExit(t *testing.T, proc interface {
	Exited() (int, bool)
	Close() error
}, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if code, ok := proc.Exited(); ok {
			return code
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for server exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
