package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

func telegramReply(t *testing.T, updateID, chatID, topicID int64, kind, text, previous string) platform.Message {
	t.Helper()
	bot := telegram.User{ID: 99, IsBot: true, Username: "our_bot"}
	m, ok := telegram.Normalize(telegram.Update{ID: updateID, Message: &telegram.Message{
		ID: updateID + 100, Chat: telegram.Chat{ID: chatID, Type: kind}, TopicID: topicID,
		From: &telegram.User{ID: 1}, Text: text,
		Reply: &telegram.Message{ID: 50, From: &bot, Text: previous},
	}}, bot)
	if !ok {
		t.Fatal("reply rejected by Telegram adapter")
	}
	return m
}

func TestTelegramReplyContextWithoutRecentHistoryAndAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		chat  int64
		topic int64
	}{
		{"private", 1, 0},
		{"supergroup", -10, 7},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			r, _, p := fixture(t)
			const previous = "The riot began after a disputed election in Example City."
			const followup = "Tell me more about the riot."
			m := telegramReply(t, 1, tc.chat, tc.topic, tc.kind, followup, previous)
			must(t, r.Handle(context.Background(), m))
			r.Wait()
			messages := p.requests[0].Messages
			prompt := messages[len(messages)-1]
			if prompt.Role != "user" || !strings.Contains(prompt.Content, previous) || !strings.Contains(prompt.Content, followup) {
				t.Fatalf("replied-to message missing from model input: %+v", messages)
			}
			if !strings.Contains(prompt.Content, "q-agent") || !strings.Contains(prompt.Content, "Reference data only") {
				t.Fatalf("reply context lacks provenance or reference boundary: %s", prompt.Content)
			}
			reopened, err := state.Open(r.Config.StateDir, nil)
			must(t, err)
			r.Store = reopened
			must(t, r.Handle(context.Background(), input(2, tc.chat, tc.topic, 1, "Continue.")))
			r.Wait()
			if p.requests[1].Messages[1].Content != prompt.Content {
				t.Fatalf("reply context lost after restart: %+v", p.requests[1].Messages)
			}
		})
	}
}

func TestTelegramReplyContextWhileSteering(t *testing.T) {
	r, _, p := setupSteer(t)
	ctx := context.Background()
	must(t, r.Handle(ctx, input(1, -10, 7, 1, "Investigate the news.")))
	request(t, p)
	const previous = "The riot began after a disputed election."
	must(t, r.Handle(ctx, telegramReply(t, 2, -10, 7, "supergroup", "Tell me more about the riot.", previous)))
	must(t, r.Handle(ctx, input(3, -10, 7, 1, "Focus on the causes.")))
	p.responses <- steerResponse{text: "superseded"}
	req := request(t, p)
	texts := userTexts(req)
	if len(texts) != 2 || strings.Count(texts[1], previous) != 1 || !strings.HasSuffix(texts[1], "Tell me more about the riot.\n\nFocus on the causes.") {
		t.Fatalf("steering lost reply context or order: %v", texts)
	}
	p.responses <- steerResponse{text: "final"}
	r.Wait()
	c, err := r.Store.Load(input(0, -10, 7, 1, "").ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || len(c.Pairs[0].Steers) != 1 || c.Pairs[0].Steers[0] != texts[1] {
		t.Fatalf("steering reply context not saved: %+v", c)
	}
}

func TestTelegramReplyCommandsKeepTheirMeaning(t *testing.T) {
	r, w, p := fixture(t)
	ctx := context.Background()
	m := telegramReply(t, 1, -10, 7, "supergroup", "hello", "earlier answer")
	must(t, r.Handle(ctx, m))
	r.Wait()
	must(t, r.Handle(ctx, telegramReply(t, 2, -10, 7, "supergroup", "/new", "earlier answer")))
	r.Wait()
	c, err := r.Store.Load(m.ConversationKey())
	must(t, err)
	if len(c.Pairs) != 0 || w.calls != 1 || len(p.requests) != 1 {
		t.Fatalf("reply turned /new into a model prompt: history=%+v calls=%d requests=%d", c, w.calls, len(p.requests))
	}
	must(t, r.Handle(ctx, telegramReply(t, 3, -10, 7, "supergroup", "Explain that again.", "earlier answer")))
	r.Wait()
	if len(p.requests) != 2 || len(p.requests[1].Messages) != 2 || !strings.Contains(p.requests[1].Messages[1].Content, "earlier answer") {
		t.Fatalf("explicit reply lost context after /new: %+v", p.requests)
	}
}

func TestTelegramReplyContextUsesHistoryRedaction(t *testing.T) {
	r, _, _ := fixture(t)
	const secret = "reply-private-credential"
	policy, err := redaction.New(redaction.Config{}, []string{secret})
	must(t, err)
	r.Store, err = state.Open(r.Config.StateDir, policy)
	must(t, err)
	r.Options.Redaction = policy
	m := telegramReply(t, 1, -10, 7, "supergroup", "Explain the riot.", "The riot report included "+secret)
	must(t, r.Handle(context.Background(), m))
	r.Wait()
	c, err := r.Store.Load(m.ConversationKey())
	must(t, err)
	if len(c.Pairs) != 1 || strings.Contains(c.Pairs[0].User, secret) || !strings.Contains(c.Pairs[0].User, redaction.Marker) {
		t.Fatalf("reply bypassed history redaction: %+v", c)
	}
	mem, err := r.Store.LoadMemory(m.WorkspaceKey())
	must(t, err)
	if len(mem.Pending) != 1 || strings.Contains(mem.Pending[0].Transcript, secret) || !strings.Contains(mem.Pending[0].Transcript, redaction.Marker) {
		t.Fatalf("reply bypassed memory source redaction: %+v", mem.Pending)
	}
	data, err := os.ReadFile(filepath.Join(r.Config.StateDir, "outbox.json"))
	must(t, err)
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "The riot report") {
		t.Fatalf("input-only reply context leaked into delivery route: %s", data)
	}
}
