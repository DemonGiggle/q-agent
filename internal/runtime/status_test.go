package runtime

import (
	"context"
	"strings"
	"testing"

	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

func TestStatusReadOnlyAndSafe(t *testing.T) {
	r, w, p := fixture(t)
	r.Config.Provider.Name = "openai"
	r.Config.Provider.Model = "configured-model"
	r.Config.Provider.Thinking = "high"
	r.Config.Provider.BaseURL = "https://user:password@private.example/?token=hidden"
	r.Config.Provider.APIKeyEnv = "PRIVATE_KEY_SOURCE"
	r.Config.Provider.APIKeyPath = "/private/key"
	r.Config.Telegram.TokenPath = "/private/bot-token"
	r.Config.Sandbox.Enabled = true
	m := input(1, 10, 7, 1, "/status")
	original := state.Conversation{Provider: "old", Model: "old-model", Pairs: []state.Pair{{User: "private prompt", Answer: "private answer"}}}
	if err := r.Store.Save(m.ConversationKey(), original); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reply, ok := r.Store.Pending()
	if !ok || reply.Route.TopicID != 7 || reply.Route.ChatID != 10 {
		t.Fatalf("bad route: %+v", reply)
	}
	text := strings.Join(reply.Parts, "")
	for _, want := range []string{"Provider: openai", "Model: configured-model", "Thinking: high", "Execution: Sandbox", "This topic: Idle", "Active tasks: 0/2", "Scheduler: Stopped", "Model steps: 8", "Tool calls: 16", "Task timeout: 5m", "Shell timeout: 2m"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
	for _, hidden := range []string{"private.example", "password", "PRIVATE_KEY_SOURCE", "/private", "private prompt", "private answer", r.Config.StateDir} {
		if strings.Contains(text, hidden) {
			t.Errorf("leaked %q", hidden)
		}
	}
	got, err := r.Store.Load(m.ConversationKey())
	if err != nil || got.Model != original.Model || len(got.Pairs) != 1 {
		t.Fatalf("history changed: %+v %v", got, err)
	}
	if w.calls != 0 || len(p.requests) != 0 {
		t.Fatal("status executed agent")
	}
}

func TestStatusWhileBusy(t *testing.T) {
	for _, tc := range []struct {
		topic int64
		want  string
	}{{7, "Running"}, {8, "Busy (another topic in this chat)"}} {
		t.Run(tc.want, func(t *testing.T) {
			r, w, _ := fixture(t)
			w.block = make(chan struct{})
			w.started = make(chan struct{}, 1)
			if err := r.Handle(context.Background(), input(1, 10, 7, 1, "work")); err != nil {
				t.Fatal(err)
			}
			<-w.started
			if err := r.Handle(context.Background(), input(2, 10, tc.topic, 1, "/status")); err != nil {
				t.Fatal(err)
			}
			reply, ok := r.Store.Pending()
			if !ok || !strings.Contains(reply.Parts[0], "This topic: "+tc.want) || !strings.Contains(reply.Parts[0], "Active tasks: 1/2") {
				t.Fatalf("%+v", reply)
			}
			close(w.block)
			r.Wait()
			if w.calls != 1 {
				t.Fatal("status started another task")
			}
		})
	}
}

func TestInfoCommandsAuthorizationAndDedup(t *testing.T) {
	for _, command := range []string{"/status", "/help"} {
		t.Run(command, func(t *testing.T) {
			r, w, p := fixture(t)
			for _, m := range []struct{ id, chat, user int64 }{{1, 10, 2}, {2, 99, 1}} {
				if err := r.Handle(context.Background(), input(m.id, m.chat, 7, m.user, command)); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := r.Store.Pending(); ok {
				t.Fatal("unauthorized reply")
			}
			m := input(3, 10, 7, 1, command)
			if err := r.Handle(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			reply, ok := r.Store.Pending()
			if !ok {
				t.Fatal("missing reply")
			}
			for range reply.Parts {
				if err := r.Store.Delivered(reply.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Handle(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.Store.Pending(); ok {
				t.Fatal("duplicate reply")
			}
			if w.calls != 0 || len(p.requests) != 0 {
				t.Fatal("info command executed agent")
			}
		})
	}
}

func TestStatusRemoteRedactionAndSchedulerLifecycle(t *testing.T) {
	r, _, _ := fixture(t)
	r.Config.Provider.Model = "sensitive-model"
	r.Options.Redaction, _ = redaction.New(redaction.Config{}, []string{"sensitive-model"})
	r.scheduleDone = make(chan struct{})
	r.mu.Lock()
	text := r.statusLocked(input(1, 10, 7, 1, "/status"))
	close(r.scheduleDone)
	stopped := r.statusLocked(input(1, 10, 7, 1, "/status"))
	r.mu.Unlock()
	if strings.Contains(text, "sensitive-model") || !strings.Contains(text, "Scheduler: Running") || !strings.Contains(stopped, "Scheduler: Stopped") {
		t.Fatalf("%s\n%s", text, stopped)
	}
}
