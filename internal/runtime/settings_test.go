package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/state"
)

func TestChatSettingsRoutingPersistenceAndDefaults(t *testing.T) {
	r, _, p := pickerFixture(t)
	r.Config.Provider.Thinking = "low"
	r.Config.Telegram.ApprovedChats = append(r.Config.Telegram.ApprovedChats, -100)
	r.Config.Providers = map[string]config.Provider{"openai": {Name: "openai", Model: "remote", Thinking: "medium"}}
	var routes []state.Conversation
	r.Providers = func(c state.Conversation) (llm.Provider, error) { routes = append(routes, c); return p, nil }
	id := int64(0)
	send := func(chat, topic int64, text string) {
		t.Helper()
		id++
		must(t, r.Handle(context.Background(), input(id, chat, topic, 1, text)))
		r.Wait()
	}
	send(-100, 7, "/provider openai")
	send(-100, 7, "/model custom")
	send(-100, 7, "/think high")
	send(-100, 8, "first task")
	if len(p.requests) != 1 || p.requests[0].Model != "custom" || p.requests[0].Thinking != "high" || routes[len(routes)-1].Provider != "openai" {
		t.Fatalf("routing: %+v %+v", p.requests, routes)
	}
	group := input(0, -100, 8, 1, "")
	before, err := r.loadModelConversation(group)
	must(t, err)
	if len(before.Pairs) != 1 {
		t.Fatal("history missing")
	}
	send(-100, 7, "/think auto")
	after, err := r.loadModelConversation(group)
	must(t, err)
	if after.Thinking != "" || len(after.Pairs) != 1 {
		t.Fatalf("thinking changed history: %+v", after)
	}
	send(-100, 7, "/think default")
	after, err = r.loadModelConversation(group)
	must(t, err)
	if after.Thinking != "medium" {
		t.Fatalf("profile default: %+v", after)
	}
	send(10, 0, "other chat")
	if p.requests[1].Model != r.Config.Provider.Model || p.requests[1].Thinking != "low" {
		t.Fatal("group settings leaked into another chat")
	}
	// A store reopen simulates restart, without changing existing conversation files.
	reopened, err := state.Open(r.Config.StateDir, nil)
	must(t, err)
	r.Store = reopened
	selected, err := r.chatModel(group)
	must(t, err)
	if selected.Provider != "openai" || selected.Model != "custom" || *selected.Thinking != "medium" {
		t.Fatalf("restart: %+v", selected)
	}
	send(-100, 7, "/new")
	selected, err = r.chatModel(group)
	must(t, err)
	if selected.Model != "custom" {
		t.Fatal("/new cleared preferences")
	}
	// Explicit provider choice remains pinned when the global provider changes.
	r.Config.Provider.Name = "opencode-go"
	r.Config.Provider.Model = "new-default"
	selected, err = r.chatModel(group)
	must(t, err)
	if selected.Provider != "openai" {
		t.Fatal("global provider changed explicit preference")
	}
	enqueue(t, r, input(0, -100, 9, 1, ""), "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if p.requests[2].Model != "custom" || p.requests[2].Thinking != "medium" || routes[len(routes)-1].Provider != "openai" {
		t.Fatal("scheduled task ignored chat settings")
	}
	send(-100, 7, "/settings reset")
	selected, err = r.chatModel(group)
	must(t, err)
	if selected.Provider != "opencode-go" || selected.Model != "new-default" || *selected.Thinking != "low" {
		t.Fatalf("reset: %+v", selected)
	}
}

func TestChatSettingsRejectBusyUnauthorizedAndUnknownProvider(t *testing.T) {
	r, _, _ := pickerFixture(t)
	m := input(1, 10, 0, 1, "/provider unknown")
	must(t, r.Handle(context.Background(), m))
	raw, err := r.Store.ChatModel(m.WorkspaceKey())
	must(t, err)
	if raw.ProviderPinned {
		t.Fatal("unknown provider saved")
	}
	r.active = map[string]task{m.WorkspaceKey(): {topic: 9}}
	m.UpdateID++
	m.Text = "/think high"
	must(t, r.Handle(context.Background(), m))
	raw, err = r.Store.ChatModel(m.WorkspaceKey())
	must(t, err)
	if raw.Thinking != nil {
		t.Fatal("busy settings changed")
	}
	delete(r.active, m.WorkspaceKey())
	m.UpdateID++
	m.UserID = 999
	must(t, r.Handle(context.Background(), m))
	raw, err = r.Store.ChatModel(m.WorkspaceKey())
	must(t, err)
	if raw.Thinking != nil {
		t.Fatal("unauthorized settings changed")
	}
}

func TestChatSettingsInvalidateMenusAndResetHistory(t *testing.T) {
	r, _, _ := pickerFixture(t)
	r.Config.Providers = map[string]config.Provider{"openai": {Name: "openai", Model: "remote"}}
	token, _ := openPicker(t, r, 1, 10, 0)
	must(t, r.Handle(context.Background(), input(2, 10, 0, 1, "first")))
	r.Wait()
	m := input(3, 10, 0, 1, "/provider openai")
	must(t, r.Handle(context.Background(), m))
	if _, ok := r.modelMenus[token]; ok {
		t.Fatal("stale provider menu retained")
	}
	c, err := r.loadModelConversation(m)
	must(t, err)
	if len(c.Pairs) != 0 || c.Provider != "openai" {
		t.Fatalf("provider history: %+v", c)
	}
	must(t, r.Handle(context.Background(), input(4, 10, 0, 1, "/provider default")))
	c, err = r.loadModelConversation(m)
	must(t, err)
	if len(c.Pairs) != 0 {
		t.Fatal("A -> B -> A restored stale history")
	}
	// Menus for nondefault providers use that provider and preserve thinking.
	must(t, r.Handle(context.Background(), input(5, 10, 0, 1, "/provider openai")))
	must(t, r.Handle(context.Background(), input(6, 10, 0, 1, "/think high")))
	token, reply := openPicker(t, r, 7, 10, 0)
	if !strings.Contains(reply.Parts[0], "openai") {
		t.Fatal("wrong provider menu")
	}
	callback(t, r, 8, 10, 0, 1, "models:"+token+":s:0")
	selected, err := r.chatModel(m)
	must(t, err)
	if selected.Provider != "openai" || !selected.ProviderPinned || *selected.Thinking != "high" {
		t.Fatalf("picker lost preferences: %+v", selected)
	}
}

type thinkingPickerProvider struct{ *pickerProvider }

func (*thinkingPickerProvider) ThinkingCapability(string) llm.ThinkingCapability {
	return llm.ThinkingCapability{Supported: true, Adjustable: true, Levels: []string{"low", "high"}}
}

func TestThinkingValidationAndRepeatedSettingsRetainHistory(t *testing.T) {
	r, _, p := pickerFixture(t)
	r.Providers = func(state.Conversation) (llm.Provider, error) { return &thinkingPickerProvider{p}, nil }
	m := input(1, 10, 0, 1, "first")
	must(t, r.Handle(context.Background(), m))
	r.Wait()
	for i, command := range []string{"/think high", "/provider ollama", "/model default", "/provider default"} {
		m.UpdateID = int64(i + 2)
		m.Text = command
		must(t, r.Handle(context.Background(), m))
		c, err := r.loadModelConversation(m)
		must(t, err)
		if len(c.Pairs) != 1 || c.Thinking != "high" {
			t.Fatalf("%s changed history or thinking: %+v", command, c)
		}
	}
	m.UpdateID = 6
	m.Text = "/think max"
	must(t, r.Handle(context.Background(), m))
	selected, err := r.chatModel(m)
	must(t, err)
	if *selected.Thinking != "high" {
		t.Fatal("unsupported thinking level saved")
	}
	// A removed profile can be recovered through reset without losing workspace state.
	must(t, r.Store.SetChatModel(m.WorkspaceKey(), state.ChatModel{Provider: "removed", ProviderPinned: true, Model: "old"}))
	m.UpdateID = 7
	m.Text = "/settings reset"
	must(t, r.Handle(context.Background(), m))
	selected, err = r.chatModel(m)
	must(t, err)
	if selected.Provider != r.Config.Provider.Name {
		t.Fatal("reset did not recover missing provider")
	}
}

func TestBareThinkListsLevelsForCurrentModel(t *testing.T) {
	r, _, p := pickerFixture(t)
	r.Providers = func(state.Conversation) (llm.Provider, error) { return &thinkingPickerProvider{p}, nil }
	must(t, r.Handle(context.Background(), input(1, 10, 0, 1, "/think")))
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("missing /think reply")
	}
	text := strings.Join(reply.Parts, "\n")
	for _, want := range []string{"Supported levels", "low", "high", "auto"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bare /think missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "Active tasks") {
		t.Fatalf("bare /think dumped status: %s", text)
	}
	must(t, r.Store.Delivered(reply.ID))
	must(t, r.Handle(context.Background(), input(2, 10, 0, 1, "/think high")))
	for old, ok := r.Store.Pending(); ok; old, ok = r.Store.Pending() {
		must(t, r.Store.Delivered(old.ID))
	}
	must(t, r.Handle(context.Background(), input(3, 10, 0, 1, "/think")))
	reply, ok = r.Store.Pending()
	if !ok {
		t.Fatal("missing /think reply after set")
	}
	text = strings.Join(reply.Parts, "\n")
	if !strings.Contains(text, "high") || !strings.Contains(text, "✓") {
		t.Fatalf("bare /think missing current marker: %s", text)
	}
}
