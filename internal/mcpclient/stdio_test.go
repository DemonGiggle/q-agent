package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
	"q-agent/internal/redaction"
	"q-agent/internal/sandbox"
)

// Fake credential material for filter/redaction tests. The TOKEN name
// triggers FilterEnvironment; the sk- value triggers token redaction.
const (
	stdioTestSecretEnv   = "Q_AGENT_TEST_CRED_TOKEN"
	stdioTestSecretValue = "supersecret-marker-value"
	stdioTestFakeToken   = "sk-TESTREDACTED0000000000"
)

// TestMCPStdioHelper is the child half of the stdio tests: when spawned
// with Q_AGENT_MCP_HELPER=1 it serves MCP over stdio instead of running
// tests. os.Exit bypasses the testing framework's stdout summary so the
// protocol stream stays clean.
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("Q_AGENT_MCP_HELPER") != "1" {
		return
	}
	if os.Getenv("Q_AGENT_MCP_HELPER_MODE") != "server" {
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
	for i := 0; i < 50; i++ {
		fmt.Fprintf(os.Stderr, "helper noise %d\n", i)
	}
	fmt.Fprintln(os.Stderr, "helper credential "+stdioTestFakeToken+" end")
	srv := mcp.NewServer(&mcp.Implementation{Name: "stdio-helper", Version: "v0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping", Description: "pong"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		fmt.Fprintln(os.Stderr, "helper ping "+stdioTestFakeToken)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "getenv", Description: "read env"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: os.Getenv(args.Name)}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "cwd", Description: "working dir"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		dir, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: dir}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "show_args", Description: "argv"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		encoded, err := json.Marshal(os.Args)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "pid", Description: "pid"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strconv.Itoa(os.Getpid())}}}, nil, nil
	})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "helper server failed")
		os.Exit(1)
	}
	os.Exit(0)
}

// stdioHelperServer builds the config entry that spawns this test binary
// as an MCP server, with marker arguments proving literal argv passing.
func stdioHelperServer(marker string) config.MCPServer {
	return config.MCPServer{
		Enabled:        true,
		Transport:      "stdio",
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestMCPStdioHelper$", marker},
		ConnectTimeout: "15s",
		CallTimeout:    "15s",
	}
}

// stdioCallTool calls one helper tool and returns its first text result.
func stdioCallTool(t *testing.T, ctx context.Context, task *Task, server, tool string, args any) string {
	t.Helper()
	session, ok := task.Session(server)
	if !ok || session == nil {
		t.Fatalf("no live session for %s", server)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("call %s returned no content", tool)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text == nil {
		t.Fatalf("call %s returned non-text content", tool)
	}
	return text.Text
}

func stdioDial(t *testing.T, ctx context.Context, factory TransportFactory, server config.MCPServer) SDKTransport {
	t.Helper()
	transport, err := factory(ctx, "test", server)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if transport == nil {
		t.Fatal("dial returned nil transport")
	}
	return transport
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

func TestHostStdioValidation(t *testing.T) {
	ctx := context.Background()
	factory := HostStdioFactory(t.TempDir(), nil, nil)
	base := stdioHelperServer("marker")
	for _, tc := range []struct {
		name   string
		mutate func(*config.MCPServer)
	}{
		{"wrong transport", func(s *config.MCPServer) { s.Transport = "http" }},
		{"empty command", func(s *config.MCPServer) { s.Command = "  " }},
		{"url set", func(s *config.MCPServer) { s.URL = "https://example.com" }},
		{"too many args", func(s *config.MCPServer) { s.Args = make([]string, maxStdioArgs+1) }},
		{"oversized arg", func(s *config.MCPServer) { s.Args = []string{strings.Repeat("x", maxStdioArgBytes+1)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := base
			tc.mutate(&server)
			if _, err := factory(ctx, "bad", server); err == nil {
				t.Fatal("expected dial error")
			}
		})
	}
	if _, err := HostStdioFactory("", nil, nil)(ctx, "x", base); err == nil {
		t.Fatal("expected working directory error")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := factory(cancelled, "x", base); err == nil {
		t.Fatal("expected cancellation error")
	}
}

// TestHostStdioHandshakeToolsAndTeardown exercises the full host path: real
// spawn with literal argv in an isolated working dir, credential-filtered
// environment, concurrent stderr during a valid session, and bounded child
// cleanup on close.
func TestHostStdioHandshakeToolsAndTeardown(t *testing.T) {
	if os.Getenv("Q_AGENT_MCP_HELPER") == "1" {
		t.Skip("helper process")
	}
	dir := t.TempDir()
	pwn := filepath.Join(dir, "PWN-MARKER-FILE")
	marker := "MARKER x;y $(touch " + pwn + ") `id` $HOME 'q\"q'"
	secretEnv := stdioTestSecretEnv
	t.Setenv(secretEnv, stdioTestSecretValue)
	t.Setenv("Q_AGENT_MCP_HELPER", "1")
	t.Setenv("Q_AGENT_MCP_HELPER_MODE", "server")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	factory := HostStdioFactory(dir, nil, nil)
	servers := map[string]config.MCPServer{"helper": stdioHelperServer(marker)}
	task := StartTask(ctx, servers, factory)
	defer task.CloseTask()

	statuses := task.Statuses()
	if len(statuses) != 1 || !statuses[0].Available {
		t.Fatalf("server unavailable: %+v", statuses)
	}
	if statuses[0].ProtocolVersion == "" {
		t.Fatalf("missing negotiated version: %+v", statuses[0])
	}

	// Arguments cross literally: no shell splitting or substitution.
	raw := stdioCallTool(t, ctx, task, "helper", "show_args", nil)
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		t.Fatalf("show_args: %v", err)
	}
	found := false
	for _, arg := range argv {
		if arg == marker {
			found = true
		}
	}
	if !found {
		t.Fatalf("marker arg missing (shell interpolation?): %q", argv)
	}
	if _, err := os.Stat(pwn); !os.IsNotExist(err) {
		t.Fatalf("marker payload executed: %v", err)
	}

	// Working directory is the chat root.
	if got := stdioCallTool(t, ctx, task, "helper", "cwd", nil); got != dir {
		t.Fatalf("cwd = %q, want %q", got, dir)
	}

	// Credentials are filtered; ordinary variables are inherited.
	if got := stdioCallTool(t, ctx, task, "helper", "getenv", map[string]any{"name": secretEnv}); got != "" {
		t.Fatalf("credential leaked to child: %q", got)
	}
	if got := stdioCallTool(t, ctx, task, "helper", "getenv", map[string]any{"name": "PATH"}); got == "" {
		t.Fatal("PATH missing from child environment")
	}

	// Valid stdout stays usable while stderr is noisy.
	if got := stdioCallTool(t, ctx, task, "helper", "ping", nil); got != "pong" {
		t.Fatalf("ping = %q", got)
	}

	// Close is idempotent and reaps the child within the bound.
	pid, err := strconv.Atoi(stdioCallTool(t, ctx, task, "helper", "pid", nil))
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	if !pidAlive(pid) {
		t.Fatal("child not running before close")
	}
	if err := task.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	task.CloseTask()
	deadline := time.Now().Add(15 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("child %d survived close", pid)
	}
	if _, ok := task.Session("helper"); ok {
		t.Fatal("session usable after close")
	}
}

// TestHostStdioStderrRedacted dials directly and asserts stderr diagnostics
// are captured but credential-shaped values are masked.
func TestHostStdioStderrRedacted(t *testing.T) {
	if os.Getenv("Q_AGENT_MCP_HELPER") == "1" {
		t.Skip("helper process")
	}
	t.Setenv("Q_AGENT_MCP_HELPER", "1")
	t.Setenv("Q_AGENT_MCP_HELPER_MODE", "server")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transport := stdioDial(t, ctx, HostStdioFactory(t.TempDir(), nil, nil), stdioHelperServer("m"))
	conn, err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	diag, ok := conn.(interface{ Diagnostic() string })
	if !ok {
		t.Fatal("host conn lacks Diagnostic")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := diag.Diagnostic()
		if strings.Contains(text, "helper starting") || strings.Contains(text, "helper noise") {
			if strings.Contains(text, stdioTestFakeToken) {
				t.Fatalf("raw token in diagnostics: %q", text)
			}
			if !strings.Contains(text, redaction.Marker) {
				t.Fatalf("expected redaction marker in diagnostics: %q", text)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stderr diagnostics captured: %q", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHostStdioExplicitEnv covers the configured-environment path: listed
// values pass through, credential names are still filtered, and nothing is
// injected.
func TestHostStdioExplicitEnv(t *testing.T) {
	if os.Getenv("Q_AGENT_MCP_HELPER") == "1" {
		t.Skip("helper process")
	}
	dir := t.TempDir()
	t.Setenv("Q_AGENT_MCP_HELPER", "1")
	t.Setenv("Q_AGENT_MCP_HELPER_MODE", "server")
	t.Setenv("Q_AGENT_PARENT_ONLY", "parent-value")
	env := []string{
		"PATH=/usr/bin:/bin",
		"Q_AGENT_PLAIN_VISIBLE=yes-plain",
		"Q_AGENT_MCP_HELPER=1",
		"Q_AGENT_MCP_HELPER_MODE=server",
		stdioTestSecretEnv + "=" + stdioTestSecretValue,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	factory := HostStdioFactory(dir, env, nil)
	task := StartTask(ctx, map[string]config.MCPServer{"helper": stdioHelperServer("m")}, factory)
	defer task.CloseTask()
	if statuses := task.Statuses(); len(statuses) != 1 || !statuses[0].Available {
		t.Fatalf("server unavailable: %+v", statuses)
	}
	if got := stdioCallTool(t, ctx, task, "helper", "getenv", map[string]any{"name": "Q_AGENT_PLAIN_VISIBLE"}); got != "yes-plain" {
		t.Fatalf("plain env missing: %q", got)
	}
	if got := stdioCallTool(t, ctx, task, "helper", "getenv", map[string]any{"name": stdioTestSecretEnv}); got != "" {
		t.Fatalf("credential leaked: %q", got)
	}
	if got := stdioCallTool(t, ctx, task, "helper", "getenv", map[string]any{"name": "Q_AGENT_PARENT_ONLY"}); got != "" {
		t.Fatalf("unlisted parent variable injected: %q", got)
	}
}

// TestHostStdioCancelDuringStartup asserts a cancelled setup never leaves
// a child behind and reports cancellation.
func TestHostStdioCancelDuringStartup(t *testing.T) {
	ctx := context.Background()
	factory := HostStdioFactory(t.TempDir(), nil, nil)
	server := stdioHelperServer("m")
	transport := stdioDial(t, ctx, factory, server)

	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := transport.Connect(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("connect with cancelled ctx = %v", err)
	}

	// A task whose parent is already cancelled marks the server
	// unavailable without spawning.
	task := StartTask(cancelled, map[string]config.MCPServer{"helper": server}, factory)
	defer task.CloseTask()
	statuses := task.Statuses()
	if len(statuses) != 1 || statuses[0].Available {
		t.Fatalf("cancelled task unexpectedly available: %+v", statuses)
	}
}

// TestHostStdioMissingBinary is fail-closed: a bad command reports a
// definitive dial/connect error with no host details beyond one line.
func TestHostStdioMissingBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	factory := HostStdioFactory(t.TempDir(), nil, nil)
	server := stdioHelperServer("m")
	server.Command = "/nonexistent-mcp-server-binary"
	transport := stdioDial(t, ctx, factory, server)
	if _, err := transport.Connect(ctx); err == nil {
		t.Fatal("expected connect error")
	} else if strings.Contains(err.Error(), "\n") {
		t.Fatalf("multiline connect error: %q", err)
	}
}

func TestSandboxStdioValidation(t *testing.T) {
	ctx := context.Background()
	base := stdioHelperServer("m")
	if _, err := SandboxStdioFactory(sandbox.Options{}, "", nil)(ctx, "x", base); err == nil {
		t.Fatal("expected rootfs error")
	}
	bad := base
	bad.Command = ""
	if _, err := SandboxStdioFactory(sandbox.Options{}, "/rootfs", nil)(ctx, "x", bad); err == nil {
		t.Fatal("expected command error")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := SandboxStdioFactory(sandbox.Options{}, "/rootfs", nil)(cancelled, "x", base); err == nil {
		t.Fatal("expected cancellation error")
	}
}

// TestSandboxStdioFailClosed asserts a sandbox setup failure never falls
// back to host execution: the error is sandbox-attributed and no session
// results.
func TestSandboxStdioFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	broken := sandbox.Options{Bubblewrap: "/nonexistent/bwrap-for-test", NewUIDMap: "/nonexistent/newuidmap", NewGIDMap: "/nonexistent/newgidmap", Worker: "/nonexistent/worker"}
	factory := SandboxStdioFactory(broken, "/nonexistent-rootfs", nil)
	transport := stdioDial(t, ctx, factory, stdioHelperServer("m"))
	_, err := transport.Connect(ctx)
	if err == nil {
		t.Fatal("expected sandbox setup error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "sandbox") {
		t.Fatalf("setup error is not sandbox-attributed (possible host fallback?): %v", err)
	}

	task := StartTask(ctx, map[string]config.MCPServer{"helper": stdioHelperServer("m")}, factory)
	defer task.CloseTask()
	statuses := task.Statuses()
	if len(statuses) != 1 || statuses[0].Available {
		t.Fatalf("broken sandbox unexpectedly available: %+v", statuses)
	}
	if strings.Contains(statuses[0].Error, os.Args[0]) {
		t.Fatalf("status leaks host details: %q", statuses[0].Error)
	}
}
