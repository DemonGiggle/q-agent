package runtime

import (
	"context"
	"strings"
	"testing"

	"q-agent/internal/llm"
	"q-agent/internal/state"
)

type unknownThinkingProvider struct{ *pickerProvider }

func (*unknownThinkingProvider) ThinkingCapability(string) llm.ThinkingCapability {
	return llm.ThinkingCapability{
		Supported:     false,
		Unknown:       true,
		RequestFormat: llm.ThinkingRequestCodexReasoning,
		ReplayFormat:  llm.ThinkingReplayReasoningDetails,
	}
}

// #24: Unknown must not be treated as "does not support thinking".
func TestThinkUnknownAcceptsPlausibleRejectsClientOnly(t *testing.T) {
	r, _, p := pickerFixture(t)
	r.Providers = func(state.Conversation) (llm.Provider, error) { return &unknownThinkingProvider{p}, nil }

	// Bare /think must not claim unsupported.
	must(t, r.Handle(context.Background(), input(1, -10, 0, 1, "/think")))
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("missing /think reply")
	}
	text := strings.Join(reply.Parts, "\n")
	if strings.Contains(text, "does not support thinking") {
		t.Fatalf("Unknown must not report unsupported: %s", text)
	}
	if !strings.Contains(text, "have not been loaded yet") {
		t.Fatalf("Unknown should hint catalog not loaded: %s", text)
	}
	must(t, r.Store.Delivered(reply.ID))

	// Plausible level accepted optimistically.
	must(t, r.Handle(context.Background(), input(2, -10, 0, 1, "/think high")))
	for old, ok := r.Store.Pending(); ok; old, ok = r.Store.Pending() {
		must(t, r.Store.Delivered(old.ID))
	}
	m := input(2, -10, 0, 1, "")
	selected, err := r.chatModel(m)
	must(t, err)
	if selected.Thinking == nil || *selected.Thinking != "high" {
		t.Fatalf("Unknown plausible should be saved, got %+v", selected.Thinking)
	}

	// Client-only rejected.
	must(t, r.Handle(context.Background(), input(3, -10, 0, 1, "/think ultra")))
	selected, err = r.chatModel(m)
	must(t, err)
	if *selected.Thinking != "high" {
		t.Fatalf("ultra must be rejected, thinking=%q", *selected.Thinking)
	}

	// Typo rejected.
	must(t, r.Handle(context.Background(), input(4, -10, 0, 1, "/think highe")))
	selected, err = r.chatModel(m)
	must(t, err)
	if *selected.Thinking != "high" {
		t.Fatalf("typo must be rejected, thinking=%q", *selected.Thinking)
	}
}
