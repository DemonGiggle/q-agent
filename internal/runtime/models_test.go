package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/state"
)

type pickerProvider struct {
	*testProvider
	models []string
	fail   bool
}

func (p *pickerProvider) Models(context.Context) ([]string, error) {
	if p.fail {
		return nil, fmt.Errorf("secret provider error")
	}
	return p.models, nil
}

type pickerSink struct {
	text     string
	keyboard *platform.InlineKeyboard
	answer   string
	edits    int
}

func (*pickerSink) Typing(context.Context, platform.Message) error { return nil }
func (s *pickerSink) EditModels(_ context.Context, _ platform.Message, text string, k *platform.InlineKeyboard) error {
	s.text = text
	s.keyboard = k
	s.edits++
	return nil
}
func (s *pickerSink) AnswerCallback(_ context.Context, _ string, text string) error {
	s.answer = text
	return nil
}

func pickerFixture(t *testing.T) (*Runtime, *pickerSink, *pickerProvider) {
	t.Helper()
	r, _, base := fixture(t)
	p := &pickerProvider{testProvider: base}
	for i := 0; i < 19; i++ {
		p.models = append(p.models, fmt.Sprintf("model-%02d", i))
	}
	p.models = append(p.models, "", "model-00", "bad\nmodel", strings.Repeat("long-model-", 12))
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	s := &pickerSink{}
	r.Sink = s
	return r, s, p
}
func openPicker(t *testing.T, r *Runtime, id, chat, topic int64) (string, state.Reply) {
	t.Helper()
	// Drain previous replies as the delivery worker would.
	for old, ok := r.Store.Pending(); ok; old, ok = r.Store.Pending() {
		if err := r.Store.Delivered(old.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Handle(context.Background(), input(id, chat, topic, 1, "/models")); err != nil {
		t.Fatal(err)
	}
	reply, ok := r.Store.Pending()
	if !ok || reply.Keyboard == nil {
		t.Fatalf("no keyboard: %+v", reply)
	}
	token := strings.Split(reply.Keyboard.Rows[0][0].Data, ":")[1]
	return token, reply
}
func callback(t *testing.T, r *Runtime, id, chat, topic, user int64, data string) {
	t.Helper()
	m := input(id, chat, topic, user, "")
	m.CallbackID = fmt.Sprint(id)
	m.CallbackData = data
	m.MessageID = 100
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}
func TestModelPickerPaginationAndSelection(t *testing.T) {
	r, s, p := pickerFixture(t)
	token, reply := openPicker(t, r, 1, 10, 7)
	if !strings.Contains(reply.Parts[0], "Page 1/3") || len(reply.Keyboard.Rows) != 9 {
		t.Fatalf("first page: %+v", reply)
	}
	for _, row := range reply.Keyboard.Rows {
		for _, b := range row {
			if len(b.Data) > 64 {
				t.Fatal("oversize callback")
			}
		}
	}
	next := reply.Keyboard.Rows[8][0].Data
	callback(t, r, 2, 10, 7, 1, next)
	if !strings.Contains(s.text, "Page 2/3") || len(s.keyboard.Rows[8]) != 2 {
		t.Fatalf("second page: %s", s.text)
	}
	callback(t, r, 3, 10, 7, 1, "models:"+token+":p:2")
	if !strings.Contains(s.text, "Page 3/3") || s.keyboard.Rows[len(s.keyboard.Rows)-1][0].Text != "‹ Previous" {
		t.Fatal("last page")
	}
	callback(t, r, 4, 10, 7, 1, "models:"+token+":p:0")
	// Pick a real model via its opaque index, not a raw model name.
	selected := "model-05"
	index := -1
	for i, name := range r.modelMenus[token].Models {
		if name == selected {
			index = i
		}
	}
	callback(t, r, 5, 10, 7, 1, fmt.Sprintf("models:%s:s:%d", token, index))
	choice, err := r.Store.ChatModel(input(0, 10, 0, 1, "").WorkspaceKey())
	if err != nil || choice.Model != selected || choice.Revision == "" {
		t.Fatalf("choice: %+v %v", choice, err)
	}
	if len(s.keyboard.Rows) != 0 || s.answer != "Model selected." {
		t.Fatal("selection did not close menu")
	}
	// New prompts in another topic use the chat model; another chat keeps defaults.
	for i, chat := range []int64{10, 20} {
		if err := r.Handle(context.Background(), input(int64(6+i), chat, 8, 1, "hello")); err != nil {
			t.Fatal(err)
		}
		r.Wait()
	}
	if len(p.requests) != 2 || p.requests[0].Model != selected || p.requests[1].Model != r.Config.Provider.Model {
		t.Fatalf("routing: %+v", p.requests)
	}
	if !strings.Contains(r.statusLocked(input(0, 10, 8, 1, "")), "Model: "+selected) {
		t.Fatal("status did not follow selection")
	}
	// /new retains selection, including after reopening durable state.
	if err := r.Handle(context.Background(), input(8, 10, 8, 1, "/new")); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(r.Config.StateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Store = reopened
	if got, _ := r.chatModel(input(0, 10, 9, 1, "")); got.Model != selected {
		t.Fatal("selection lost on restart/new")
	}
	// Old selectors are consumed and cannot revert a selection.
	callback(t, r, 9, 10, 7, 1, "models:"+token+":s:0")
	if !strings.Contains(s.answer, "expired") {
		t.Fatal("stale menu accepted")
	}
}
func TestModelPickerAuthorizationExpiryBusyAndTampering(t *testing.T) {
	r, s, _ := pickerFixture(t)
	token, _ := openPicker(t, r, 1, 10, 7)
	data := "models:" + token + ":s:0"
	r.Config.Telegram.ApprovedUsers = append(r.Config.Telegram.ApprovedUsers, 2)
	callback(t, r, 2, 10, 7, 2, data)
	if !strings.Contains(s.answer, "own menu") {
		t.Fatal("other user accepted")
	}
	callback(t, r, 3, 20, 7, 1, data)
	callback(t, r, 4, 10, 8, 1, data)
	if got, _ := r.chatModel(input(0, 10, 0, 1, "")); got.Model != r.Config.Provider.Model {
		t.Fatal("cross route mutation")
	}
	r.active = map[string]task{input(0, 10, 0, 1, "").WorkspaceKey(): {topic: 8, cancel: func() {}}}
	callback(t, r, 5, 10, 7, 1, data)
	if !strings.Contains(s.answer, "busy") {
		t.Fatal("busy change accepted")
	}
	r.active = nil
	for i, bad := range []string{"models:" + token + ":s:-1", "models:" + token + ":s:99999", "models:" + token + ":p:99999", "models:" + token + ":s:bad", "models:" + token + ":x:0", "models:fake:s:0"} {
		callback(t, r, int64(6+i), 10, 7, 1, bad)
	}
	if got, _ := r.chatModel(input(0, 10, 0, 1, "")); got.Model != r.Config.Provider.Model {
		t.Fatal("tampering changed model")
	}
	menu := r.modelMenus[token]
	menu.Expires = time.Now().Add(-time.Second)
	r.modelMenus[token] = menu
	callback(t, r, 12, 10, 7, 1, data)
	if !strings.Contains(s.answer, "expired") {
		t.Fatal("expired menu accepted")
	}
}
func TestModelRevisionPreventsResurrectingOtherTopicHistory(t *testing.T) {
	r, _, p := pickerFixture(t)
	m := input(1, 10, 7, 1, "old history")
	if err := r.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	old, _ := r.Store.Load(m.ConversationKey())
	if len(old.Pairs) != 1 {
		t.Fatal("missing history")
	}
	token, _ := openPicker(t, r, 2, 10, 8)
	callback(t, r, 3, 10, 8, 1, "models:"+token+":s:0")
	token, _ = openPicker(t, r, 4, 10, 8)
	index := -1
	for i, model := range r.modelMenus[token].Models {
		if model == r.Config.Provider.Model {
			index = i
		}
	}
	callback(t, r, 5, 10, 8, 1, fmt.Sprintf("models:%s:s:%d", token, index))
	if err := r.Handle(context.Background(), input(6, 10, 7, 1, "new history")); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	// Only system + new user; old A history must not return after A -> B -> A.
	if len(p.requests[1].Messages) != 2 {
		t.Fatalf("stale history resurrected: %+v", p.requests[1].Messages)
	}
}
func TestModelDiscoveryFailureDoesNotRunAgent(t *testing.T) {
	r, _, p := pickerFixture(t)
	p.fail = true
	if err := r.Handle(context.Background(), input(1, 10, 0, 1, "/models")); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	reply, ok := r.Store.Pending()
	if !ok || !strings.Contains(reply.Parts[0], "Unable to load models") || strings.Contains(reply.Parts[0], "secret") || len(p.requests) != 0 {
		t.Fatalf("failure: %+v", reply)
	}
}

func TestScheduledTaskUsesChatModelAndSameChoiceRetainsHistory(t *testing.T) {
	r, _, p := pickerFixture(t)
	token, _ := openPicker(t, r, 1, 10, 7)
	index := 0
	chosen := r.modelMenus[token].Models[index]
	callback(t, r, 2, 10, 7, 1, fmt.Sprintf("models:%s:s:%d", token, index))
	must(t, r.Handle(context.Background(), input(3, 10, 7, 1, "first")))
	r.Wait()
	before, _ := r.Store.Load(input(0, 10, 7, 1, "").ConversationKey())
	token, _ = openPicker(t, r, 4, 10, 7)
	callback(t, r, 5, 10, 7, 1, fmt.Sprintf("models:%s:s:%d", token, index))
	after, _ := r.loadModelConversation(input(0, 10, 7, 1, ""))
	if len(after.Pairs) != 1 || after.Identity != before.Identity || after.ModelSelection != before.ModelSelection {
		t.Fatal("same choice cleared history")
	}
	enqueue(t, r, input(0, 10, 8, 1, ""), "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	r.Wait()
	if len(p.requests) != 2 || p.requests[1].Model != chosen {
		t.Fatal("scheduler ignored chat model")
	}
	// A global provider change makes the old override inactive.
	r.Config.Provider.Name = "openai"
	r.Config.Provider.Model = "new-default"
	got, err := r.chatModel(input(0, 10, 7, 1, ""))
	if err != nil || got.Provider != "openai" || got.Model != "new-default" {
		t.Fatalf("old override leaked across provider: %+v %v", got, err)
	}
}
