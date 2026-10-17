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
	"testing"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

func TestServeModelsPaginationAndRouting(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-model-picker-token")
	c := config.Defaults()
	c.StateDir = t.TempDir()
	c.Telegram.ApprovedUsers = []int64{1}
	c.Telegram.ApprovedChats = []int64{10}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			models := []map[string]any{}
			for i := 0; i < 12; i++ {
				models = append(models, map[string]any{"name": fmt.Sprintf("model-%02d", i), "capabilities": []string{"completion"}})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": models})
		case "/api/show":
			fmt.Fprintln(w, `{"capabilities":["completion"]}`)
		case "/api/chat":
			var body struct{ Model string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Model != "model-09" {
				t.Errorf("wrong selected model: %s", body.Model)
			}
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"picker complete"},"done":true}`)
		default:
			t.Errorf("provider method: %s", r.URL.Path)
		}
	}))
	defer model.Close()
	c.Provider.BaseURL = model.URL
	first := make(chan *platform.InlineKeyboard, 1)
	second := make(chan *platform.InlineKeyboard, 2)
	delivered := make(chan struct{}, 1)
	sendUpdate := func(w http.ResponseWriter, id int64, text, data string) {
		update := telegram.Update{ID: id}
		msg := &telegram.Message{ID: 100, TopicID: 7, Chat: telegram.Chat{ID: 10, Type: "supergroup"}}
		if data != "" {
			msg.From = &telegram.User{ID: 99}
			update.Callback = &telegram.CallbackQuery{ID: fmt.Sprint(id), From: &telegram.User{ID: 1}, Message: msg, Data: data}
		} else {
			msg.From = &telegram.User{ID: 1}
			msg.Text = text
			update.Message = msg
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []telegram.Update{update}})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "getMe":
			fmt.Fprintln(w, `{"ok":true,"result":{"id":99,"username":"test_bot"}}`)
		case "getUpdates":
			var body struct{ Offset int64 }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			switch body.Offset {
			case 0:
				sendUpdate(w, 1, "/models@test_bot", "")
			case 2:
				select {
				case keyboard := <-first:
					row := keyboard.Rows[len(keyboard.Rows)-1]
					sendUpdate(w, 2, "", row[0].Data)
				case <-r.Context().Done():
				}
			case 3:
				select {
				case keyboard := <-second:
					for _, row := range keyboard.Rows {
						for _, b := range row {
							if b.Text == "model-09" {
								sendUpdate(w, 3, "", b.Data)
								return
							}
						}
					}
					t.Error("model missing on second page")
				case <-r.Context().Done():
				}
			case 4:
				sendUpdate(w, 4, "/ask@test_bot test the chosen model", "")
			default:
				<-r.Context().Done()
			}
		case "sendMessage", "editMessageText":
			var body struct {
				Text     string
				Keyboard *platform.InlineKeyboard `json:"reply_markup"`
				ChatID   int64                    `json:"chat_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ChatID != 10 {
				t.Error("wrong chat")
			}
			if body.Keyboard != nil && len(body.Keyboard.Rows) > 0 {
				if strings.Contains(body.Text, "Page 1/") {
					first <- body.Keyboard
				} else {
					second <- body.Keyboard
				}
			}
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
			if body.Text == "picker complete" {
				delivered <- struct{}{}
			}
		case "answerCallbackQuery", "sendChatAction":
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("Telegram method: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot := telegram.New("test-model-picker-token")
	bot.BaseURL = server.URL
	bot.HTTP = server.Client()
	policy, _ := redaction.New(redaction.Config{}, nil)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, c, policy, bot) }()
	select {
	case <-delivered:
	case err := <-done:
		t.Fatalf("service stopped: %v", err)
	case <-ctx.Done():
		t.Fatal("model picker flow timed out")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(c.StateDir, policy)
	if err != nil {
		t.Fatal(err)
	}
	m := platform.Message{Platform: "telegram", ChatID: 10, TopicID: 7}
	choice, err := store.ChatModel(m.WorkspaceKey())
	if err != nil || choice.Model != "model-09" {
		t.Fatalf("choice: %+v %v", choice, err)
	}
	conversation, err := store.Load(m.ConversationKey())
	if err != nil || conversation.Model != "model-09" || len(conversation.Pairs) != 1 {
		t.Fatalf("conversation: %+v %v", conversation, err)
	}
}
