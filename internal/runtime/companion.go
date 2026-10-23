package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
)

const companionMenuLifetime = 15 * time.Minute

type companionMenu struct {
	Route   platform.Message
	Expires time.Time
}

// companionCommand implements /companion with text args and an interactive
// menu. Caller must not hold r.mu; it locks like chatSettings.
func (r *Runtime) companionCommand(m platform.Message, fields []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("runtime closed")
	}
	if len(fields) == 1 {
		return r.showCompanionLocked(m)
	}
	if len(fields) == 2 && fields[1] == "status" {
		pref := r.companionPrefLocked(m)
		effective := state.CompanionEffective(r.Config.Proactive.Enabled, pref)
		return r.reply(m, companionStatusText(r.Config.Proactive.Enabled, pref, effective))
	}
	if len(fields) == 2 {
		arg := strings.ToLower(strings.TrimSpace(fields[1]))
		var mode string
		switch arg {
		case "on", "enable", "enabled":
			mode = "on"
		case "off", "disable", "disabled":
			mode = "off"
		case "default", "auto", "reset":
			mode = "default"
		default:
			return r.reply(m, "Use `/companion`, `/companion status`, `/companion on|off|default`.")
		}
		if _, busy := r.active[m.WorkspaceKey()]; busy {
			return r.reply(m, "Chat is busy. Finish or cancel the active task first.")
		}
		if err := r.Store.SetCompanion(m.WorkspaceKey(), state.CompanionPref{Mode: mode}); err != nil {
			return err
		}
		r.invalidateCompanionMenusLocked(m.WorkspaceKey())
		pref := state.CompanionPref{Mode: mode}
		effective := state.CompanionEffective(r.Config.Proactive.Enabled, pref)
		return r.reply(m, "Companion settings saved for this entire chat (all topics).\n"+companionStatusText(r.Config.Proactive.Enabled, pref, effective))
	}
	return r.reply(m, "Use `/companion`, `/companion status`, `/companion on|off|default`.")
}

func (r *Runtime) companionPrefLocked(m platform.Message) state.CompanionPref {
	pref, err := r.Store.GetCompanion(m.WorkspaceKey())
	if err != nil {
		r.report(err)
		return state.CompanionPref{Mode: "default"}
	}
	if pref.Mode == "" {
		pref.Mode = "default"
	}
	return pref
}

func companionStatusText(globalEnabled bool, pref state.CompanionPref, effective bool) string {
	mode := pref.Mode
	if mode == "" {
		mode = "default"
	}
	stateLabel := "off"
	if effective {
		stateLabel = "on"
	}
	source := "global setting"
	if mode != "default" {
		source = "this chat overrides global"
	}
	globalLabel := "off"
	if globalEnabled {
		globalLabel = "on"
	}
	return fmt.Sprintf("Companion: %s (%s; global %s, this chat %s).\nUse `/companion` to change it. Applies to this entire chat (all topics).", stateLabel, source, globalLabel, mode)
}

func (r *Runtime) showCompanionLocked(m platform.Message) error {
	if _, ok := r.Sink.(modelSink); !ok {
		pref := r.companionPrefLocked(m)
		effective := state.CompanionEffective(r.Config.Proactive.Enabled, pref)
		return r.reply(m, companionStatusText(r.Config.Proactive.Enabled, pref, effective)+"\nInteractive buttons are unavailable on this adapter. Use `/companion on|off|default`.")
	}
	if r.closed {
		return context.Canceled
	}
	var tokenBytes [8]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes[:])
	menu := companionMenu{Route: m, Expires: time.Now().Add(companionMenuLifetime)}
	if r.companionMenus == nil {
		r.companionMenus = map[string]companionMenu{}
	}
	for key, old := range r.companionMenus {
		if time.Now().After(old.Expires) || old.Route.WorkspaceKey() == m.WorkspaceKey() && old.Route.UserID == m.UserID && old.Route.TopicID == m.TopicID {
			delete(r.companionMenus, key)
		}
	}
	if len(r.companionMenus) >= 128 {
		return r.reply(m, "Too many open companion menus. Try again later.")
	}
	r.companionMenus[token] = menu
	pref := r.companionPrefLocked(m)
	effective := state.CompanionEffective(r.Config.Proactive.Enabled, pref)
	text, keyboard := companionPage(token, pref, r.Config.Proactive.Enabled, effective)
	return r.Store.QueueKeyboard(m, []string{text}, keyboard)
}

func companionPage(token string, pref state.CompanionPref, globalEnabled, effective bool) (string, *platform.InlineKeyboard) {
	mode := pref.Mode
	if mode == "" {
		mode = "default"
	}
	current := "off"
	if effective {
		current = "on"
	}
	globalLabel := "off"
	if globalEnabled {
		globalLabel = "on"
	}
	raw := fmt.Sprintf("Companion check-ins\nCurrent: %s (global %s, this chat %s)\n\nQuiet by default: a few daily sweeps may send at most 1 short message. Silence is the common case. Applies to this entire chat (all topics). Menus expire after 15 minutes.", current, globalLabel, mode)
	text := raw
	keyboard := &platform.InlineKeyboard{
		Rows: [][]platform.InlineButton{
			{
				{Text: "Enable ✓", Data: fmt.Sprintf("companion:%s:s:%d", token, 0)},
				{Text: "Disable", Data: fmt.Sprintf("companion:%s:s:%d", token, 1)},
			},
			{
				{Text: fmt.Sprintf("Use default (now %s)", globalLabel), Data: fmt.Sprintf("companion:%s:s:%d", token, 2)},
			},
		},
	}
	_ = text
	// Format for Telegram markdown; keep plain to avoid parse errors.
	return text, keyboard
}

func (r *Runtime) invalidateCompanionMenusLocked(workspaceKey string) {
	for token, old := range r.companionMenus {
		if old.Route.WorkspaceKey() == workspaceKey {
			delete(r.companionMenus, token)
		}
	}
}

func companionStatusLine(r *Runtime, m platform.Message) string {
	pref, err := r.Store.GetCompanion(m.WorkspaceKey())
	if err != nil {
		pref = state.CompanionPref{Mode: "default"}
	}
	if pref.Mode == "" {
		pref.Mode = "default"
	}
	effective := state.CompanionEffective(r.Config.Proactive.Enabled, pref)
	label := "off"
	if effective {
		label = "on"
	}
	source := "global"
	if pref.Mode != "default" {
		source = "this chat"
	}
	return "\nCompanion: " + label + " (" + source + " " + pref.Mode + ")"
}

func (r *Runtime) handleCompanionCallback(ctx context.Context, m platform.Message, parts []string) error {
	sink, ok := r.Sink.(modelSink)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	answer := "This menu expired. Send /companion again."
	var editText string
	var editKeyboard *platform.InlineKeyboard
	defer func() {
		r.report(sink.AnswerCallback(ctx, m.CallbackID, answer))
		if editText != "" {
			r.report(sink.EditModels(ctx, m, editText, editKeyboard))
		}
	}()
	if len(parts) != 4 || parts[0] != "companion" || parts[2] != "s" {
		return nil
	}
	var idx int
	switch parts[3] {
	case "0":
		idx = 0
	case "1":
		idx = 1
	case "2":
		idx = 2
	default:
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return context.Canceled
	}
	menu, ok := r.companionMenus[parts[1]]
	if !ok || time.Now().After(menu.Expires) {
		return nil
	}
	if menu.Route.ChatID != m.ChatID || menu.Route.TopicID != m.TopicID || menu.Route.UserID != m.UserID {
		answer = "Open your own menu with /companion."
		return nil
	}
	if _, busy := r.active[m.WorkspaceKey()]; busy {
		answer = "Chat is busy. Finish or cancel the active task first."
		return nil
	}
	modes := []string{"on", "off", "default"}
	mode := modes[idx]
	if err := r.Store.SetCompanion(m.WorkspaceKey(), state.CompanionPref{Mode: mode}); err != nil {
		answer = "Unable to save companion settings."
		return err
	}
	for token, old := range r.companionMenus {
		if old.Route.WorkspaceKey() == m.WorkspaceKey() {
			delete(r.companionMenus, token)
		}
	}
	pref := state.CompanionPref{Mode: mode}
	effective := state.CompanionEffective(r.Config.Proactive.Enabled, pref)
	answer = "Companion updated."
	text := "Companion settings saved for this entire chat (all topics).\n" + companionStatusText(r.Config.Proactive.Enabled, pref, effective)
	if err := r.reply(m, text); err != nil {
		return err
	}
	editText, editKeyboard = text, &platform.InlineKeyboard{Rows: [][]platform.InlineButton{}}
	return nil
}

func helpText() string {
	return "**q-agent commands**\n`/update` — Install the latest stable Release and restart (private admin only).\n`/restart` — Restart the systemd user service (private admin only).\n`/status` — Show current runtime, process RSS memory, and safe settings (no model call).\n`/models` — Choose a model for this chat.\n`/provider` — Show providers or choose this chat's provider.\n`/model` — Set this chat's model.\n`/think` — Set this chat's thinking.\n`/max_steps` — Show or set this chat's model-step limit.\n`/settings` — Show settings or restore defaults.\n`/companion` — Enable or disable quiet check-ins for this chat (`/companion on|off|default|status`).\n`/memory` — Show this chat's long-term memory.\n`/forget` — Clear notes and pending memory sources.\n`/help` — Show this help.\n`/new` — Clear this topic's conversation; keep workspace.\n`/cancel` — Cancel this topic's active task.\n\nSend a message to start a task. Send follow-up text in the same topic to steer your running task. `/cancel` stops the task and pending follow-ups.\n\nIn groups, address commands to @your_bot (e.g. `/status@your_bot`), mention the bot, or reply to it."
}
