package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

const modelsPerPage = 8
const modelMenuLifetime = 15 * time.Minute

type modelMenu struct {
	Route    platform.Message
	Models   []string
	Provider string
	Expires  time.Time
}

type modelSink interface {
	EditModels(context.Context, platform.Message, string, *platform.InlineKeyboard) error
	AnswerCallback(context.Context, string, string) error
}

func (r *Runtime) chatModel(m platform.Message) (state.ChatModel, error) {
	selected, err := r.Store.ChatModel(m.WorkspaceKey())
	if err != nil {
		return selected, err
	}
	return r.resolveChatModel(selected)
}

func (r *Runtime) resolveChatModel(selected state.ChatModel) (state.ChatModel, error) {
	if !selected.ProviderPinned && selected.Provider != r.Config.Provider.Name {
		selected.Provider = r.Config.Provider.Name
		selected.Model = ""
	}
	provider, ok := r.Config.ProviderByName(selected.Provider)
	if !ok {
		return selected, fmt.Errorf("chat provider %q is no longer configured; use /provider default", selected.Provider)
	}
	if selected.Model == "" {
		selected.Model = provider.Model
	}
	if selected.Thinking == nil {
		thinking := r.Config.Provider.Thinking
		if provider.Thinking != "" {
			thinking = provider.Thinking
		}
		selected.Thinking = &thinking
	}
	return selected, nil
}

func (r *Runtime) showModels(ctx context.Context, m platform.Message) error {
	if _, ok := r.Sink.(modelSink); !ok {
		return r.reply(m, "Model buttons are unavailable on this adapter.")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	selected, err := r.chatModel(m)
	if err != nil {
		return err
	}
	provider, err := r.Providers(state.Conversation{Provider: selected.Provider, Model: selected.Model})
	if err != nil {
		r.report(err)
		return r.reply(m, "Unable to load models. Try /models again later.")
	}
	lister, ok := provider.(llm.ModelLister)
	if !ok {
		return r.reply(m, "This provider does not support model discovery.")
	}
	models, err := lister.Models(ctx)
	if err != nil {
		r.report(err)
		return r.reply(m, "Unable to load models. Try /models again later.")
	}
	unique := map[string]bool{}
	for _, model := range models {
		if model == "" || len(model) > 256 || !utf8.ValidString(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 || strings.TrimSpace(model) != model {
			continue
		}
		if r.Options.Redaction.Text(redaction.Remote, model) != model {
			continue
		}
		if validator, ok := provider.(llm.ModelValidator); ok && validator.ValidateModel(model) != nil {
			continue
		}
		unique[model] = true
	}
	// The currently configured model remains visible even if discovery omits it.
	if selected.Model != "" && len(selected.Model) <= 256 && strings.IndexFunc(selected.Model, unicode.IsControl) < 0 && r.Options.Redaction.Text(redaction.Remote, selected.Model) == selected.Model {
		unique[selected.Model] = true
	}
	models = nil
	for model := range unique {
		models = append(models, model)
	}
	sort.Strings(models)
	if len(models) == 0 {
		return r.reply(m, "No models are available from this provider.")
	}
	if len(models) > 2000 {
		return r.reply(m, "Provider returned too many models to display.")
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(random[:])
	menu := modelMenu{Route: m, Models: models, Provider: selected.Provider, Expires: time.Now().Add(modelMenuLifetime)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return context.Canceled
	}
	if r.modelMenus == nil {
		r.modelMenus = map[string]modelMenu{}
	}
	for key, old := range r.modelMenus {
		if time.Now().After(old.Expires) || old.Route.WorkspaceKey() == m.WorkspaceKey() && old.Route.UserID == m.UserID && old.Route.TopicID == m.TopicID {
			delete(r.modelMenus, key)
		}
	}
	if len(r.modelMenus) >= 128 {
		return r.reply(m, "Too many open model menus. Try again later.")
	}
	r.modelMenus[token] = menu
	text, keyboard := modelPage(token, menu, selected.Model, 0)
	return r.Store.QueueKeyboard(m, []string{text}, keyboard)
}

func modelPage(token string, menu modelMenu, current string, page int) (string, *platform.InlineKeyboard) {
	pages := (len(menu.Models) + modelsPerPage - 1) / modelsPerPage
	keyboard := &platform.InlineKeyboard{}
	for i := page * modelsPerPage; i < len(menu.Models) && i < (page+1)*modelsPerPage; i++ {
		label := menu.Models[i]
		if label == current {
			label = "✓ " + label
		}
		runes := []rune(label)
		if len(runes) > 60 {
			label = string(runes[:59]) + "…"
		}
		keyboard.Rows = append(keyboard.Rows, []platform.InlineButton{{Text: label, Data: fmt.Sprintf("models:%s:s:%d", token, i)}})
	}
	var nav []platform.InlineButton
	if page > 0 {
		nav = append(nav, platform.InlineButton{Text: "‹ Previous", Data: fmt.Sprintf("models:%s:p:%d", token, page-1)})
	}
	if page+1 < pages {
		nav = append(nav, platform.InlineButton{Text: "Next ›", Data: fmt.Sprintf("models:%s:p:%d", token, page+1)})
	}
	if len(nav) > 0 {
		keyboard.Rows = append(keyboard.Rows, nav)
	}
	raw := fmt.Sprintf("Choose a model · %s\nCurrent: %s\nPage %d/%d\n\nApplies to this entire chat (all topics). Switching starts fresh model history. Menus expire after 15 minutes.", menu.Provider, current, page+1, pages)
	return telegram.Format(raw), keyboard
}

func (r *Runtime) handleModelCallback(ctx context.Context, m platform.Message) error {
	sink, ok := r.Sink.(modelSink)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	answer := "This menu expired. Send /models again."
	var editText string
	var editKeyboard *platform.InlineKeyboard
	defer func() {
		r.report(sink.AnswerCallback(ctx, m.CallbackID, answer))
		if editText != "" {
			r.report(sink.EditModels(ctx, m, editText, editKeyboard))
		}
	}()
	parts := strings.Split(m.CallbackData, ":")
	if len(parts) != 4 || parts[0] != "models" {
		return nil
	}
	index, err := strconv.Atoi(parts[3])
	if err != nil || index < 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return context.Canceled
	}
	menu, ok := r.modelMenus[parts[1]]
	if !ok || time.Now().After(menu.Expires) {
		return nil
	}
	if menu.Route.ChatID != m.ChatID || menu.Route.TopicID != m.TopicID || menu.Route.UserID != m.UserID {
		answer = "Open your own menu with /models."
		return nil
	}
	selected, err := r.chatModel(m)
	if err != nil {
		answer = "Unable to load chat settings."
		return err
	}
	if menu.Provider != selected.Provider {
		return nil
	}

	switch parts[2] {
	case "p":
		if index >= (len(menu.Models)+modelsPerPage-1)/modelsPerPage {
			return nil
		}
		text, keyboard := modelPage(parts[1], menu, selected.Model, index)
		answer = ""
		editText, editKeyboard = text, keyboard
	case "s":
		if index >= len(menu.Models) {
			return nil
		}
		if _, busy := r.active[m.WorkspaceKey()]; busy {
			answer = "Chat is busy. Finish or cancel the active task first."
			return nil
		}
		if selected.Model != menu.Models[index] {
			raw, err := r.Store.ChatModel(m.WorkspaceKey())
			if err != nil {
				return err
			}
			raw.Provider, raw.Model, raw.Revision = menu.Provider, menu.Models[index], parts[1]
			if err = r.Store.SetChatModel(m.WorkspaceKey(), raw); err != nil {
				answer = "Unable to save model settings."
				return err
			}
		}
		// Invalidate all old selectors for this chat; no stale menu can undo a choice.
		for token, old := range r.modelMenus {
			if old.Route.WorkspaceKey() == m.WorkspaceKey() {
				delete(r.modelMenus, token)
			}
		}
		answer = "Model selected."
		text := "Chat model: " + menu.Models[index] + "\nApplies to all topics in this chat. The next task will use this model."
		if err = r.reply(m, text); err != nil {
			return err
		}
		editText, editKeyboard = telegram.Format(text), &platform.InlineKeyboard{Rows: [][]platform.InlineButton{}}
	}
	return nil
}

// Resolve routing before building a provider, for Telegram and scheduled runs.
func (r *Runtime) loadModelConversation(m platform.Message) (state.Conversation, error) {
	conversation, err := r.Store.Load(m.ConversationKey())
	if err != nil {
		return conversation, err
	}
	selection, err := r.chatModel(m)
	if err != nil {
		return conversation, fmt.Errorf("load chat model settings: %w", err)
	}
	if conversation.Provider != selection.Provider || conversation.Model != selection.Model || conversation.ModelSelection != selection.Revision {
		conversation = state.Conversation{Provider: selection.Provider, Model: selection.Model, ModelSelection: selection.Revision}
	}
	conversation.Thinking = *selection.Thinking
	return conversation, nil
}
