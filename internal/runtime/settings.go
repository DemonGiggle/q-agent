package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
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
			names := []string{r.Config.Provider.Name}
			for name := range r.Config.Providers {
				names = append(names, name)
			}
			sort.Strings(names)
			return r.reply(m, "Configured providers: "+strings.Join(names, ", ")+"\nUse /provider <name> or /provider default.")
		}
		if fields[0] == "/think" {
			return r.reply(m, r.thinkLevelsLocked(m))
		}
		return r.reply(m, r.statusLocked(m))
	}
	if len(fields) != 2 {
		return r.reply(m, "Use /provider <name|default>, /model <id|default>, /think <level|auto|default>, or /settings reset.")
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
			switch value {
			case "off", "on", "none", "minimal", "low", "medium", "high", "xhigh", "max":
			default:
				return r.reply(m, "Unknown thinking level. Use off, on, none, minimal, low, medium, high, xhigh, max, auto, or default.")
			}
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
				if len(cap.Levels) > 0 {
					valid := false
					for _, level := range cap.Levels {
						valid = valid || level == value
					}
					if !valid {
						return r.reply(m, "Supported thinking levels for this model: "+strings.Join(cap.Levels, ", ")+". Use auto for the provider default.")
					}
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
	return r.reply(m, "Settings saved for this entire chat (all topics).\n"+r.statusLocked(m))
}

// thinkLevelsLocked describes the thinking policy for this chat's current
// provider and model, using the same per-model ThinkingCapability the
// providers use at request time (qcode's catalog plus Models.dev sync).
// It powers bare `/think`, so users see the valid selector values for
// their model instead of the full status dump.
func (r *Runtime) thinkLevelsLocked(m platform.Message) string {
	selected, err := r.chatModel(m)
	if err != nil {
		return "Provider is unavailable. Use /provider default or /settings reset."
	}
	current := ""
	if selected.Thinking != nil {
		current = *selected.Thinking
	}
	currentLabel := "`auto` (provider default)"
	if current != "" {
		currentLabel = "`" + current + "`"
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
	head := "**Thinking**\n\nModel: `" + selected.Model + "` (" + selected.Provider + ")\nCurrent: " + currentLabel
	if !cap.Supported {
		return head + "\n\nThis model does not support thinking levels. `/think` has no effect for it."
	}
	if cap.AlwaysOn {
		level := cap.Default
		if level == "" && len(cap.Levels) > 0 {
			level = cap.Levels[0]
		}
		if level == "" {
			level = current
		}
		if level != "" {
			return head + "\n\nThinking is always on for this model (`" + level + "`). `/think` is not adjustable."
		}
		return head + "\n\nThinking is always on for this model. `/think` is not adjustable."
	}
	if len(cap.Levels) == 0 || !cap.Adjustable {
		return head + "\n\nThinking is not adjustable for this model.\n\nUse `/think auto` for the provider default or `/think default` to restore the configured default."
	}
	levels := make([]string, len(cap.Levels))
	for i, level := range cap.Levels {
		levels[i] = "`" + level + "`"
		if level == current {
			levels[i] += " ✓"
		}
	}
	text := head + "\nSupported levels: " + strings.Join(levels, ", ")
	if cap.Default != "" {
		text += "\nModel default: `" + cap.Default + "`"
	}
	return text + "\n\nUse `/think <level|auto|default>` to change it. `auto` uses the provider default."
}
