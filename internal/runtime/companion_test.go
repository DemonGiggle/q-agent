package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"q-agent/internal/llm"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

type companionProvider struct {
	requests []llm.Request
	verdict  string
	err      error
	during   func()
}

func (*companionProvider) Name() string { return "ollama" }
func (p *companionProvider) Complete(_ context.Context, req llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.requests = append(p.requests, req)
	if p.during != nil {
		p.during()
	}
	return llm.Response{Message: llm.Message{Content: p.verdict}}, p.err
}

func companionFixture(t *testing.T) (*Runtime, *companionProvider, state.ChatRecordWithKey) {
	t.Helper()
	r, _, _ := fixture(t)
	now := time.Now()
	r.Config.Proactive.ActiveTime = now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	p := &companionProvider{verdict: `{"verdict":"warmth","draft":"Hope your interview preparations are going well.","reason":"context"}`}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	m := input(1, 1, 0, 1, "I've been preparing for an interview")
	must(t, r.Store.RecordChat(m, time.Now().Add(-2*time.Hour)))
	must(t, r.Store.Save(m.ConversationKey(), state.Conversation{Pairs: []state.Pair{{User: m.Text, Answer: "Take it one step at a time."}}}))
	chats, err := r.Store.ListChats()
	must(t, err)
	return r, p, chats[0]
}

func TestCompanionFirstSweepDeliveryAndRestart(t *testing.T) {
	r, p, chat := companionFixture(t)
	must(t, r.proactiveChat(context.Background(), chat))
	if len(p.requests) != 1 || len(p.requests[0].Tools) != 0 {
		t.Fatal("expected one no-tools request")
	}
	if !strings.Contains(p.requests[0].Messages[1].Content, "Current time:") {
		t.Fatal("missing time context")
	}
	reply, ok := r.Store.Pending()
	if !ok || !strings.HasPrefix(reply.ID, "companion:") {
		t.Fatalf("no companion outbox: %+v", reply)
	}
	runs, err := r.Store.CompanionRuns(chat.Key)
	must(t, err)
	if runs[0].Result != "queued" {
		t.Fatalf("wrong result: %+v", runs)
	}
	// Restart with delivery still pending; neither the outbox nor its rate
	// evidence can disappear or be confused with another chat's update 0.
	r.Store, err = state.Open(r.Config.StateDir, nil)
	must(t, err)
	must(t, r.proactiveChat(context.Background(), chat))
	if len(p.requests) != 1 {
		t.Fatal("pending send bypassed rate limit after restart")
	}
	must(t, r.Store.DeferReply(reply.ID))
	text := r.companionHistoryText(chat.WorkspaceRoute())
	if !strings.Contains(text, "NOT confirmed delivered") || !strings.Contains(text, "delivery failed") {
		t.Fatal(text)
	}
	must(t, r.Store.Delivered(reply.ID))
	contextMessages := r.memoryHistory(chat.WorkspaceRoute(), state.Conversation{})
	if len(contextMessages) == 0 || contextMessages[0].Origin != "companion_checkins" || !contextMessages[0].Untrusted {
		t.Fatal("delivered check-in missing from follow-up context")
	}
	otherTopic := chat.WorkspaceRoute()
	otherTopic.TopicID = 42
	if len(r.companionConversationContext(otherTopic)) != 0 {
		t.Fatal("check-in crossed topic boundary")
	}
	r.Store, err = state.Open(r.Config.StateDir, nil)
	must(t, err)
	must(t, r.proactiveChat(context.Background(), chat))
	if len(p.requests) != 1 {
		t.Fatal("restart reset daily cap")
	}
	if !strings.Contains(r.companionHistoryText(chat.WorkspaceRoute()), "OK · delivered") {
		t.Fatal("missing delivery success")
	}
}

func TestCompanionHTTPDeliveryUpdatesDiagnostics(t *testing.T) {
	r, _, chat := companionFixture(t)
	must(t, r.proactiveChat(context.Background(), chat))
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	defer server.Close()
	client := telegram.New("test")
	client.BaseURL = server.URL
	client.HTTP = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); client.Deliver(ctx, r.Store, func(err error) { t.Error(err) }) }()
	defer func() { cancel(); <-done }()
	select {
	case payload := <-requests:
		if payload["chat_id"] != float64(1) || !strings.Contains(payload["text"].(string), "interview") {
			t.Fatalf("%+v", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no Telegram send")
	}
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		runs, err := r.Store.CompanionRuns(chat.Key)
		must(t, err)
		if runs[0].Result == "delivered" {
			break
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("successful send not recorded")
		}
	}
	if !strings.Contains(r.companionHistoryText(chat.WorkspaceRoute()), "OK · delivered") {
		t.Fatal("missing successful HTTP delivery in menu")
	}
}

func TestCompanionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, want string
		providerError       bool
	}{
		{"silence", `{"verdict":"silent","reason":"not needed"}`, "silent", false},
		{"bad JSON", "invalid", "failed", false},
		{"provider error", "", "failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p, chat := companionFixture(t)
			p.verdict = tc.verdict
			if tc.providerError {
				p.err = fmt.Errorf("private provider failure")
			}
			err := r.proactiveChat(context.Background(), chat)
			if (err != nil) != (tc.want == "failed") {
				t.Fatalf("error=%v", err)
			}
			runs, e := r.Store.CompanionRuns(chat.Key)
			must(t, e)
			if len(runs) != 1 || runs[0].Result != tc.want || runs[0].Finished.IsZero() {
				t.Fatalf("%+v", runs)
			}
			if _, ok := r.Store.Pending(); ok {
				t.Fatal("unexpected message")
			}
			if strings.Contains(r.companionHistoryText(chat.WorkspaceRoute()), "private provider") {
				t.Fatal("diagnostics leaked error")
			}
		})
	}
}

func TestCompanionSkipsAndMidDecisionChanges(t *testing.T) {
	for _, kind := range []string{"off", "recent", "busy", "opt out during decision", "activity during decision", "window ended"} {
		t.Run(kind, func(t *testing.T) {
			r, p, chat := companionFixture(t)
			switch kind {
			case "off":
				must(t, r.Store.SetCompanion(chat.Key, state.CompanionPref{Mode: "off"}))
			case "recent":
				chat.Record.LastSeen = time.Now()
			case "busy":
				r.active = map[string]task{chat.Key: {}}
			case "opt out during decision":
				p.during = func() { must(t, r.Store.SetCompanion(chat.Key, state.CompanionPref{Mode: "off"})) }
			case "activity during decision":
				p.during = func() { must(t, r.Store.RecordChat(chat.WorkspaceRoute(), time.Now())) }
			case "window ended":
				p.during = func() {
					r.Config.Proactive.ActiveTime = time.Now().Add(time.Hour).Format("15:04") + "-" + time.Now().Add(2*time.Hour).Format("15:04")
				}
			}
			must(t, r.proactiveChat(context.Background(), chat))
			delete(r.active, chat.Key)
			if _, ok := r.Store.Pending(); ok {
				t.Fatal("unexpected check-in")
			}
			runs, err := r.Store.CompanionRuns(chat.Key)
			must(t, err)
			if len(runs) != 1 || runs[0].Result != "skipped" {
				t.Fatalf("%+v", runs)
			}
		})
	}
}

func TestCompanionGlobalKillSwitchAndMenu(t *testing.T) {
	r, p, chat := companionFixture(t)
	r.Config.Proactive.Enabled = false
	must(t, r.Store.SetCompanion(chat.Key, state.CompanionPref{Mode: "on"}))
	must(t, r.proactiveSweep(context.Background()))
	if len(p.requests) != 0 || state.CompanionEffective(false, state.CompanionPref{Mode: "on"}) {
		t.Fatal("global kill switch ignored")
	}
	r.Sink = &pickerSink{}
	m := input(2, 1, 0, 1, "/companion")
	must(t, r.Handle(context.Background(), m))
	reply, ok := r.Store.Pending()
	if !ok || reply.Keyboard == nil || !strings.Contains(reply.Parts[0], "Current: off") || !strings.Contains(reply.Parts[0], "Today's companion checks") {
		t.Fatalf("%+v", reply)
	}
	if !strings.Contains(reply.Parts[0], "Global switch off") {
		t.Fatal(reply.Parts[0])
	}
}

func TestCompanionNoRepeatAfterCooldown(t *testing.T) {
	r, p, chat := companionFixture(t)
	r.Config.Proactive.MaxPerDay = 10
	r.Config.Proactive.Cooldown = "1ns"
	must(t, r.proactiveChat(context.Background(), chat))
	reply, _ := r.Store.Pending()
	must(t, r.Store.Delivered(reply.ID))
	must(t, r.proactiveChat(context.Background(), chat))
	if len(p.requests) != 1 {
		t.Fatal("unchanged history triggered another model request")
	}
	m := chat.WorkspaceRoute()
	must(t, r.Store.Save(m.ConversationKey(), state.Conversation{Pairs: []state.Pair{{User: "More preparation today", Answer: "Sounds good"}}}))
	must(t, r.proactiveChat(context.Background(), chat))
	if len(p.requests) != 2 {
		t.Fatal("new history did not reach model")
	}
	if _, ok := r.Store.Pending(); ok {
		t.Fatal("identical draft was sent again")
	}
	if !strings.Contains(p.requests[1].Messages[1].Content, "Hope your interview") {
		t.Fatal("missing previous check-in context")
	}
}

func TestCompanionDiagnosticsTimezoneAndIsolation(t *testing.T) {
	r, _, chat := companionFixture(t)
	r.Config.Proactive.Timezone = "Pacific/Kiritimati"
	r.Config.Proactive.ActiveTime = "23:00-22:59" // wrap-around, excluding only one minute
	loc := memoryLocation(r.Config.Proactive.Timezone)
	now := time.Now().In(loc)
	// Use a start safely inside today's window, including on UTC date changes.
	started := time.Date(now.Year(), now.Month(), now.Day(), 12, 34, 56, 0, loc)
	id, err := r.Store.StartCompanionRun(chat.Key, started)
	must(t, err)
	must(t, r.Store.FinishCompanionRun(chat.Key, id, "silent", "model chose silence"))
	id, err = r.Store.StartCompanionRun("another-workspace", started)
	must(t, err)
	must(t, r.Store.FinishCompanionRun("another-workspace", id, "failed", "OTHER CHAT"))
	id, err = r.Store.StartCompanionRun(chat.Key, started.AddDate(0, 0, -1))
	must(t, err)
	must(t, r.Store.FinishCompanionRun(chat.Key, id, "failed", "YESTERDAY"))
	text := r.companionHistoryText(chat.WorkspaceRoute())
	if !strings.Contains(text, "12:34:56 — OK · stayed silent") || !strings.Contains(text, now.Format("2006-01-02")) || strings.Contains(text, "OTHER CHAT") || strings.Contains(text, "YESTERDAY") {
		t.Fatal(text)
	}
	if !utf8.ValidString(truncateUTF8(strings.Repeat("陪伴", 3000), 6000)) {
		t.Fatal("invalid UTF-8 cap")
	}
}
