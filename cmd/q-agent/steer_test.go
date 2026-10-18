//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

func TestServeTelegramFollowupSteersActiveTask(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-steer-token")
	c := config.Defaults()
	c.StateDir = t.TempDir()
	c.Telegram.ApprovedUsers, c.Telegram.ApprovedChats = []int64{1}, []int64{10}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, acknowledged, delivered := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var acknowledgements atomic.Int32
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprintln(w, `{"capabilities":["completion"]}`)
			return
		}
		if r.URL.Path != "/api/chat" {
			t.Errorf("provider path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body struct{ Messages []llm.Message }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := requests.Add(1)
		if n == 1 {
			close(started)
			select {
			case <-acknowledged:
			case <-r.Context().Done():
				return
			}
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"superseded answer"},"done":true}`)
			return
		}
		if n != 2 {
			t.Errorf("unexpected provider request: %d", n)
		}
		messages := body.Messages
		if len(messages) != 4 || messages[1].Content != "original task" || messages[2].Role != "assistant" || messages[2].Content != "superseded answer" || messages[3].Role != "user" || messages[3].Content != "new instruction\n\nwith extra detail" {
			t.Errorf("steer not injected: %+v", messages)
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"revised final"},"done":true}`)
	}))
	defer model.Close()
	c.Provider.BaseURL = model.URL
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "getMe":
			fmt.Fprintln(w, `{"ok":true,"result":{"id":99,"username":"test_bot"}}`)
		case "getMyCommands":
			fmt.Fprintln(w, `{"ok":true,"result":[]}`)
		case "setMyCommands":
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
		case "getUpdates":
			var body struct{ Offset int64 }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			id, text := int64(1), "/ask@test_bot original task"
			switch body.Offset {
			case 0:
			case 2:
				select {
				case <-started:
				case <-r.Context().Done():
					return
				}
				id, text = 2, "@test_bot new instruction"
			default:
				<-r.Context().Done()
				return
			}
			update := telegram.Update{ID: id, Message: &telegram.Message{ID: id, TopicID: 7, From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: 10, Type: "supergroup"}, Text: text}}
			updates := []telegram.Update{update}
			if id == 2 {
				updates = append(updates, telegram.Update{ID: 3, Message: &telegram.Message{ID: 3, TopicID: 7, From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: 10, Type: "supergroup"}, Text: "/ask@test_bot with extra detail"}})
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
		case "sendMessage", "editMessageText":
			var body struct {
				Text    string
				ChatID  int64 `json:"chat_id"`
				TopicID int64 `json:"message_thread_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ChatID != 10 {
				t.Error("reply escaped chat")
			}
			if strings.HasPrefix(body.Text, "Follow-up received;") {
				if acknowledgements.Add(1) == 2 {
					close(acknowledged)
				}
			}
			if body.Text == "superseded answer" {
				t.Error("superseded answer delivered as final")
			}
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
			if body.Text == "revised final" {
				if body.TopicID != 7 {
					t.Errorf("final topic = %d", body.TopicID)
				}
				select {
				case delivered <- struct{}{}:
				default:
				}
			}
		case "sendChatAction", "deleteMessage":
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("Telegram method: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot := telegram.New("test-steer-token")
	bot.BaseURL, bot.HTTP = server.URL, server.Client()
	policy, _ := redaction.New(redaction.Config{}, nil)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, c, policy, bot) }()
	select {
	case <-delivered:
	case err := <-done:
		t.Fatalf("service stopped: %v", err)
	case <-ctx.Done():
		t.Fatal("Telegram steer flow timed out")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(c.StateDir, policy)
	if err != nil {
		t.Fatal(err)
	}
	route := platform.Message{Platform: "telegram", ChatID: 10, TopicID: 7}
	conversation, err := store.Load(route.ConversationKey())
	if err != nil || len(conversation.Pairs) != 1 || conversation.Pairs[0].Answer != "revised final" || len(conversation.Pairs[0].Steers) != 1 || conversation.Pairs[0].Steers[0] != "new instruction\n\nwith extra detail" || requests.Load() != 2 {
		t.Fatalf("conversation: %+v, requests=%d, error=%v", conversation, requests.Load(), err)
	}
}
