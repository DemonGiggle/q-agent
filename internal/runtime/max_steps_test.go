package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/state"
)

func TestMaxStepsPersistenceIsolationAndReset(t *testing.T) {
	r, _, _ := pickerFixture(t)
	ctx := context.Background()
	id := int64(0)
	send := func(text string) {
		t.Helper()
		id++
		must(t, r.Handle(ctx, input(id, 10, 7, 1, text)))
		r.Wait()
	}
	send("first task")
	send("/max_steps 20")
	m := input(0, 10, 8, 1, "")
	selected, err := r.chatModel(m)
	must(t, err)
	if selected.MaxSteps != 20 {
		t.Fatalf("other topic limit: %d", selected.MaxSteps)
	}
	if !strings.Contains(r.statusLocked(m), "Model steps: 20") {
		t.Fatal("status ignored override")
	}
	c, err := r.loadModelConversation(input(0, 10, 7, 1, ""))
	must(t, err)
	if len(c.Pairs) != 1 {
		t.Fatal("step change cleared history")
	}
	other, err := r.chatModel(input(0, 20, 0, 1, ""))
	must(t, err)
	if other.MaxSteps != r.Config.Limits.MaxSteps {
		t.Fatal("override leaked across chats")
	}
	r.Store, err = state.Open(r.Config.StateDir, nil)
	must(t, err)
	send("/new")
	selected, err = r.chatModel(m)
	must(t, err)
	if selected.MaxSteps != 20 {
		t.Fatal("restart or /new lost limit")
	}
	// Other setting writes (including the picker) preserve the new field.
	send("/think high")
	token, _ := openPicker(t, r, id+1, 10, 7)
	id++
	callback(t, r, id+1, 10, 7, 1, "models:"+token+":s:0")
	id++
	selected, err = r.chatModel(m)
	must(t, err)
	if selected.MaxSteps != 20 {
		t.Fatal("model/thinking change lost limit")
	}
	send("/max_steps default")
	r.Config.Limits.MaxSteps = 9
	selected, err = r.chatModel(m)
	must(t, err)
	if selected.MaxSteps != 9 {
		t.Fatal("default did not follow global setting")
	}
	send("/max_steps 30")
	send("/settings reset")
	raw, err := r.Store.ChatModel(m.WorkspaceKey())
	must(t, err)
	if raw.MaxSteps != 0 {
		t.Fatal("settings reset retained override")
	}
	// Bare command reads settings, never invokes the provider.
	send("/max_steps")
	found := false
	for reply, ok := r.Store.Pending(); ok; reply, ok = r.Store.Pending() {
		if strings.Contains(strings.Join(reply.Parts, "\n"), "Maximum model steps: 9") {
			found = true
		}
		must(t, r.Store.Delivered(reply.ID))
	}
	if !found {
		t.Fatal("bare command missing effective limit")
	}
}

func TestMaxStepsRejectInvalidBusyAndUnauthorized(t *testing.T) {
	r, _, p := pickerFixture(t)
	ctx := context.Background()
	m := input(1, 10, 0, 1, "/max_steps 5")
	must(t, r.Handle(ctx, m))
	for _, text := range []string{"/max_steps 0", "/max_steps -1", "/max_steps 1.5", "/max_steps nope", "/max_steps 999999999999999999999999999", "/max_steps 5 extra"} {
		m.UpdateID++
		m.Text = text
		must(t, r.Handle(ctx, m))
	}
	m.UpdateID++
	m.Text = "/max_steps 10"
	r.active = map[string]task{m.WorkspaceKey(): {topic: 7}}
	must(t, r.Handle(ctx, m))
	delete(r.active, m.WorkspaceKey())
	m.UpdateID++
	m.UserID = 999
	must(t, r.Handle(ctx, m))
	raw, err := r.Store.ChatModel(m.WorkspaceKey())
	must(t, err)
	if raw.MaxSteps != 5 {
		t.Fatalf("invalid change saved: %+v", raw)
	}
	if len(p.requests) != 0 {
		t.Fatal("settings command invoked model")
	}
}

// Always ask for another tool execution, so the runtime must enforce the
// configured model-step budget rather than merely displaying/persisting it.
type stepLoopProvider struct{ calls int }

func (*stepLoopProvider) Name() string { return "ollama" }
func (p *stepLoopProvider) Complete(context.Context, llm.Request, llm.StreamCallback) (llm.Response, error) {
	p.calls++
	return llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{ID: fmt.Sprint(p.calls), Name: "probe", Arguments: json.RawMessage(fmt.Sprintf(`{"step":%d}`, p.calls))}}}}, nil
}

func TestMaxStepsEnforcedForInteractiveAndScheduledTasks(t *testing.T) {
	for _, tc := range []struct {
		scheduled bool
		steps     int
	}{{false, 2}, {false, 12}, {true, 2}, {true, 12}} {
		t.Run(fmt.Sprintf("scheduled=%t/steps=%d", tc.scheduled, tc.steps), func(t *testing.T) {
			r, _, _ := fixture(t)
			p := &stepLoopProvider{}
			r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
			must(t, r.Handle(context.Background(), input(1, 10, 7, 1, fmt.Sprintf("/max_steps %d", tc.steps))))
			if tc.scheduled {
				enqueue(t, r, input(0, 10, 8, 1, ""), "agent", time.Now())
				must(t, r.dispatchScheduled(context.Background()))
			} else {
				must(t, r.Handle(context.Background(), input(2, 10, 8, 1, "work")))
			}
			r.Wait()
			if p.calls != tc.steps {
				t.Fatalf("model calls=%d, want %d", p.calls, tc.steps)
			}
			found := false
			for reply, ok := r.Store.Pending(); ok; reply, ok = r.Store.Pending() {
				if strings.Contains(strings.Join(reply.Parts, "\n"), fmt.Sprintf("maximum of %d model steps", tc.steps)) {
					found = true
				}
				must(t, r.Store.Delivered(reply.ID))
			}
			if !found {
				t.Fatal("missing step-limit failure")
			}
		})
	}
}
