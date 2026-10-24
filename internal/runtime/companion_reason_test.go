package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestSilentDetailKeepsReason(t *testing.T) {
	if got := silentDetail(""); got != "model chose silence" {
		t.Fatalf("empty reason: %q", got)
	}
	if got := silentDetail("  not needed  "); got != "model chose silence: not needed" {
		t.Fatalf("trim: %q", got)
	}
	if got := silentDetail("line one\nline two\tline three"); got != "model chose silence: line one line two line three" {
		t.Fatalf("multiline: %q", got)
	}
	long := strings.Repeat("a", 500)
	got := silentDetail(long)
	if !strings.HasPrefix(got, "model chose silence: ") {
		t.Fatalf("prefix: %q", got)
	}
	if n := len([]rune(strings.TrimPrefix(got, "model chose silence: "))); n != proactiveSilentReasonMaxRunes {
		t.Fatalf("not capped to %d: got %d", proactiveSilentReasonMaxRunes, n)
	}
}

func TestSilentReasonReachesHistory(t *testing.T) {
	r, p, chat := companionFixture(t)
	p.verdict = `{"verdict":"silent","reason":"just small talk, nothing to add"}`
	r.Config.Proactive.MaxPerDay = 10
	r.Config.Proactive.Cooldown = "1ns"
	must(t, r.proactiveChat(context.Background(), chat))
	runs, err := r.Store.CompanionRuns(chat.Key)
	must(t, err)
	if len(runs) != 1 || runs[0].Result != "silent" {
		t.Fatalf("%+v", runs)
	}
	if runs[0].Detail != "model chose silence: just small talk, nothing to add" {
		t.Fatalf("detail: %q", runs[0].Detail)
	}
	text := r.companionHistoryText(chat.WorkspaceRoute())
	if !strings.Contains(text, "OK · stayed silent · model chose silence: just small talk") {
		t.Fatalf("history missing reason: %q", text)
	}
	if _, ok := r.Store.Pending(); ok {
		t.Fatal("silent sweep queued a message")
	}
}

func TestDeciderIsSociable(t *testing.T) {
	r, p, chat := companionFixture(t)
	p.verdict = `{"verdict":"silent","reason":"x"}`
	r.Config.Proactive.MaxPerDay = 10
	r.Config.Proactive.Cooldown = "1ns"
	must(t, r.proactiveChat(context.Background(), chat))
	if len(p.requests) != 1 {
		t.Fatalf("expected one request, got %d", len(p.requests))
	}
	req := p.requests[0]
	if req.Temperature != proactiveTemperature {
		t.Fatalf("temperature=%v, want %v", req.Temperature, proactiveTemperature)
	}
	if proactiveTemperature <= 0.2 {
		t.Fatalf("temperature not raised: %v", proactiveTemperature)
	}
	sys := req.Messages[0].Content
	if !strings.Contains(sys, "ARE valid triggers for warmth") {
		t.Fatalf("prompt does not invite warmth on chatter: %q", sys[:200])
	}
	if strings.Contains(sys, "empty or boring history") {
		t.Fatalf("prompt still silences boring history: %q", sys[:200])
	}
}
