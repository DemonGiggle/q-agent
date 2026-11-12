package runtime

import (
	"context"
	"fmt"
	"q-agent/internal/agent"
	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/mcpclient"
	"q-agent/internal/platform"
	"q-agent/internal/state"
	"strings"
	"sync"
	"testing"
	"time"
)

type testWorkspace struct {
	mu      sync.Mutex
	calls   int
	fail    bool
	block   chan struct{}
	started chan struct{}
}

func (w *testWorkspace) Ensure(ctx context.Context, m platform.Message) (string, error) {
	w.mu.Lock()
	w.calls++
	fail := w.fail
	w.mu.Unlock()
	if w.started != nil {
		w.started <- struct{}{}
	}
	if w.block != nil {
		select {
		case <-w.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail {
		return "", fmt.Errorf("provisioning failed")
	}
	return m.WorkspaceKey(), nil
}

type testProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	block    bool
	started  chan struct{}
}

func (p *testProvider) Name() string { return "ollama" }
func (p *testProvider) Complete(ctx context.Context, r llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	p.mu.Unlock()
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.block {
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}
	return llm.Response{Message: llm.Message{Content: "answer"}}, nil
}

type noTools struct{}

func (noTools) Schemas() []llm.Tool { return nil }
func (noTools) Execute(context.Context, llm.ToolCall) (string, error) {
	return "", fmt.Errorf("unexpected tool")
}
func fixture(t *testing.T) (*Runtime, *testWorkspace, *testProvider) {
	t.Helper()
	c := config.Defaults()
	c.Telegram.AllowedUsers = []int64{1}
	c.Telegram.AllowedGroups = []int64{-10, -20, -30}
	c.StateDir = t.TempDir()
	s, err := state.Open(c.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &testWorkspace{}
	p := &testProvider{}
	r := &Runtime{Config: c, Store: s, Workspaces: w, Providers: func(state.Conversation) (llm.Provider, error) { return p, nil }, Tools: func(string) agent.Tools { return noTools{} }, Options: agent.Options{Model: "test"}}
	t.Cleanup(r.Close)
	return r, w, p
}
func input(id, chat, topic, user int64, text string) platform.Message {
	return platform.Message{Platform: "telegram", UpdateID: id, ChatID: chat, TopicID: topic, UserID: user, Text: text}
}

// nonMemoryMessages strips long-term memory injections (consolidated notes and
// unconsolidated same-day journal) so working-history assertions stay focused
// on prompt history rather than the memory feature's prompt shape.
func nonMemoryMessages(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range msgs {
		if m.Role == "system" && (strings.Contains(m.Content, "Long-term memory") || strings.Contains(m.Content, "Unconsolidated notes")) {
			continue
		}
		out = append(out, m)
	}
	return out
}
func TestAuthorizationAndDuplicates(t *testing.T) {
	r, w, _ := fixture(t)
	// Private spoof, unlisted private, unlisted group, and private spoof command.
	for _, m := range []platform.Message{input(1, 1, 0, 2, "go"), input(2, 99, 0, 1, "go"), input(3, -99, 0, 1, "go"), input(4, 1, 0, 2, "/cancel")} {
		if err := r.Handle(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	r.Wait()
	if w.calls != 0 {
		t.Fatal("unauthorized execution")
	}
	// Group allows any user without per-user listing; private allows its owner.
	for _, m := range []platform.Message{input(5, -10, 0, 2, "go"), input(6, 1, 0, 1, "go")} {
		if err := r.Handle(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		r.Wait()
	}
	if w.calls != 2 {
		t.Fatalf("authorized private/group execution = %d", w.calls)
	}
	m := input(5, -10, 0, 2, "go")
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if w.calls != 2 {
		t.Fatal("duplicate executed")
	}
}
func TestTopicHistoryNewAndRestart(t *testing.T) {
	r, w, p := fixture(t)
	for i, topic := range []int64{7, 8, 7} {
		if err := r.Handle(context.Background(), input(int64(i+1), -10, topic, 1, "go")); err != nil {
			t.Fatal(err)
		}
		r.Wait()
	}
	if len(nonMemoryMessages(p.requests[1].Messages)) != 2 || len(nonMemoryMessages(p.requests[2].Messages)) != 4 {
		t.Fatal("topic history mixed")
	}
	m := input(4, -10, 7, 1, "/new")
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	c, _ := r.Store.Load(m.ConversationKey())
	if len(c.Pairs) != 0 || w.calls != 3 {
		t.Fatal("new modified workspace or did not clear history")
	}
}
func TestInterruptedUpdateIsNotReplayedAfterRestart(t *testing.T) {
	r, w, _ := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 1)
	m := input(50, -10, 0, 1, "go")
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	<-w.started
	r.Close()
	store, err := state.Open(r.Config.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Runtime{Config: r.Config, Store: store, Workspaces: w, Providers: r.Providers, Tools: r.Tools, Options: r.Options}
	defer restarted.Close()
	if err := restarted.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if w.calls != 1 {
		t.Fatal("interrupted task replayed")
	}
}
func TestBusyIncludesProvisioningAndOtherTopics(t *testing.T) {
	r, w, _ := fixture(t)
	w.block = make(chan struct{})
	w.started = make(chan struct{}, 4)
	if err := r.Handle(context.Background(), input(1, -10, 7, 1, "go")); err != nil {
		t.Fatal(err)
	}
	<-w.started
	if err := r.Handle(context.Background(), input(2, -10, 8, 1, "go")); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(context.Background(), input(3, -20, 0, 1, "go")); err != nil {
		t.Fatal(err)
	}
	<-w.started
	if err := r.Handle(context.Background(), input(4, -30, 0, 1, "go")); err != nil {
		t.Fatal(err)
	}
	reply, ok := r.Store.Pending()
	if !ok || reply.Route.UpdateID != 2 || reply.Parts[0] != queuedAck {
		t.Fatalf("%+v", reply)
	}
	w.mu.Lock()
	calls := w.calls
	w.mu.Unlock()
	if calls != 2 {
		t.Fatal("concurrency exceeded")
	}
	close(w.block)
	r.Wait()
	w.mu.Lock()
	calls = w.calls
	w.mu.Unlock()
	if calls != 4 {
		t.Fatalf("queued tasks did not run: calls=%d", calls)
	}
}
func TestCancelAndProvisioningFailure(t *testing.T) {
	r, w, p := fixture(t)
	p.block = true
	p.started = make(chan struct{}, 1)
	if err := r.Handle(context.Background(), input(1, -10, 7, 1, "go")); err != nil {
		t.Fatal(err)
	}
	<-p.started
	if err := r.Handle(context.Background(), input(2, -10, 7, 1, "/cancel")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop task")
	}
	c, _ := r.Store.Load(input(0, -10, 7, 1, "").ConversationKey())
	if len(c.Pairs) != 0 {
		t.Fatal("interrupted run saved a pair")
	}
	w.fail = true
	p.block = false
	if err := r.Handle(context.Background(), input(3, -20, 0, 1, "go")); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if len(p.requests) != 1 {
		t.Fatal("provisioning failure reached model")
	}
}

// TestMCPComposesWithScheduleToolsForAllTasks (#52) proves the runtime's
// task composition shape: unknown runners yield no stdio factory (no
// cross-mode fallback), inactive MCP is a passthrough for both interactive
// and scheduled runs (both flow through run()), and an active wrapper
// keeps the owned schedule_* names while its lifecycle propagates through
// scheduleTools even when every MCP server is unavailable.
func TestMCPComposesWithScheduleToolsForAllTasks(t *testing.T) {
	r, _, _ := fixture(t)
	if mcpclient.Active(r.Config) {
		t.Fatal("default config must leave MCP inactive")
	}
	if f := mcpStdioDial(noTools{}, nil); f != nil {
		t.Fatal("unknown runner must not yield a stdio factory")
	}
	// Inactive MCP returns the built-in tools unchanged: no sessions, no
	// setup, for scheduled runs exactly as for interactive ones.
	base := noTools{}
	if got := mcpclient.Wrap(base, r.Config, nil, nil, scheduleCreateName, scheduleListName, scheduleDeleteName); got != agent.Tools(base) {
		t.Fatalf("inactive Wrap should return base unchanged, got %T", got)
	}

	// Active MCP with no usable transport: the task records one
	// unavailable server and ordinary + schedule tools keep working.
	cfg := r.Config
	cfg.MCP = config.MCP{Servers: map[string]config.MCPServer{
		"docs": {Enabled: true, Transport: "http", URL: "https://example.com/mcp", ConnectTimeout: "1s", CallTimeout: "1s"},
	}}
	wrapped := mcpclient.Wrap(base, cfg, nil, nil, scheduleCreateName, scheduleListName, scheduleDeleteName)
	mcpTools, ok := wrapped.(interface {
		Statuses() []mcpclient.ServerStatus
		Summary() string
	})
	if !ok {
		t.Fatalf("active Wrap should expose MCP diagnostics, got %T", wrapped)
	}
	owner := input(1, -10, 0, 1, "go")
	tools := &scheduleTools{base: wrapped, runtime: r, owner: owner}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	tools.StartTask(cancelled, nil) // must not hang, panic, or dial out
	defer tools.CloseTask()
	if statuses := mcpTools.Statuses(); len(statuses) != 1 || statuses[0].Available {
		t.Fatalf("statuses = %+v, want one unavailable server", statuses)
	}
	// No adapted name may shadow the owned schedule tools.
	seen := map[string]bool{}
	for _, schema := range tools.Schemas() {
		seen[schema.Name] = true
		if strings.Contains(schema.Name, "__") {
			t.Fatalf("unexpected adapted tool while servers are down: %q", schema.Name)
		}
	}
	for _, name := range []string{scheduleCreateName, scheduleListName, scheduleDeleteName} {
		if !seen[name] {
			t.Fatalf("schedule tool %q missing from composed schemas", name)
		}
	}
	// Schedule tools stay usable while MCP is down: listing returns an
	// (empty) page rather than an MCP error.
	out, err := tools.Execute(context.Background(), llm.ToolCall{Name: scheduleListName, Arguments: []byte(`{"offset":0}`)})
	if err != nil || !strings.Contains(out, `"total":0`) {
		t.Fatalf("schedule_list during MCP outage = %q, %v", out, err)
	}
	if summary := mcpTools.Summary(); !strings.Contains(summary, "0/1 servers available") {
		t.Fatalf("summary = %q", summary)
	}
}
