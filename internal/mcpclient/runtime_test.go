package mcpclient

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/agent"
	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
)

// stubBase is a fake built-in runner: it records lifecycle and hook calls
// so tests prove the wrapper forwards everything through.
type stubBase struct {
	mu         sync.Mutex
	starts     int
	closes     int
	hooks      int
	shellHooks int
	exec       func(ctx context.Context, call llm.ToolCall) (string, error)
	prompt     string
	taskErr    error
	schemas    []llm.Tool
	hookFn     func(ctx context.Context, command string, lastLines []string) bool
	maxExtends int
}

func (s *stubBase) Schemas() []llm.Tool { return append([]llm.Tool(nil), s.schemas...) }

func (s *stubBase) SystemPrompt() string {
	if s.prompt != "" {
		return s.prompt
	}
	return "stub base prompt"
}

func (s *stubBase) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if s.exec != nil {
		return s.exec(ctx, call)
	}
	return "base:" + call.Name, nil
}

func (s *stubBase) StartTask(_ context.Context, _ *redaction.Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
}

func (s *stubBase) CloseTask() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
}

func (s *stubBase) TaskError() error { return s.taskErr }

func (s *stubBase) SetProcessHooks(_ process.Hooks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks++
}

func (s *stubBase) SetShellTimeoutHook(fn func(context.Context, string, []string) bool, maxExtends int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shellHooks++
	s.hookFn = fn
	s.maxExtends = maxExtends
}

func (s *stubBase) counts() (starts, closes, hooks, shellHooks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.closes, s.hooks, s.shellHooks
}

// plainBase lacks every optional interface: it proves the wrapper works
// with runners that only satisfy agent.Tools.
type plainBase struct {
	schemas []llm.Tool
}

func (p *plainBase) Schemas() []llm.Tool { return append([]llm.Tool(nil), p.schemas...) }
func (p *plainBase) Execute(_ context.Context, call llm.ToolCall) (string, error) {
	return "plain:" + call.Name, nil
}

func activeConfig() config.Config {
	return config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"docs": {Enabled: true, Transport: "http", URL: "https://example.com/mcp"},
	}}}
}

func baseSchemas() []llm.Tool {
	return []llm.Tool{{Name: "shell", Description: "local shell", Parameters: map[string]any{"type": "object"}}}
}

func TestWrapInactiveReturnsBase(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	got := Wrap(base, config.Config{}, nil, nil, "schedule_create")
	if got != agent.Tools(base) {
		t.Fatalf("inactive Wrap should return base unchanged")
	}
	// Disabled servers are inactive too.
	cfg := config.Config{MCP: config.MCP{Servers: map[string]config.MCPServer{
		"off": {Enabled: false, Transport: "http", URL: "https://example.com/mcp"},
	}}}
	if got := Wrap(base, cfg, nil, nil); got != agent.Tools(base) {
		t.Fatalf("disabled-servers Wrap should return base unchanged")
	}
}

func TestWrapNilBase(t *testing.T) {
	if Wrap(nil, activeConfig(), nil, nil) != nil {
		t.Fatalf("nil base should wrap to nil")
	}
}

func TestCombinedDialRoutesAndNeverFallsBack(t *testing.T) {
	dial := CombinedDial(nil)
	httpServer := config.MCPServer{Enabled: true, Transport: "http", URL: "http://127.0.0.1:9/mcp"}
	if _, err := dial(context.Background(), "h", httpServer); err != nil {
		t.Fatalf("http dial should construct: %v", err)
	}
	stdioServer := config.MCPServer{Enabled: true, Transport: "stdio", Command: "/bin/true"}
	if _, err := dial(context.Background(), "s", stdioServer); err == nil {
		t.Fatalf("nil stdio factory must mark stdio unavailable, not fall back")
	}
	if _, err := dial(context.Background(), "s", config.MCPServer{Transport: "bogus"}); err == nil {
		t.Fatalf("unknown transport must fail")
	}
}

func TestTaskToolsUnavailableServersKeepBaseUsable(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	wrapped := Wrap(base, activeConfig(), nil, nil, "schedule_create", "schedule_list", "schedule_delete")
	w, ok := wrapped.(*TaskTools)
	if !ok {
		t.Fatalf("active Wrap should return *TaskTools, got %T", wrapped)
	}
	ctx := context.Background()
	w.StartTask(ctx, nil)
	defer w.CloseTask()

	// No transport can succeed (nil stdio, unreachable HTTP): schemas stay
	// exactly the built-in set, and ordinary tools still execute.
	if got := len(w.Schemas()); got != len(baseSchemas()) {
		t.Fatalf("schemas = %d tools, want %d (base only)", got, len(baseSchemas()))
	}
	out, err := w.Execute(ctx, llm.ToolCall{Name: "shell"})
	if err != nil {
		t.Fatalf("base Execute: %v", err)
	}
	if out != "base:shell" {
		t.Fatalf("Execute = %q, want base fallthrough", out)
	}
	// Unknown MCP names fall through to base too (stale snapshots never
	// remap to a different server or tool).
	out, err = w.Execute(ctx, llm.ToolCall{Name: "docs__lookup"})
	if err != nil {
		t.Fatalf("unknown Execute: %v", err)
	}
	if out != "base:docs__lookup" {
		t.Fatalf("Execute = %q, want base fallthrough", out)
	}
	statuses := w.Statuses()
	if len(statuses) != 1 || statuses[0].Available {
		t.Fatalf("statuses = %+v, want one unavailable server", statuses)
	}
	if statuses[0].Name != "docs" || statuses[0].Transport != "http" {
		t.Fatalf("status identity = %+v", statuses[0])
	}
	if summary := w.Summary(); !strings.Contains(summary, "0/1 servers available") {
		t.Fatalf("summary = %q", summary)
	}
	if starts, closes, _, _ := base.counts(); starts != 1 || closes != 0 {
		t.Fatalf("base starts=%d closes=%d, want 1/0 before CloseTask", starts, closes)
	}
}

func TestTaskToolsForwardsLifecycleAndHooks(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	ctx := context.Background()
	w.StartTask(ctx, nil)
	w.SetProcessHooks(process.Hooks{})
	hookCalled := false
	w.SetShellTimeoutHook(func(context.Context, string, []string) bool { hookCalled = true; return true }, 3)
	_ = hookCalled
	if _, _, hooks, shell := base.counts(); hooks != 1 || shell != 1 {
		t.Fatalf("hooks=%d shell=%d, want 1/1 forwarded", hooks, shell)
	}
	base.mu.Lock()
	maxExtends := base.maxExtends
	base.mu.Unlock()
	if maxExtends != 3 {
		t.Fatalf("maxExtends = %d, want 3", maxExtends)
	}
	sentinel := errors.New("sandbox boundary failed")
	base.taskErr = sentinel
	if err := w.TaskError(); !errors.Is(err, sentinel) {
		t.Fatalf("TaskError = %v, want fatal base error", err)
	}
	w.CloseTask()
	w.CloseTask() // idempotent
	if starts, closes, _, _ := base.counts(); starts != 1 || closes != 2 {
		t.Fatalf("starts=%d closes=%d, want 1/2 (repeat StartTask-free, double close)", starts, closes)
	}
	// After CloseTask the wrapper holds no sessions and routes to base.
	if w.active() {
		t.Fatalf("wrapper still active after CloseTask")
	}
	if out, err := w.Execute(ctx, llm.ToolCall{Name: "shell"}); err != nil || out != "base:shell" {
		t.Fatalf("post-close Execute = %q, %v", out, err)
	}
}

func TestTaskToolsPlainBaseWithoutOptionals(t *testing.T) {
	base := &plainBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	ctx := context.Background()
	w.StartTask(ctx, nil) // must not panic on missing StartTask/CloseTask
	defer w.CloseTask()
	if err := w.TaskError(); err != nil {
		t.Fatalf("TaskError = %v, want nil without base hook", err)
	}
	w.SetProcessHooks(process.Hooks{})
	w.SetShellTimeoutHook(func(context.Context, string, []string) bool { return false }, 1)
	out, err := w.Execute(ctx, llm.ToolCall{Name: "shell"})
	if err != nil || out != "plain:shell" {
		t.Fatalf("Execute = %q, %v", out, err)
	}
}

func TestTaskToolsSystemPromptDistinguishesRemote(t *testing.T) {
	base := &stubBase{schemas: baseSchemas(), prompt: "local host prompt"}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	prompt := w.SystemPrompt()
	if !strings.Contains(prompt, "local host prompt") {
		t.Fatalf("prompt lost base text: %q", prompt)
	}
	for _, want := range []string{"Remote MCP tools", "server__tool", "local tools", "untrusted"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
	w.StartTask(context.Background(), nil)
	defer w.CloseTask()
	prompt = w.SystemPrompt()
	if !strings.Contains(prompt, "docs") || !strings.Contains(prompt, "Unavailable MCP servers") {
		t.Fatalf("prompt missing availability: %q", prompt)
	}
	// Sanitized: commands, URLs, and environments never appear.
	for _, leak := range []string{"example.com", "secret", "/bin/"} {
		if strings.Contains(prompt, leak) {
			t.Fatalf("prompt leaks %q: %q", leak, prompt)
		}
	}
}

func TestTaskToolsIsolationBetweenTasks(t *testing.T) {
	baseA := &stubBase{schemas: baseSchemas()}
	baseB := &stubBase{schemas: baseSchemas()}
	cfg := activeConfig()
	a := Wrap(baseA, cfg, nil, nil).(*TaskTools)
	b := Wrap(baseB, cfg, nil, nil).(*TaskTools)
	ctx := context.Background()
	a.StartTask(ctx, nil)
	b.StartTask(ctx, nil)
	a.CloseTask()
	if !b.active() {
		t.Fatalf("closing task A released task B state")
	}
	if starts, closes, _, _ := baseA.counts(); starts != 1 || closes != 1 {
		t.Fatalf("A starts=%d closes=%d, want 1/1", starts, closes)
	}
	if starts, closes, _, _ := baseB.counts(); starts != 1 || closes != 0 {
		t.Fatalf("B starts=%d closes=%d, want 1/0", starts, closes)
	}
	b.CloseTask()
}

func TestTaskToolsCancelledSetupStillCloses(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.StartTask(ctx, nil) // must record sanitized failure, not hang or panic
	defer w.CloseTask()
	if got := len(w.Statuses()); got != 1 {
		t.Fatalf("statuses = %d, want 1 cancelled record", got)
	}
	if summary := w.Summary(); !strings.Contains(summary, "0/1 servers available") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestTaskToolsDispatchesDiscoveredTools(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	// Hand-assemble one discovered snapshot (bypassing network setup) to
	// prove routing, provenance, and exactly-once dispatch.
	catalog := fixtureCatalog(t, "docs", plainTool("lookup"))
	adapted := adaptedName(t, catalog, "lookup")
	caller := &fakeCaller{fn: func(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		if params.Name != "lookup" {
			t.Errorf("server tool name = %q, want lookup", params.Name)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "remote answer"}}}, nil
	}}
	w.mu.Lock()
	w.catalog = catalog
	w.sessions = map[string]Caller{"docs": caller}
	w.task = &Task{}
	w.started = true
	w.mu.Unlock()

	out, err := w.Execute(context.Background(), callWith(t, adapted, map[string]any{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "remote answer") || !strings.Contains(out, `server="docs"`) {
		t.Fatalf("Execute = %q, want normalized text with provenance", out)
	}
	if n := caller.count(); n != 1 {
		t.Fatalf("calls = %d, want exactly one", n)
	}
	// Built-ins still fall through beside MCP tools.
	if out, err := w.Execute(context.Background(), llm.ToolCall{Name: "shell"}); err != nil || out != "base:shell" {
		t.Fatalf("base Execute = %q, %v", out, err)
	}
	// Schemas merge both sets.
	names := map[string]bool{}
	for _, tool := range w.Schemas() {
		names[tool.Name] = true
	}
	if !names["shell"] || !names[adapted] {
		t.Fatalf("schemas missing base or MCP tool: %v", names)
	}
}

func TestTaskToolsReservedKeepsScheduleNames(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil, "schedule_create", "schedule_list", "schedule_delete").(*TaskTools)
	for _, name := range []string{"schedule_create", "schedule_list", "schedule_delete", "shell"} {
		if !w.reserved[name] {
			t.Fatalf("reserved missing %q", name)
		}
	}
}

func TestTaskToolsSummaryBeforeStart(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	if s := w.Summary(); !strings.Contains(s, "setup has not run") {
		t.Fatalf("summary = %q", s)
	}
	if w.Statuses() != nil || w.Quarantined() != nil {
		t.Fatalf("pre-start diagnostics should be nil")
	}
}

func TestTaskToolsConcurrentUse(t *testing.T) {
	base := &stubBase{schemas: baseSchemas()}
	w := Wrap(base, activeConfig(), nil, nil).(*TaskTools)
	w.StartTask(context.Background(), nil)
	defer w.CloseTask()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Schemas()
			_ = w.SystemPrompt()
			_ = w.Summary()
			_, _ = w.Execute(context.Background(), llm.ToolCall{Name: "shell"})
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("concurrent use hung")
	}
}

func TestWrapScheduleNamesConstant(t *testing.T) {
	// The runtime passes these three names as extraReserved; keep this
	// pinned so a rename on either side is caught here, not in prod.
	for _, name := range []string{"schedule_create", "schedule_list", "schedule_delete"} {
		if !ReservedSet(name)[name] {
			t.Fatalf("reserved set broken for %q", name)
		}
	}
}
