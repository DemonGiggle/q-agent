package llm

import (
	"fmt"
	"strings"
)

// isCodexClientOnlyEffort reports efforts q-agent does not send on the wire
// in v1. Upstream resolve_reasoning_effort maps ultra (observed: gpt-6.1-sol
// ultra -> wire xhigh) and persistent (-> disabled); q-agent has no
// multi-agent capability that uses multi_agent_reasoning_effort, so silent
// downgrade would mislead. Callers must filter these from Levels and the
// wire encoder must refuse them defensively.
func isCodexClientOnlyEffort(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "ultra", "persistent", "custom":
		return true
	default:
		return false
	}
}

// codexFilteredLevels drops client-only efforts while preserving catalog
// order. Empty input stays empty.
func codexFilteredLevels(levels []string) []string {
	if len(levels) == 0 {
		return nil
	}
	out := make([]string, 0, len(levels))
	for _, lv := range levels {
		if strings.TrimSpace(lv) == "" {
			continue
		}
		if isCodexClientOnlyEffort(lv) {
			continue
		}
		out = append(out, lv)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// codexPlausibleLevels is the fallback set used while the catalog is unknown.
// It comes from the official reasoning guide (none/minimal/low/medium/high/
// xhigh/max are per-model) minus client-only ultra/persistent/custom. It is
// NOT authoritative for a known model: known models validate strictly against
// their catalog Levels.
var codexPlausibleLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// isCodexPlausibleLevel reports whether level is in the unknown-fallback set.
func isCodexPlausibleLevel(level string) bool {
	norm := strings.ToLower(strings.TrimSpace(level))
	for _, lv := range codexPlausibleLevels {
		if norm == lv {
			return true
		}
	}
	return false
}

// CodexPlausibleLevels returns the fallback set used while the catalog is
// unknown. It is NOT authoritative for a known model.
func CodexPlausibleLevels() []string {
	return append([]string(nil), codexPlausibleLevels...)
}

// unknownCodexCapability marks "catalog not loaded / model absent" distinctly
// from unsupported. Wire format is still codex_reasoning so an explicit
// plausible level can be sent optimistically; UI must not claim the model
// cannot think.
func unknownCodexCapability() ThinkingCapability {
	return ThinkingCapability{
		Supported:     false,
		Adjustable:    false,
		AlwaysOn:      false,
		Levels:        nil,
		Default:       "",
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
		Unknown:       true,
	}
}

// codexCapabilityForEntry converts one decoded catalog entry into a
// ThinkingCapability. v1 excludes ultra/persistent/custom (see above).
//
// Empty raw SupportedReasoning (legacy fallback or server omits metadata)
// returns Unknown, not unsupported: we must not claim "cannot think" when we
// simply have not learned levels. Only an entry that advertises reasoning
// metadata but filters to zero usable tiers reports unsupported.
//
// A single advertised level is NOT always-on: one selectable effort does not
// prove the backend's behavior when effort is omitted (and ["none"] has the
// opposite of always-on semantics), and the catalog default is never
// synthesized. Selector adjustability, always-on state, advertised default,
// and explicit effort wire encoding are handled separately.
func codexCapabilityForEntry(entry CodexModelEntry) ThinkingCapability {
	raw := entry.Levels()
	if len(raw) == 0 {
		cap := unknownCodexCapability()
		cap.Unknown = true
		return cap
	}
	levels := codexFilteredLevels(raw)
	if len(levels) == 0 {
		return ThinkingCapability{Supported: false, Unknown: false}
	}
	def := strings.ToLower(strings.TrimSpace(entry.DefaultReasoning))
	defaultLevel := ""
	if def != "" && !isCodexClientOnlyEffort(def) {
		for _, lv := range levels {
			if strings.ToLower(lv) == def {
				defaultLevel = lv
				break
			}
		}
	}
	return ThinkingCapability{
		Supported:     true,
		Adjustable:    len(levels) > 1,
		AlwaysOn:      false,
		Levels:        append([]string(nil), levels...),
		Default:       defaultLevel,
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
		Unknown:       false,
	}
}

// ValidateCodexThinkingLevel checks an explicit level against a Codex
// capability. Empty (auto) is always allowed and handled by the caller.
// Unknown allows any plausible non-client-only effort optimistically; known
// models require strict membership. Client-only ultra/persistent/custom are
// always rejected in v1. A single advertised level is validated by membership
// like any known set; it never reports "always on".
func ValidateCodexThinkingLevel(cap ThinkingCapability, level string) error {
	norm := strings.ToLower(strings.TrimSpace(level))
	if norm == "" {
		return nil
	}
	if isCodexClientOnlyEffort(norm) {
		return fmt.Errorf("thinking level %q is not sendable in v1 (client-only)", level)
	}
	if cap.Unknown {
		if isCodexPlausibleLevel(norm) {
			return nil
		}
		return fmt.Errorf("unknown thinking level %q", level)
	}
	if !cap.Supported {
		return fmt.Errorf("model does not support thinking levels")
	}
	for _, lv := range cap.Levels {
		if norm == strings.ToLower(strings.TrimSpace(lv)) {
			return nil
		}
	}
	if len(cap.Levels) == 0 {
		return fmt.Errorf("thinking is not adjustable for this model")
	}
	return fmt.Errorf("unsupported thinking level %q", level)
}

// findCodexEntry matches a model against slug then aliases, exact first then
// case-insensitive. Returns nil for unknown models so callers can return an
// explicit Unknown capability instead of unsupported.
func findCodexEntry(snap CodexCatalogSnapshot, model string) *CodexModelEntry {
	m := strings.TrimSpace(model)
	if m == "" {
		return nil
	}
	// Exact slug.
	for i := range snap.Entries {
		if snap.Entries[i].Slug == m {
			return &snap.Entries[i]
		}
	}
	// Exact alias.
	for i := range snap.Entries {
		for _, a := range snap.Entries[i].Aliases {
			if a == m {
				return &snap.Entries[i]
			}
		}
	}
	// Case-insensitive slug.
	for i := range snap.Entries {
		if strings.EqualFold(snap.Entries[i].Slug, m) {
			return &snap.Entries[i]
		}
	}
	// Case-insensitive alias.
	for i := range snap.Entries {
		for _, a := range snap.Entries[i].Aliases {
			if strings.EqualFold(a, m) {
				return &snap.Entries[i]
			}
		}
	}
	return nil
}
