package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

// chatSettings serializes settings changes with task admission for this chat.
func (r *Runtime) chatSettings(m platform.Message, fields []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("runtime closed")
	}
	if len(fields) == 1 {
		if fields[0] == "/provider" {
			if _, ok := r.Sink.(modelSink); ok {
				return r.showProviders(m)
			}
			names := []string{r.Config.Provider.Name}
			for name := range r.Config.Providers {
				names = append(names, name)
			}
			sort.Strings(names)
			return r.reply(m, "Configured providers: "+strings.Join(names, ", ")+"\nUse /provider <name> or /provider default.")
		}
		if fields[0] == "/max_steps" {
			selected, err := r.chatModel(m)
			if err != nil {
				return err
			}
			return r.reply(m, fmt.Sprintf("Maximum model steps: %d\nUse /max_steps <positive integer> or /max_steps default. Applies to this entire chat (all topics), including scheduled tasks. Timeout limits still apply.", selected.MaxSteps))
		}
		if fields[0] == "/think" {
			return r.reply(m, r.thinkLevelsLocked(m))
		}
		return r.reply(m, r.statusLocked(m))
	}
	if len(fields) != 2 {
		return r.reply(m, "Use /provider <name|default>, /model <id|default>, /think <level|auto|default>, /max_steps <number|default>, or /settings reset.")
	}
	if _, busy := r.active[m.WorkspaceKey()]; busy {
		return r.reply(m, "Chat is busy. Finish or cancel the active task first.")
	}
	raw, err := r.Store.ChatModel(m.WorkspaceKey())
	if err != nil {
		return err
	}
	before, beforeErr := r.chatModel(m)
	value := fields[1]
	switch fields[0] {
	case "/settings":
		if value != "reset" {
			return r.reply(m, "Use /settings or /settings reset.")
		}
		raw = state.ChatModel{}
	case "/max_steps":
		if value == "default" {
			raw.MaxSteps = 0
		} else {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return r.reply(m, "Use /max_steps <positive integer> or /max_steps default.")
			}
			raw.MaxSteps = n
		}
	case "/provider":
		name := value
		if name == "default" {
			name = r.Config.Provider.Name
		}
		if _, ok := r.Config.ProviderByName(name); !ok {
			return r.reply(m, "Provider is not configured. Use /provider to list configured providers.")
		}
		if raw.Provider != name || raw.ProviderPinned != (value != "default") {
			raw.Model = ""
		}
		raw.Provider, raw.ProviderPinned = name, value != "default"
	case "/model":
		if beforeErr != nil {
			return r.reply(m, "Provider is unavailable. Use /provider default or /settings reset.")
		}
		if len(value) > 256 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 || r.Options.Redaction.Text(redaction.Remote, value) != value {
			return r.reply(m, "Invalid model ID.")
		}
		raw.Provider = before.Provider
		raw.Model = value
		if value == "default" {
			raw.Model = ""
		}
		model := raw.Model
		if model == "" {
			p, _ := r.Config.ProviderByName(before.Provider)
			model = p.Model
		}
		provider, err := r.Providers(state.Conversation{Provider: before.Provider, Model: model})
		if err != nil {
			r.report(err)
			return r.reply(m, "Unable to configure this provider.")
		}
		if validator, ok := provider.(llm.ModelValidator); ok && validator.ValidateModel(model) != nil {
			return r.reply(m, "This model is not supported by the provider adapter.")
		}
	case "/think":
		if value == "default" {
			raw.Thinking = nil
			break
		}
		if value == "auto" {
			value = ""
		} else {
			value = strings.ToLower(strings.TrimSpace(value))
			if beforeErr != nil {
				return r.reply(m, "Provider is unavailable. Use /provider default or /settings reset.")
			}
			provider, err := r.Providers(state.Conversation{Provider: before.Provider, Model: before.Model})
			if err != nil {
				r.report(err)
				return r.reply(m, "Unable to configure this provider.")
			}
			if tp, ok := provider.(llm.ThinkingProvider); ok {
				cap := tp.ThinkingCapability(before.Model)
				if err := llm.ValidateThinkingLevel(cap, value); err != nil {
					if cap.Unknown {
						return r.reply(m, err.Error()+". Catalog not loaded yet; plausible levels ("+strings.Join(llm.CodexPlausibleLevels(), ", ")+") are accepted optimistically. Use auto for the provider default.")
					}
					if len(cap.Levels) > 0 && cap.Supported {
						return r.reply(m, "Supported thinking levels for this model: "+strings.Join(cap.Levels, ", ")+". Use auto for the provider default.")
					}
					return r.reply(m, err.Error()+". Use auto for the provider default or /think default to restore the configured default.")
				}
			} else {
				valid := false
				for _, lv := range llm.KnownThinkingLevels {
					if value == lv {
						valid = true
						break
					}
				}
				if !valid {
					return r.reply(m, "Unknown thinking level. Use "+strings.Join(llm.KnownThinkingLevels, ", ")+", auto, or default.")
				}
			}
		}
		raw.Thinking = &value
	}
	// A new routing revision prevents A -> B -> A from resurrecting topic history.
	// Repeating a choice or changing thinking alone retains history.
	after, err := r.resolveChatModel(raw)
	if err != nil {
		return err
	}
	if beforeErr != nil || before.Provider != after.Provider || before.Model != after.Model {
		var token [8]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		raw.Revision = hex.EncodeToString(token[:])
	} else {
		raw.Revision = before.Revision
	}

	if err := r.Store.SetChatModel(m.WorkspaceKey(), raw); err != nil {
		return err
	}
	for token, menu := range r.modelMenus {
		if menu.Route.WorkspaceKey() == m.WorkspaceKey() {
			delete(r.modelMenus, token)
		}
	}
	for token, menu := range r.providerMenus {
		if menu.Route.WorkspaceKey() == m.WorkspaceKey() {
			delete(r.providerMenus, token)
		}
	}
	return r.reply(m, "Settings saved for this entire chat (all topics).\n"+r.statusLocked(m))
}

// thinkLevelsLocked describes the thinking policy for this chat's current
// provider and model, using the same per-model ThinkingCapability the
// providers use at request time (qcode's catalog plus Models.dev sync).
// It powers bare `/think`, so users see the valid selector values for
// their model instead of the full status dump.
//
// Semantics (issue #25):
//   - `/think auto` (empty string) omits the effort field; the backend
//     decides. It never claims equality with a concrete level.
//   - `/think default` clears the chat override (raw nil) and restores the
//     configured default, which may itself be explicit (e.g. high).
//   - Codex `default_reasoning_level` is what the Codex CLI sends when unset;
//     q-agent omits effort when auto, which is a different behavior. The UI
//     shows both lines separately and never guesses the effective effort
//     from style/latency/token counts.
func (r *Runtime) thinkLevelsLocked(m platform.Message) string {
	selected, err := r.chatModel(m)
	if err != nil {
		return "Provider is unavailable. Use /provider default or /settings reset."
	}
	current := ""
	if selected.Thinking != nil {
		current = *selected.Thinking
	}
	currentLabel := "`auto` (unspecified; backend decides)"
	if current != "" {
		currentLabel = "`" + current + "`"
	}
	// Chat override vs configured default. resolveChatModel fills Thinking
	// from config when raw is nil, so read the raw record to tell
	// "explicit auto" apart from "inherited default".
	overrideLabel := "none (using configured default)"
	if raw, rawErr := r.Store.ChatModel(m.WorkspaceKey()); rawErr == nil && raw.Thinking != nil {
		if *raw.Thinking == "" {
			overrideLabel = "`auto` (explicit, omits effort)"
		} else {
			overrideLabel = "`" + *raw.Thinking + "` (chat override)"
		}
	}
	configuredDefault := r.Config.Provider.Thinking
	if pCfg, ok := r.Config.ProviderByName(selected.Provider); ok {
		// resolveChatModel fallback: default provider thinking unless the
		// selected provider sets its own.
		if selected.Provider == r.Config.Provider.Name {
			configuredDefault = pCfg.Thinking
		} else if pCfg.Thinking != "" {
			configuredDefault = pCfg.Thinking
		} else {
			configuredDefault = r.Config.Provider.Thinking
		}
	}
	configuredLabel := "`auto` (unspecified; backend decides)"
	if configuredDefault != "" {
		configuredLabel = "`" + configuredDefault + "`"
	}
	provider, err := r.Providers(state.Conversation{Provider: selected.Provider, Model: selected.Model})
	if err != nil {
		r.report(err)
		return "Unable to load thinking levels. Try /think again later."
	}
	capability, ok := provider.(llm.ThinkingProvider)
	if !ok {
		return "Thinking levels are not available from this provider.\nCurrent: " + currentLabel + "."
	}
	cap := capability.ThinkingCapability(selected.Model)
	head := "**Thinking**\n\nModel: `" + selected.Model + "` (" + selected.Provider + ")\nCurrent: " + currentLabel + "\nChat override: " + overrideLabel + "\nConfigured default: " + configuredLabel
	isCodex := cap.RequestFormat == llm.ThinkingRequestCodexReasoning
	catalogStatus := "Catalog: not loaded yet"
	if cap.Source != "" {
		catalogStatus = "Catalog: " + cap.Source
		if cap.Stale {
			catalogStatus += " (stale, last-known-good)"
		} else {
			catalogStatus += " (fresh)"
		}
	}
	if cap.Unknown {
		text := head + "\n" + catalogStatus
		text += "\n\nThinking levels have not been loaded yet for this model. Plausible levels (" + strings.Join(llm.CodexPlausibleLevels(), ", ") + ") are accepted optimistically; run `/models` to refresh. Use `/think <level|auto|default>` to change it. `auto` (unspecified; backend decides, omits effort) uses the provider default. `/think default` clears the chat override and restores the configured default."
		if isCodex {
			text += "\nCodex catalog default: unknown (catalog not loaded yet; q-agent omits effort when auto, Codex CLI sends its default explicitly)."
		}
		return text
	}
	if !cap.Supported {
		return head + "\n" + catalogStatus + "\n\nThis model does not support thinking levels. `/think` has no effect for it."
	}
	// Codex never reports AlwaysOn (a single advertised effort does not prove
	// the backend's behavior when effort is omitted, and ["none"] has the
	// opposite semantics). Keep the always-on branch for other providers.
	if cap.AlwaysOn && !isCodex {
		level := cap.Default
		if level == "" && len(cap.Levels) > 0 {
			level = cap.Levels[0]
		}
		if level == "" {
			level = current
		}
		text := head + "\n" + catalogStatus
		if level != "" {
			text += "\n\nThinking is always on for this model (`" + level + "`). `/think` is not adjustable."
		} else {
			text += "\n\nThinking is always on for this model. `/think` is not adjustable."
		}
		return text
	}
	// Single-level Codex: not a selector, but an explicit choice is still sent
	// (auto omits). Never report always-on.
	if isCodex && len(cap.Levels) == 1 {
		only := cap.Levels[0]
		mark := ""
		if only == current {
			mark = " ✓"
		}
		text := head + "\nSupported level: `" + only + "`" + mark + " (only option; explicit selection is sent, auto omits effort)"
		if cap.Default != "" {
			text += "\nCodex catalog default: `" + cap.Default + "` (Codex CLI sends this explicitly when unset; q-agent omits effort when auto)"
		} else {
			text += "\nCodex catalog default: none advertised"
		}
		text += "\nSummary: omitted (server default; effort and summary are independent)"
		text += "\n" + catalogStatus
		return text + "\n\nUse `/think <level|auto|default>` to change it. `auto` (unspecified; backend decides, omits effort) uses the provider default. `/think default` clears the chat override and restores the configured default."
	}
	if len(cap.Levels) == 0 || !cap.Adjustable {
		return head + "\n" + catalogStatus + "\n\nThinking is not adjustable for this model.\n\nUse `/think auto` (unspecified; backend decides, omits effort) for the provider default or `/think default` to restore the configured default."
	}
	levels := make([]string, len(cap.Levels))
	for i, level := range cap.Levels {
		levels[i] = "`" + level + "`"
		if level == current {
			levels[i] += " ✓"
		}
	}
	text := head + "\nSupported levels: " + strings.Join(levels, ", ")
	if isCodex {
		if cap.Default != "" {
			text += "\nCodex catalog default: `" + cap.Default + "` (Codex CLI sends this explicitly when unset; q-agent omits effort when auto)"
		} else {
			text += "\nCodex catalog default: none advertised"
		}
		text += "\nSummary: omitted (server default; effort and summary are independent)"
		text += "\n" + catalogStatus
	} else if cap.Default != "" {
		text += "\nModel default: `" + cap.Default + "`"
	}
	return text + "\n\nUse `/think <level|auto|default>` to change it. `auto` (unspecified; backend decides, omits effort) uses the provider default. `/think default` clears the chat override and restores the configured default."
}
