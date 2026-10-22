package llm

import "strings"

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

// codexCapabilityForEntry converts one decoded catalog entry into a
// ThinkingCapability. v1 excludes ultra/persistent/custom (see above) and
// never invents levels: no levels => unsupported, same as pre-#23.
func codexCapabilityForEntry(entry CodexModelEntry) ThinkingCapability {
	levels := codexFilteredLevels(entry.Levels())
	if len(levels) == 0 {
		return ThinkingCapability{Supported: false}
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
	cap := ThinkingCapability{
		Supported:     true,
		Adjustable:    len(levels) > 1,
		AlwaysOn:      len(levels) == 1,
		Levels:        append([]string(nil), levels...),
		Default:       defaultLevel,
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
	}
	if cap.AlwaysOn && cap.Default == "" {
		cap.Default = levels[0]
	}
	return cap
}

// findCodexEntry matches a model against slug then aliases, exact first then
// case-insensitive. Returns nil for unknown models so callers can keep the
// pre-#23 unsupported behavior (unknown is handled in #24/#25).
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
