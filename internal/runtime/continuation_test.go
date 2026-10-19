package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/agent"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

type continuationTools struct{ calls []string }

func (*continuationTools) Schemas() []llm.Tool { return []llm.Tool{{Name: "shell"}} }
func (t *continuationTools) Execute(_ context.Context, c llm.ToolCall) (string, error) {
	t.calls = append(t.calls, string(c.Arguments))
	return "CONFIRMED_RESULT: resource 42 created", nil
}

type continuationProvider struct {
	requests []llm.Request
	fn       func(llm.Request) (llm.Response, error)
}

func (*continuationProvider) Name() string { return "ollama" }
func (p *continuationProvider) Complete(_ context.Context, req llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.requests = append(p.requests, req)
	return p.fn(req)
}
func continuationRequestText(req llm.Request) string {
	var parts []string
	for _, m := range req.Messages {
		if m.Role != "system" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

func TestFailedTaskContinuesAfterRestartWithoutReplayingTools(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, scheduled := range []bool{false, true} {
			t.Run(fmt.Sprintf("memory=%t/scheduled=%t", memory, scheduled), func(t *testing.T) {
				r, _, _ := fixture(t)
				r.Config.Memory.Enabled = memory
				tools := &continuationTools{}
				r.Tools = func(string) agent.Tools { return tools }
				first := &continuationProvider{fn: func(llm.Request) (llm.Response, error) {
					return llm.Response{Message: llm.Message{Thinking: "PRIVATE_REASONING", ToolCalls: []llm.ToolCall{{Name: "shell", Arguments: json.RawMessage(`{"command":"CREATE_ONCE"}`)}}}}, nil
				}}
				r.Providers = func(state.Conversation) (llm.Provider, error) { return first, nil }
				ctx := context.Background()
				route := input(0, -10, 7, 1, "")
				must(t, r.Handle(ctx, input(1, -10, 7, 1, "/max_steps 1")))
				original := "ORIGINAL_REQUEST create a resource, then report its ID"
				if scheduled {
					_, err := r.Store.CreateSchedule(state.Schedule{Route: route, Kind: "agent", Text: original, At: time.Now().Add(-time.Second), Repeat: "once", Timezone: "Asia/Taipei"}, time.Now().Add(-time.Hour))
					must(t, err)
					must(t, r.Store.ScanDue(time.Now()))
					must(t, r.dispatchScheduled(ctx))
				} else {
					must(t, r.Handle(ctx, input(2, -10, 7, 1, original)))
				}
				r.Wait()
				if len(tools.calls) != 1 {
					t.Fatalf("tools=%v", tools.calls)
				}
				// Failure itself is durable before its reply; it is not a successful memory fact.
				reopened, err := state.Open(r.Config.StateDir, nil)
				must(t, err)
				r.Store = reopened
				saved, err := r.Store.Load(route.ConversationKey())
				must(t, err)
				if len(saved.Pairs) != 1 || !saved.Pairs[0].Interrupted || !strings.Contains(saved.Pairs[0].User, "ORIGINAL_REQUEST") || !strings.Contains(saved.Pairs[0].Progress, "CONFIRMED_RESULT") {
					t.Fatalf("failure not saved: %+v", saved)
				}
				mem, err := r.Store.LoadMemory(route.WorkspaceKey())
				must(t, err)
				if len(mem.Pending) != 0 {
					t.Fatal("failure recorded as successful long-term source")
				}
				if !strings.Contains(strings.Join(drainReplies(t, r.Store), "\n"), "Task context saved") {
					t.Fatal("failure reply did not acknowledge saved context")
				}
				next := &continuationProvider{fn: func(req llm.Request) (llm.Response, error) {
					text := continuationRequestText(req)
					for _, want := range []string{"ORIGINAL_REQUEST", "CREATE_ONCE", "CONFIRMED_RESULT", "maximum of 1 model steps", "continue"} {
						if !strings.Contains(text, want) {
							t.Errorf("followup missing %q: %s", want, text)
						}
					}
					for _, m := range req.Messages {
						if len(m.ToolCalls) > 0 || m.ToolCallID != "" || m.Thinking != "" {
							t.Error("native calls or private reasoning replayed")
						}
					}
					return llm.Response{Message: llm.Message{Content: "Resource 42 was already created; reporting its ID without creating it again."}}, nil
				}}
				r.Providers = func(state.Conversation) (llm.Provider, error) { return next, nil }
				// Raising the limit must not discard the failed task's history.
				must(t, r.Handle(ctx, input(3, -10, 7, 1, "/max_steps 32")))
				must(t, r.Handle(ctx, input(4, -10, 7, 1, "continue")))
				r.Wait()
				if len(next.requests) != 1 || len(tools.calls) != 1 {
					t.Fatal("continuation replayed completed tool action")
				}
				saved, err = r.Store.Load(route.ConversationKey())
				must(t, err)
				if len(saved.Pairs) != 2 || saved.Pairs[1].Interrupted {
					t.Fatal("continuation did not commit normal success")
				}
				// An unrelated topic in the same chat does not receive failed-task history.
				other, err := r.loadModelConversation(input(0, -10, 8, 1, ""))
				must(t, err)
				if strings.Contains(continuationRequestText(llm.Request{Messages: other.Messages()}), "ORIGINAL_REQUEST") {
					t.Fatal("failure context leaked into other topic")
				}
				must(t, r.Store.Reset(route.ConversationKey()))
				fresh, err := r.Store.Load(route.ConversationKey())
				must(t, err)
				if len(fresh.Messages()) != 0 {
					t.Fatal("/new semantics retained failed context")
				}
			})
		}
	}
}

func TestFailedTaskRetainsAppliedSteeringAndProviderError(t *testing.T) {
	r, _, p := setupSteer(t)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "original request")))
	request(t, p)
	must(t, r.Handle(ctx, input(2, -10, 7, 1, "important correction")))
	p.responses <- steerResponse{text: "superseded answer"}
	request(t, p)
	p.responses <- steerResponse{err: errors.New("provider unavailable")}
	r.Wait()
	c, err := r.Store.Load(input(0, -10, 7, 1, "").ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || !c.Pairs[0].Interrupted || strings.Join(c.Pairs[0].Steers, "|") != "important correction" || !strings.Contains(c.Pairs[0].Answer, "provider unavailable") {
		t.Fatalf("failed steering lost: %+v", c)
	}
}

func TestFailedTaskDeadlineRetainsUncertainToolOutcome(t *testing.T) {
	r, _, _ := fixture(t)
	p := &continuationProvider{fn: func(llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{Name: "shell", Arguments: json.RawMessage(`{"command":"possibly_created_resource"}`)}}}}, nil
	}}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A tool-local deadline is a failure even when the task context is still alive.
	r.Tools = func(string) agent.Tools { return deadlineContinuationTools{} }
	m := input(1, -10, 7, 1, "original deadline request")
	must(t, r.Handle(ctx, m))
	r.Wait()
	c, err := r.Store.Load(m.ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || !strings.Contains(c.Pairs[0].Progress, "outcome uncertain") || !strings.Contains(c.Pairs[0].Progress, "partial result") {
		t.Fatalf("deadline progress: %+v", c)
	}
}

type deadlineContinuationTools struct{}

func (deadlineContinuationTools) Schemas() []llm.Tool { return nil }
func (deadlineContinuationTools) Execute(context.Context, llm.ToolCall) (string, error) {
	return "partial result", context.DeadlineExceeded
}

func TestFailedTaskReportsContinuationStorageFailure(t *testing.T) {
	r, _, _ := fixture(t)
	m := input(1, -10, 7, 1, "original request")
	historyPath := filepath.Join(r.Config.StateDir, "conversations", platform.WorkspaceKey(m.ConversationKey(), 0)+".json")
	p := &continuationProvider{fn: func(llm.Request) (llm.Response, error) {
		// Obstruct only the next conversation write, after admission saved routing.
		if err := os.Rename(historyPath, historyPath+".backup"); err != nil {
			return llm.Response{}, err
		}
		if err := os.Mkdir(historyPath, 0700); err != nil {
			return llm.Response{}, err
		}
		return llm.Response{}, errors.New("provider failed")
	}}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	must(t, r.Handle(context.Background(), m))
	r.Wait()
	text := strings.Join(drainReplies(t, r.Store), "\n")
	if strings.Contains(text, "Task context saved") || !strings.Contains(text, "task context could not be saved") || !strings.Contains(text, "provider failed") {
		t.Fatal(text)
	}
}

func TestFailedTaskTaskTimeoutPreservesOriginalRequest(t *testing.T) {
	r, _, _ := fixture(t)
	r.Config.Limits.TaskTimeout = "20ms"
	p := &testProvider{block: true}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	m := input(1, -10, 7, 1, "original request before task timeout")
	must(t, r.Handle(context.Background(), m))
	r.Wait()
	c, err := r.Store.Load(m.ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || !c.Pairs[0].Interrupted || c.Pairs[0].User != m.Text {
		t.Fatalf("timeout context: %+v", c)
	}
	text := strings.Join(drainReplies(t, r.Store), "\n")
	if !strings.Contains(text, "Task reached its deadline") || !strings.Contains(text, "Task context saved") {
		t.Fatal(text)
	}
}
