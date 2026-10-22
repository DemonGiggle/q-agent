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
	// Single-level model: NOT always-on (one option does not prove backend
	// behavior when effort is omitted); explicit single effort must still be
	// sent, auto omits. Default is the advertised catalog default only.
	single := CodexModelEntry{
		Slug:             "solo",
		DefaultReasoning: "max",
		SupportedReasoning: []CodexReasoningLevelOption{
			{Effort: "max"},
		},
	}
	singleCap := codexCapabilityForEntry(single)
	if singleCap.AlwaysOn || !singleCap.Supported || singleCap.Adjustable || singleCap.Default != "max" {
		t.Fatalf("single = %+v, want supported non-adjustable non-always-on default max", singleCap)
	}
	if len(singleCap.Levels) != 1 || singleCap.Levels[0] != "max" {
		t.Fatalf("single levels = %+v, want [max]", singleCap.Levels)
	}
	if err := ValidateCodexThinkingLevel(singleCap, "max"); err != nil {
		t.Fatalf("single explicit max must validate: %v", err)
	}
	if out := codexRequestBodyWithCapability(singleCap, Request{Model: single.Slug, Thinking: "max"}); out["reasoning"] == nil {
		t.Fatalf("single explicit max must send reasoning, got %+v", out)
	}
	// Single ["none"]: must not be reported as always-on; explicit none sends.
	noneOnly := CodexModelEntry{
		Slug:               "none-only",
		SupportedReasoning: []CodexReasoningLevelOption{{Effort: "none"}},
	}
	noneCap := codexCapabilityForEntry(noneOnly)
	if noneCap.AlwaysOn || !noneCap.Supported {
		t.Fatalf("none-only = %+v, want supported non-always-on", noneCap)
	}
	if err := ValidateCodexThinkingLevel(noneCap, "none"); err != nil {
		t.Fatalf("none-only explicit none must validate: %v", err)
	}
	if out := codexRequestBodyWithCapability(noneCap, Request{Model: noneOnly.Slug, Thinking: "none"}); out["reasoning"] == nil {
		t.Fatalf("none-only explicit none must send reasoning")
	}
	// No synthesized default when the catalog advertises none.
	noDef := CodexModelEntry{
		Slug:               "solo-nodef",
		SupportedReasoning: []CodexReasoningLevelOption{{Effort: "high"}},
	}
	noDefCap := codexCapabilityForEntry(noDef)
	if noDefCap.AlwaysOn || noDefCap.Default != "" {
		t.Fatalf("single without advertised default must not synthesize default, got %+v", noDefCap)
	}
}
