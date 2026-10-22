package runtime

import (
	"context"
	"strings"
	"testing"

	"q-agent/internal/llm"
	"q-agent/internal/state"
)

type codexUXProvider struct {
	*pickerProvider
	cap llm.ThinkingCapability
}

func (p *codexUXProvider) ThinkingCapability(string) llm.ThinkingCapability {
	return p.cap
}

func codexUXCap() llm.ThinkingCapability {
	return llm.ThinkingCapability{
		Supported:     true,
		Adjustable:    true,
		Levels:        []string{"low", "medium", "high", "xhigh", "max"},
		Default:       "low",
		RequestFormat: llm.ThinkingRequestCodexReasoning,
		ReplayFormat:  llm.ThinkingReplayReasoningDetails,
		Source:        "live",
		Stale:         false,
	}
}

func drainPending(t *testing.T, r *Runtime) {
	t.Helper()
	for old, ok := r.Store.Pending(); ok; old, ok = r.Store.Pending() {
		if err := r.Store.Delivered(old.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func bareThink(t *testing.T, r *Runtime, id int64) string {
	t.Helper()
	drainPending(t, r)
	must(t, r.Handle(context.Background(), input(id, -10, 0, 1, "/think")))
	reply, ok := r.Store.Pending()
	if !ok {
		t.Fatal("missing /think reply")
	}
	return strings.Join(reply.Parts, "\n")
}

// #25: auto / explicit / config-inherited default / unknown each have distinct
// strings; catalog default and source/stale are shown separately.
func TestThinkUXAutoExplicitDefaultUnknown(t *testing.T) {
	r, _, p := pickerFixture(t)
	cap := codexUXCap()
	r.Providers = func(state.Conversation) (llm.Provider, error) { return &codexUXProvider{p, cap}, nil }

	// auto (no override, config default empty): distinct auto + catalog lines.
	// Note: reply rendering converts `x` to <code>x</code>, so assert on text
	// content, not backticks.
	text := bareThink(t, r, 1)
	for _, want := range []string{
		"Current:",
		"auto",
		"unspecified; backend decides",
		"Chat override: none",
		"Configured default:",
		"Codex catalog default:",
		"low",
		"Catalog: live (fresh)",
		"Supported levels",
		"Summary: omitted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("auto state missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "have not been loaded yet") {
		t.Fatalf("known catalog must not claim not loaded: %s", text)
	}
	drainPending(t, r)

	// explicit high: current marker + chat override.
	must(t, r.Handle(context.Background(), input(2, -10, 0, 1, "/think high")))
	drainPending(t, r)
	text = bareThink(t, r, 3)
	for _, want := range []string{"Current:", "high", "(chat override)", "✓"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explicit state missing %q: %s", want, text)
		}
	}

	// explicit auto: omits effort but records an explicit override.
	must(t, r.Handle(context.Background(), input(4, -10, 0, 1, "/think auto")))
	drainPending(t, r)
	text = bareThink(t, r, 5)
	for _, want := range []string{"Current:", "auto", "unspecified; backend decides", "explicit, omits effort"} {
		if !strings.Contains(text, want) {
			t.Fatalf("explicit auto missing %q: %s", want, text)
		}
	}

	// default: clears override, restores configured default (set config to high).
	r.Config.Provider.Thinking = "high"
	must(t, r.Handle(context.Background(), input(6, -10, 0, 1, "/think default")))
	drainPending(t, r)
	text = bareThink(t, r, 7)
	for _, want := range []string{"Current:", "high", "Chat override: none", "Configured default:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("default state missing %q: %s", want, text)
		}
	}
}

// Unknown shows "not loaded yet", never "does not support", plus catalog line.
func TestThinkUXUnknownDistinct(t *testing.T) {
	r, _, p := pickerFixture(t)
	unknown := llm.ThinkingCapability{
		Supported:     false,
		Unknown:       true,
		RequestFormat: llm.ThinkingRequestCodexReasoning,
		ReplayFormat:  llm.ThinkingReplayReasoningDetails,
	}
	r.Providers = func(state.Conversation) (llm.Provider, error) { return &codexUXProvider{p, unknown}, nil }
	text := bareThink(t, r, 1)
	if strings.Contains(text, "does not support thinking") {
		t.Fatalf("unknown must not report unsupported: %s", text)
	}
	for _, want := range []string{
		"have not been loaded yet",
		"Catalog: not loaded yet",
		"Codex catalog default: unknown",
		"unspecified; backend decides",
		"clears the chat override",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unknown state missing %q: %s", want, text)
		}
	}
	drainPending(t, r)
}

// Stale catalog is labeled, never hidden.
func TestThinkUXStaleLabeled(t *testing.T) {
	r, _, p := pickerFixture(t)
	cap := codexUXCap()
	cap.Stale = true
	r.Providers = func(state.Conversation) (llm.Provider, error) { return &codexUXProvider{p, cap}, nil }
	text := bareThink(t, r, 1)
	if !strings.Contains(text, "Catalog: live (stale, last-known-good)") {
		t.Fatalf("stale must be labeled: %s", text)
	}
	drainPending(t, r)
}
