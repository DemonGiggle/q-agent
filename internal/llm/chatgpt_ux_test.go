package llm

import (
	"testing"
)

// #25: UI and wire must share the same snapshot-derived capability.
// The same CodexModelEntry drives both the selector levels and the request
// encoding; auto omits, explicit sends effort only (no summary).
func TestCodexUXWireMatchesUICapability(t *testing.T) {
	clearCodexCatalogCache()
	entry := CodexModelEntry{
		Slug:             "gpt-6.1-sol",
		Visibility:       "list",
		DefaultReasoning: "low",
		SupportedReasoning: []CodexReasoningLevelOption{
			{Effort: "low"}, {Effort: "medium"}, {Effort: "high"}, {Effort: "xhigh"}, {Effort: "max"},
		},
	}
	cap := codexCapabilityForEntry(entry)
	// UI: levels + catalog default.
	if len(cap.Levels) != 5 || cap.Default != "low" {
		t.Fatalf("cap = %+v, want 5 levels default low", cap)
	}
	if cap.RequestFormat != ThinkingRequestCodexReasoning {
		t.Fatalf("RequestFormat = %q, want codex_reasoning", cap.RequestFormat)
	}
	// Wire: same cap, auto omits, explicit sends effort without summary.
	if out := codexRequestBodyWithCapability(cap, Request{Model: entry.Slug}); out["reasoning"] != nil {
		t.Fatalf("auto must omit, got %v", out["reasoning"])
	}
	got := codexRequestBodyWithCapability(cap, Request{Model: entry.Slug, Thinking: "high"})
	raw, ok := got["reasoning"]
	if !ok {
		t.Fatalf("explicit high must send reasoning")
	}
	m, ok := raw.(map[string]any)
	if !ok || m["effort"] != "high" {
		t.Fatalf("effort = %v, want high", raw)
	}
	if _, hasSummary := m["summary"]; hasSummary {
		t.Fatalf("codex must not send summary, got %v", m)
	}
	// Single-level model: AlwaysOn UI still uses same cap; wire sends it.
	single := CodexModelEntry{
		Slug:             "solo",
		DefaultReasoning: "max",
		SupportedReasoning: []CodexReasoningLevelOption{
			{Effort: "max"},
		},
	}
	singleCap := codexCapabilityForEntry(single)
	if !singleCap.AlwaysOn || singleCap.Default != "max" {
		t.Fatalf("single = %+v, want always-on max", singleCap)
	}
}
