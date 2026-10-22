package llm

import (
	"fmt"
	"strings"
)

const (
	ThinkingRequestNone            = "none"
	ThinkingRequestReasoningEffort = "reasoning_effort"
	ThinkingRequestObject          = "thinking"
	ThinkingRequestOllamaThink     = "think"
	ThinkingReplayNone             = "none"
	ThinkingReplayReasoningContent = "reasoning_content"
	ThinkingReplayReasoningDetails = "reasoning_details"
)

// ThinkingRequestResponsesReasoning encodes a level as the Responses API
// reasoning object: {"reasoning": {"effort": "<level>"}}.
const ThinkingRequestResponsesReasoning = "responses_reasoning"

// ThinkingRequestCodexReasoning encodes a Codex level as
// {"reasoning": {"effort": "<level>"}} with no summary by default.
// It reuses the Responses effort encoding but deliberately does not copy the
// "effort != none => summary:auto" policy: summary omission means the server
// default, and summary=none is an omission, not a wire value.
const ThinkingRequestCodexReasoning = "codex_reasoning"

// openCodeGoThinkingCatalog records qcode's exact-match OpenCode Go adapters
// and conservative built-in choices. Official CLI runs can replace the choices
// from Models.dev, while request and replay formats stay local.
var openCodeGoThinkingCatalog = map[string]ThinkingCapability{
	// OpenCode Go currently exposes deepseek-flash as an alias of V4.1 Flash.
	// V4.1 accepts these named aliases (as well as a numeric 1–100 budget,
	// which qcode intentionally does not expose as a selector value).
	"deepseek-flash": {
		Supported: true, Adjustable: true, Levels: []string{"low", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"deepseek-v4.1-flash": {
		Supported: true, Adjustable: true, Levels: []string{"low", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"deepseek-v4-flash": {
		Supported: true, Adjustable: true, Levels: []string{"off", "low", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"deepseek-v4-flash-vision-exp": {
		Supported: true, Adjustable: true, Levels: []string{"off", "low", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"deepseek-v4-pro": {
		Supported: true, Adjustable: true, Levels: []string{"off", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"glm-5": {
		Supported: true, Adjustable: true, Levels: []string{"off", "on"}, Default: "on",
		RequestFormat: ThinkingRequestObject, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"glm-5.1": {
		Supported: true, Adjustable: true, Levels: []string{"off", "on"}, Default: "on",
		RequestFormat: ThinkingRequestObject, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"glm-5.2": {
		Supported: true, Adjustable: true, Levels: []string{"high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"glm-5.3": {
		Supported: true, Adjustable: true, Levels: []string{"low", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"glm-5.3-flash": {
		Supported: true, Adjustable: true, Levels: []string{"low", "high", "max"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"kimi-k2.5": {
		Supported: true, Adjustable: true, Levels: []string{"off", "on"}, Default: "on",
		RequestFormat: ThinkingRequestObject, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"kimi-k2.6": {
		Supported: true, Adjustable: true, Levels: []string{"off", "on"}, Default: "on",
		RequestFormat: ThinkingRequestObject, ReplayFormat: ThinkingReplayReasoningContent,
	},
	// Kimi K3 only advertises the max reasoning tier, so present it as an
	// always-on state instead of a selector with a single invalid alternative.
	"kimi-k3": {
		Supported: true, AlwaysOn: true, Levels: []string{"max"}, Default: "max",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"hy3": {
		Supported: true, Adjustable: true, Levels: []string{"none", "low", "high"}, Default: "none",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"hy3-preview": {
		Supported: true, Adjustable: true, Levels: []string{"none", "low", "high"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	"hy4-preview": {
		Supported: true, Adjustable: true, Levels: []string{"none", "high"}, Default: "high",
		RequestFormat: ThinkingRequestReasoningEffort, ReplayFormat: ThinkingReplayReasoningContent,
	},
	// Responses-route models accept a reasoning object whose effort is the only
	// knob qcode exposes. Effort sets are model-specific and were verified
	// against the live endpoint on 2026-09-16. Muse Spark 1.3 lists "max" in
	// some upstream error text but rejects it, so it stays hidden.
	"gpt-5.6-luna": {
		Supported: true, Adjustable: true, Levels: []string{"none", "low", "medium", "high", "xhigh", "max"}, Default: "medium",
		RequestFormat: ThinkingRequestResponsesReasoning, ReplayFormat: ThinkingReplayReasoningDetails,
	},
	"grok-4.6": {
		Supported: true, Adjustable: true, Levels: []string{"minimal", "low", "medium", "high", "xhigh"},
		RequestFormat: ThinkingRequestResponsesReasoning, ReplayFormat: ThinkingReplayReasoningDetails,
	},
	"muse-spark-1.2-contributor": {
		Supported: true, Adjustable: true, Levels: []string{"minimal", "low", "medium", "high", "xhigh"}, Default: "high",
		RequestFormat: ThinkingRequestResponsesReasoning, ReplayFormat: ThinkingReplayReasoningDetails,
	},
	"muse-spark-1.3-contributor": {
		Supported: true, Adjustable: true, Levels: []string{"minimal", "low", "medium", "high", "xhigh"}, Default: "high",
		RequestFormat: ThinkingRequestResponsesReasoning, ReplayFormat: ThinkingReplayReasoningDetails,
	},
}

func (p *openAIProvider) ThinkingCapability(model string) ThinkingCapability {
	if p.name == "openai" {
		if p.baseURL != openAIBaseURL || p.modelMeta == nil || p.modelMeta.Provider != "openai" {
			return ThinkingCapability{RequestFormat: ThinkingRequestNone, ReplayFormat: ThinkingReplayNone}
		}
		options := p.modelMeta.Models[model]
		if len(options.Effort) == 0 {
			return ThinkingCapability{RequestFormat: ThinkingRequestNone, ReplayFormat: ThinkingReplayNone}
		}
		levels := append([]string(nil), options.Effort...)
		capability := ThinkingCapability{
			Supported:     true,
			Adjustable:    len(levels) > 1,
			AlwaysOn:      len(levels) == 1,
			Levels:        levels,
			RequestFormat: ThinkingRequestReasoningEffort,
			ReplayFormat:  ThinkingReplayNone,
		}
		if capability.AlwaysOn {
			capability.Default = levels[0]
		}
		return capability
	}
	if p.name != "opencode-go" || p.baseURL != openCodeGoBaseURL {
		return ThinkingCapability{RequestFormat: ThinkingRequestNone, ReplayFormat: ThinkingReplayNone}
	}
	capability, ok := openCodeGoThinkingCatalog[model]
	if !ok {
		return ThinkingCapability{RequestFormat: ThinkingRequestNone, ReplayFormat: ThinkingReplayNone}
	}
	if p.modelMeta != nil && p.modelMeta.Provider == "opencode-go" {
		capability = syncedOpenCodeGoCapability(capability, p.modelMeta.Models[model])
	}
	capability.Levels = append([]string(nil), capability.Levels...)
	return capability
}

func syncedOpenCodeGoCapability(adapter ThinkingCapability, options ProviderModelOptions) ThinkingCapability {
	choices := []string(nil)
	if adapter.RequestFormat == ThinkingRequestObject {
		if options.Toggle {
			choices = []string{"off", "on"}
		}
	} else {
		choices = append([]string(nil), options.Effort...)
	}
	if len(choices) == 0 {
		adapter.Levels = nil
		adapter.Default = ""
		adapter.Adjustable = false
		if adapter.AlwaysOn {
			// The model remains a fixed reasoning model, but no selector value is
			// inferred when Models.dev lists no caller-controlled option.
			adapter.Supported = true
		} else {
			adapter.Supported = false
			adapter.AlwaysOn = false
		}
		return adapter
	}
	adapter.Levels = choices
	adapter.Supported = true
	adapter.AlwaysOn = len(choices) == 1
	adapter.Adjustable = len(choices) > 1
	if containsChoice(choices, adapter.Default) {
		return adapter
	}
	if adapter.AlwaysOn {
		adapter.Default = choices[0]
	} else {
		adapter.Default = ""
	}
	return adapter
}

func containsChoice(choices []string, choice string) bool {
	for _, candidate := range choices {
		if candidate == choice {
			return true
		}
	}
	return false
}

func validThinkingLevel(capability ThinkingCapability, level string) bool {
	for _, candidate := range capability.Levels {
		if level == candidate {
			return true
		}
	}
	return false
}

// KnownThinkingLevels is the shared fallback set for Unknown capabilities and
// for providers without per-model metadata. Runtime /think and config
// validation must use ValidateThinkingLevel (which delegates to the Codex
// per-model validator when applicable) instead of maintaining a separate
// hardcoded list.
var KnownThinkingLevels = []string{"off", "on", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ValidateThinkingLevel checks an explicit level against any capability.
// Empty (auto) is always allowed. Codex capabilities use the per-model
// validator (Unknown allows plausible efforts optimistically); other
// providers validate strictly against Levels.
func ValidateThinkingLevel(cap ThinkingCapability, level string) error {
	norm := strings.ToLower(strings.TrimSpace(level))
	if norm == "" {
		return nil
	}
	if cap.RequestFormat == ThinkingRequestCodexReasoning {
		return ValidateCodexThinkingLevel(cap, level)
	}
	if cap.Unknown {
		for _, lv := range KnownThinkingLevels {
			if norm == lv {
				return nil
			}
		}
		return fmt.Errorf("unknown thinking level %q", level)
	}
	if !cap.Supported {
		return fmt.Errorf("model does not support thinking levels")
	}
	if cap.AlwaysOn {
		if len(cap.Levels) == 1 && norm == strings.ToLower(cap.Levels[0]) {
			return nil
		}
		if cap.Default != "" && norm == strings.ToLower(cap.Default) {
			return nil
		}
		return fmt.Errorf("thinking is always on for this model")
	}
	if !cap.Adjustable || len(cap.Levels) == 0 {
		return fmt.Errorf("thinking is not adjustable for this model")
	}
	for _, lv := range cap.Levels {
		if norm == strings.ToLower(lv) {
			return nil
		}
	}
	return fmt.Errorf("unsupported thinking level %q", level)
}

// thinkingFields resolves the single optional request encoding. Off is a
// thinking-object setting even for models that use reasoning_effort for their
// enabled tiers; this prevents sending mutually exclusive fields together.
func thinkingFields(capability ThinkingCapability, level string) map[string]any {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return nil
	}
	// Unknown Codex catalog: send plausible efforts optimistically so an
	// explicit level set before the first models list is not silently dropped.
	// Known validation still rejects typos and client-only efforts.
	if capability.Unknown && capability.RequestFormat == ThinkingRequestCodexReasoning {
		if isCodexClientOnlyEffort(level) || !isCodexPlausibleLevel(level) {
			return nil
		}
		return map[string]any{"reasoning": map[string]any{"effort": level}}
	}
	if !capability.Adjustable || !validThinkingLevel(capability, level) {
		return nil
	}
	if capability.RequestFormat == ThinkingRequestOllamaThink {
		if level == "off" {
			return map[string]any{"think": false}
		}
		return map[string]any{"think": level}
	}
	// Responses models pass the wire-native effort through directly, including
	// "none" when the model supports disabling reasoning. Request an automatic
	// summary for enabled reasoning so the Responses reasoning-summary stream is
	// available to qcode's thinking view.
	if capability.RequestFormat == ThinkingRequestResponsesReasoning {
		reasoning := map[string]any{"effort": level}
		if level != "none" {
			reasoning["summary"] = "auto"
		}
		return map[string]any{"reasoning": reasoning}
	}
	if capability.RequestFormat == ThinkingRequestCodexReasoning {
		// Codex contract (#23): effort only, summary omitted by default.
		// ultra/persistent/custom are client-only or upstream-mapped and must
		// never go on the wire, even if a custom catalog advertises them.
		switch level {
		case "ultra", "persistent", "custom":
			return nil
		}
		return map[string]any{"reasoning": map[string]any{"effort": level}}
	}
	if level == "off" {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}
	}
	if capability.RequestFormat == ThinkingRequestObject {
		return map[string]any{"thinking": map[string]any{"type": "enabled"}}
	}
	if capability.RequestFormat == ThinkingRequestReasoningEffort {
		return map[string]any{"reasoning_effort": level}
	}
	return nil
}
