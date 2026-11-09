// Deterministic end-to-end coverage for the composed MCP lifetime
// (issue #52, step 8/8 of epic #44).
//
// Every fixture here is SDK-backed and local: the stdio side re-executes
// this test binary as an MCP server (no external command), and the HTTP
// side serves the SDK Streamable HTTP handler over httptest loopback (no
// external network, no credentials). Tests exercise the full operator
// path — Wrap + CombinedDial + StartTask + Schemas/Execute + CloseTask —
// across both transports, both HTTP response modes (plain JSON and
// request-scoped SSE), partial failure, cancellation, and task isolation.
package mcpclient

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/agent"
	"q-agent/internal/config"
	"q-agent/internal/llm"
)

// coverage52Config builds one task config with a host stdio helper server
// ("local") and one Streamable HTTP fixture server ("remote").
func coverage52Config(t *testing.T, httpURL string) config.Config {
	t.Helper()
	return config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"local":  stdioHelperServer("coverage52"),
		"remote": {Enabled: true, Transport: "http", URL: httpURL, ConnectTimeout: "5s", CallTimeout: "5s"},
	}}}
}

// coverage52Wrap binds the stub base to both transports via the host stdio
// factory, mirroring how the runtime wires host-mode tasks.
func coverage52Wrap(t *testing.T, base *stubBase, cfg config.Config, dir string) *TaskTools {
	t.Helper()
	wrapped := Wrap(base, cfg, nil, CombinedDial(HostStdioFactory(dir, nil, nil)))
	w, ok := wrapped.(*TaskTools)
	if !ok {
		t.Fatalf("active Wrap should return *TaskTools, got %T", wrapped)
	}
	return w
}

// coverage52Adapted finds the adapted tool name ending in the given suffix
// (for example "__ping") in the wrapper's current snapshot.
func coverage52Adapted(t *testing.T, w *TaskTools, suffix string) string {
	t.Helper()
	for _, tool := range w.Schemas() {
		if strings.HasSuffix(tool.Name, suffix) {
			return tool.Name
		}
	}
	t.Fatalf("no adapted tool with suffix %q in snapshot", suffix)
	return ""
}

// coverage52Execute runs one adapted tool through the wrapper and returns
// its rendered output.
func coverage52Execute(t *testing.T, ctx context.Context, w *TaskTools, name string, args map[string]any) string {
	t.Helper()
	out, err := w.Execute(ctx, callWith(t, name, args))
	if err != nil {
		t.Fatalf("Execute %s: %v", name, err)
	}
	return out
}

func coverage52HelperGuard(t *testing.T) {
	t.Helper()
	if os.Getenv("Q_AGENT_MCP_HELPER") == "1" {
		t.Skip("helper process")
	}
}

func coverage52Statuses(t *testing.T, w *TaskTools) map[string]ServerStatus {
	t.Helper()
	statuses := w.Statuses()
	if len(statuses) == 0 {
		t.Fatal("no statuses recorded")
	}
	byName := make(map[string]ServerStatus, len(statuses))
	for _, s := range statuses {
		byName[s.Name] = s
	}
	return byName
}

// TestCoverage52BothTransportsEndToEnd proves the authorized-chat path over
// both transports at once: one Wrap task discovers the host stdio helper
// and the JSON-mode HTTP fixture, invokes a tool on each, keeps ordinary
// tools working, and exposes a sanitized summary.
func TestCoverage52BothTransportsEndToEnd(t *testing.T) {
	coverage52HelperGuard(t)
	t.Setenv("Q_AGENT_MCP_HELPER", "1")
	t.Setenv("Q_AGENT_MCP_HELPER_MODE", "server")

	server, rec := httpTestServer(t, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	dir := t.TempDir()
	base := &stubBase{schemas: baseSchemas()}
	w := coverage52Wrap(t, base, coverage52Config(t, server.URL), dir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w.StartTask(ctx, nil)
	defer w.CloseTask()

	byName := coverage52Statuses(t, w)
	for _, name := range []string{"local", "remote"} {
		status, ok := byName[name]
		if !ok || !status.Available {
			t.Fatalf("server %q unavailable: %+v", name, byName)
		}
		if status.ProtocolVersion == "" {
			t.Fatalf("server %q missing negotiated version: %+v", name, status)
		}
	}

	// Catalog consistency: both servers contribute adapted names that
	// resolve back to their owning server.
	ping := coverage52Adapted(t, w, "__ping")
	greet := coverage52Adapted(t, w, "__greet")

	if out := coverage52Execute(t, ctx, w, ping, nil); !strings.Contains(out, "pong") {
		t.Fatalf("stdio ping = %q, want pong", out)
	}
	if out := coverage52Execute(t, ctx, w, greet, map[string]any{"name": "chat"}); !strings.Contains(out, "hi chat") {
		t.Fatalf("http greet = %q, want hi chat", out)
	}
	// Ordinary tools still route to the built-in runner.
	if out, err := w.Execute(ctx, llm.ToolCall{Name: "shell"}); err != nil || out != "base:shell" {
		t.Fatalf("base fallthrough = %q, %v", out, err)
	}
	// JSON-mode requests carry protocol metadata and no credentials.
	requireMetadata(t, rec.snapshot())
	requireNoCredentials(t, rec.snapshot())
	// Operator-visible diagnostics name both servers and hide details.
	if summary := w.Summary(); !strings.Contains(summary, "2/2 servers available") {
		t.Fatalf("summary = %q", summary)
	}
	prompt := w.SystemPrompt()
	for _, want := range []string{"local", "remote"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing server %q", want)
		}
	}
	if strings.Contains(prompt, server.URL) {
		t.Fatalf("prompt leaks endpoint URL: %q", prompt)
	}
}

// TestCoverage52SSETransportEndToEnd proves the same Wrap path over the
// request-scoped SSE response mode with a stateful fixture: discover,
// invoke, no credentials, and a session DELETE on teardown.
func TestCoverage52SSETransportEndToEnd(t *testing.T) {
	coverage52HelperGuard(t)

	server, rec := httpTestServer(t, &mcp.StreamableHTTPOptions{})
	cfg := config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"remote": {Enabled: true, Transport: "http", URL: server.URL, ConnectTimeout: "5s", CallTimeout: "5s"},
	}}}
	base := &stubBase{schemas: baseSchemas()}
	w := coverage52Wrap(t, base, cfg, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	w.StartTask(ctx, nil)
	defer w.CloseTask()

	byName := coverage52Statuses(t, w)
	if status, ok := byName["remote"]; !ok || !status.Available {
		t.Fatalf("sse server unavailable: %+v", byName)
	}
	greet := coverage52Adapted(t, w, "__greet")
	if out := coverage52Execute(t, ctx, w, greet, map[string]any{"name": "sse"}); !strings.Contains(out, "hi sse") {
		t.Fatalf("sse greet = %q", out)
	}
	requireMetadata(t, rec.snapshot())
	requireNoCredentials(t, rec.snapshot())
}

// TestCoverage52PartialFailureKeepsHealthyUsable proves one task survives
// mixed failure: a healthy HTTP fixture stays invocable while a missing
// stdio binary and a refused HTTP endpoint record bounded, single-line,
// sanitized availability without disabling ordinary tools.
func TestCoverage52PartialFailureKeepsHealthyUsable(t *testing.T) {
	coverage52HelperGuard(t)

	server, _ := httpTestServer(t, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	brokenStdio := stdioHelperServer("coverage52-broken")
	brokenStdio.Command = "/nonexistent-mcp-server-binary"
	cfg := config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"remote":       {Enabled: true, Transport: "http", URL: server.URL, ConnectTimeout: "5s", CallTimeout: "5s"},
		"broken_stdio": brokenStdio,
		"broken_http":  {Enabled: true, Transport: "http", URL: "http://127.0.0.1:1/mcp", ConnectTimeout: "5s", CallTimeout: "5s"},
	}}}
	base := &stubBase{schemas: baseSchemas()}
	w := coverage52Wrap(t, base, cfg, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w.StartTask(ctx, nil)
	defer w.CloseTask()

	byName := coverage52Statuses(t, w)
	if len(byName) != 3 {
		t.Fatalf("statuses = %d, want 3", len(byName))
	}
	if status, ok := byName["remote"]; !ok || !status.Available {
		t.Fatalf("healthy server unavailable: %+v", byName)
	}
	for _, name := range []string{"broken_stdio", "broken_http"} {
		status, ok := byName[name]
		if !ok {
			t.Fatalf("missing status for %q", name)
		}
		if status.Available {
			t.Fatalf("broken server %q reported available", name)
		}
		if strings.Contains(status.Error, "\n") {
			t.Fatalf("multiline status error for %q: %q", name, status.Error)
		}
		if len(status.Error) > 512 {
			t.Fatalf("unbounded status error for %q (%d bytes)", name, len(status.Error))
		}
	}
	// The healthy server still serves through the wrapper.
	greet := coverage52Adapted(t, w, "__greet")
	if out := coverage52Execute(t, ctx, w, greet, map[string]any{"name": "partial"}); !strings.Contains(out, "hi partial") {
		t.Fatalf("healthy greet = %q", out)
	}
	// Ordinary tools are unaffected by the partial outage.
	if out, err := w.Execute(ctx, llm.ToolCall{Name: "shell"}); err != nil || out != "base:shell" {
		t.Fatalf("base fallthrough = %q, %v", out, err)
	}
	if summary := w.Summary(); !strings.Contains(summary, "1/3 servers available") {
		t.Fatalf("summary = %q", summary)
	}
}

// TestCoverage52CancelReapsChildWithoutLeaks proves cancellation-safe
// teardown: after CloseTask the stdio child is reaped, sessions are gone,
// and calls fall through to the built-in runner.
func TestCoverage52CancelReapsChildWithoutLeaks(t *testing.T) {
	coverage52HelperGuard(t)
	t.Setenv("Q_AGENT_MCP_HELPER", "1")
	t.Setenv("Q_AGENT_MCP_HELPER_MODE", "server")

	dir := t.TempDir()
	base := &stubBase{schemas: baseSchemas()}
	cfg := config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"local": stdioHelperServer("coverage52-cancel"),
	}}}
	w := coverage52Wrap(t, base, cfg, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w.StartTask(ctx, nil)

	pidName := coverage52Adapted(t, w, "__pid")
	// Executor output carries a provenance header line before the tool
	// text; the pid is the trailing line.
	pidOut := coverage52Execute(t, ctx, w, pidName, nil)
	pidText := strings.TrimSpace(pidOut)
	if i := strings.LastIndex(pidText, "\n"); i >= 0 {
		pidText = strings.TrimSpace(pidText[i+1:])
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	if !pidAlive(pid) {
		t.Fatal("child not running before close")
	}

	w.CloseTask()
	w.CloseTask() // idempotent
	deadline := time.Now().Add(15 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("child %d survived CloseTask", pid)
	}
	// No sessions survive: adapted names fall through to base, statuses
	// are gone, and the wrapper reports no active task.
	if out, err := w.Execute(ctx, llm.ToolCall{Name: pidName}); err != nil || out != "base:"+pidName {
		t.Fatalf("post-close Execute = %q, %v", out, err)
	}
	if statuses := w.Statuses(); statuses != nil {
		t.Fatalf("statuses survive close: %+v", statuses)
	}
	if starts, closes, _, _ := base.counts(); starts != 1 || closes != 2 {
		t.Fatalf("base starts=%d closes=%d, want 1/2", starts, closes)
	}
}

// TestCoverage52SequentialTasksIsolated proves tasks never share MCP state:
// after one task closes, a fresh wrapper over the same fixtures reconnects
// both transports and serves calls independently.
func TestCoverage52SequentialTasksIsolated(t *testing.T) {
	coverage52HelperGuard(t)
	t.Setenv("Q_AGENT_MCP_HELPER", "1")
	t.Setenv("Q_AGENT_MCP_HELPER_MODE", "server")

	server, _ := httpTestServer(t, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	cfg := coverage52Config(t, server.URL)
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	firstBase := &stubBase{schemas: baseSchemas()}
	first := coverage52Wrap(t, firstBase, cfg, dir)
	first.StartTask(ctx, nil)
	firstPing := ""
	for _, tool := range first.Schemas() {
		if strings.HasSuffix(tool.Name, "__ping") {
			firstPing = tool.Name
		}
	}
	if firstPing == "" {
		t.Fatal("first task discovered no ping tool")
	}
	first.CloseTask()

	secondBase := &stubBase{schemas: baseSchemas()}
	second := coverage52Wrap(t, secondBase, cfg, dir)
	second.StartTask(ctx, nil)
	defer second.CloseTask()

	byName := coverage52Statuses(t, second)
	for _, name := range []string{"local", "remote"} {
		if status, ok := byName[name]; !ok || !status.Available {
			t.Fatalf("second task server %q unavailable: %+v", name, byName)
		}
	}
	greet := coverage52Adapted(t, second, "__greet")
	if out := coverage52Execute(t, ctx, second, greet, map[string]any{"name": "second"}); !strings.Contains(out, "hi second") {
		t.Fatalf("second task greet = %q", out)
	}
	// The closed first task holds no sessions and routes to its base.
	if out, err := first.Execute(ctx, llm.ToolCall{Name: firstPing}); err != nil || out != "base:"+firstPing {
		t.Fatalf("closed first task Execute = %q, %v", out, err)
	}
}

// coverage52Provider answers immediately without tool calls: the chat
// authorizes the task, the model needs no remote tools, and the wrapper
// lifetime must still open and close deterministically.
type coverage52Provider struct{ calls int }

func (p *coverage52Provider) Name() string { return "mock" }

func (p *coverage52Provider) Complete(_ context.Context, _ llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.calls++
	return llm.Response{Message: llm.Message{Content: "done"}}, nil
}

// TestCoverage52AgentRunClosesTaskLifetime proves the agent loop drives the
// composed lifetime end to end: agent.Run starts the task (dialing an
// unreachable server records sanitized unavailability instead of failing
// the chat) and its deferred CloseTask releases everything, so the base
// runner sees exactly one start and one close with no sessions left over.
func TestCoverage52AgentRunClosesTaskLifetime(t *testing.T) {
	cfg := config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"remote": {Enabled: true, Transport: "http", URL: "http://127.0.0.1:1/mcp", ConnectTimeout: "5s", CallTimeout: "5s"},
	}}}
	base := &stubBase{schemas: baseSchemas()}
	wrapped := Wrap(base, cfg, nil, nil)
	w, ok := wrapped.(*TaskTools)
	if !ok {
		t.Fatalf("active Wrap should return *TaskTools, got %T", wrapped)
	}

	provider := &coverage52Provider{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := agent.Run(ctx, provider, wrapped, nil, "go", agent.Options{Model: "test", MaxSteps: 1})
	if err != nil {
		t.Fatalf("agent run with unavailable MCP: %v", err)
	}
	if result.Answer != "done" || provider.calls != 1 {
		t.Fatalf("result = %+v calls = %d", result, provider.calls)
	}
	if starts, closes, _, _ := base.counts(); starts != 1 || closes != 1 {
		t.Fatalf("base starts=%d closes=%d, want 1/1 (deferred CloseTask ran)", starts, closes)
	}
	if statuses := w.Statuses(); statuses != nil {
		t.Fatalf("statuses survive the run: %+v (lifetime leaked)", statuses)
	}
	if summary := w.Summary(); !strings.Contains(summary, "setup has not run") {
		t.Fatalf("summary = %q, want post-run idle state", summary)
	}
	// After the run the wrapper is inert: calls fall through to base.
	if out, err := w.Execute(ctx, llm.ToolCall{Name: "shell"}); err != nil || out != "base:shell" {
		t.Fatalf("post-run Execute = %q, %v", out, err)
	}
}
