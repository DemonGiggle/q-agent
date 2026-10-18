package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"q-agent/internal/platform"
	"q-agent/internal/state"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientRoutesAndPolling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || !strings.HasPrefix(r.URL.Path, "/bottoken/") {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		method := strings.TrimPrefix(r.URL.Path, "/bottoken/")
		switch method {
		case "getMe":
			w.Write([]byte(`{"ok":true,"result":{"id":99,"username":"bot"}}`))
		case "getUpdates":
			if body["offset"] != float64(11) || body["timeout"] != float64(25) {
				t.Errorf("poll: %+v", body)
			}
			w.Write([]byte(`{"ok":true,"result":[{"update_id":11,"message":{"message_id":1,"from":{"id":1},"chat":{"id":-10,"type":"supergroup"},"message_thread_id":7,"text":"hello"}}]}`))
		case "sendMessage":
			if body["chat_id"] != float64(-10) || body["message_thread_id"] != float64(7) || body["parse_mode"] != "HTML" {
				t.Errorf("route: %+v", body)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		case "sendChatAction":
			if body["chat_id"] != float64(-10) || body["message_thread_id"] != float64(7) || body["parse_mode"] != nil {
				t.Errorf("route: %+v", body)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Error(method)
		}
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	u, err := c.Me(context.Background())
	if err != nil || u.ID != 99 {
		t.Fatal(err)
	}
	updates, err := c.Updates(context.Background(), 11)
	if err != nil || len(updates) != 1 {
		t.Fatal(err)
	}
	m := platform.Message{ChatID: -10, TopicID: 7}
	if err = c.Send(context.Background(), m, "hi"); err != nil {
		t.Fatal(err)
	}
	if err = c.Typing(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}
func TestDeliveryRetryAndResume(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	first := true
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		messages = append(messages, body.Text)
		fail := first
		first = false
		mu.Unlock()
		if fail {
			w.WriteHeader(429)
			w.Write([]byte(`{"ok":false,"error_code":429,"description":"retry","parameters":{"retry_after":1}}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
		if body.Text == "last" {
			delivered <- struct{}{}
		}
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	dir := t.TempDir()
	s, _ := state.Open(dir, nil)
	s.Queue(platform.Message{Platform: "telegram", UpdateID: 1}, []string{"already delivered", "retry this", "last"})
	reply, _ := s.Pending()
	if err := s.Delivered(reply.ID); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Deliver(ctx, s, nil); close(done) }()
	select {
	case <-delivered:
	case <-time.After(4 * time.Second):
		cancel()
		t.Fatal("delivery failed")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.Pending(); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(messages, ",") != "retry this,retry this,last" {
		t.Fatalf("%v", messages)
	}
	if _, ok := s.Pending(); ok {
		t.Fatal("pending reply remains")
	}
}

func TestModelKeyboardAPIs(t *testing.T) {
	calls := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		method := strings.TrimPrefix(r.URL.Path, "/bottoken/")
		calls[method] = true
		switch method {
		case "getUpdates":
			allowed := body["allowed_updates"].([]any)
			if len(allowed) != 2 || allowed[1] != "callback_query" {
				t.Errorf("updates: %+v", body)
			}
			w.Write([]byte(`{"ok":true,"result":[{"update_id":2,"callback_query":{"id":"q","from":{"id":1},"message":{"message_id":5,"message_thread_id":7,"from":{"id":99},"chat":{"id":10,"type":"supergroup"}},"data":"models:token:p:1"}}]}`))
			return
		case "sendMessage", "editMessageText":
			if body["chat_id"] != float64(10) || body["parse_mode"] != "HTML" || body["reply_markup"] == nil {
				t.Errorf("keyboard: %+v", body)
			}
			if method == "sendMessage" && body["message_thread_id"] != float64(7) {
				t.Error("lost topic")
			}
			if method == "editMessageText" && body["message_id"] != float64(5) {
				t.Error("wrong edit target")
			}
		case "answerCallbackQuery":
			if body["callback_query_id"] != "q" {
				t.Error("wrong callback")
			}
		default:
			t.Error(method)
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	ctx := context.Background()
	updates, err := c.Updates(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := Normalize(updates[0], User{ID: 99, Username: "bot"})
	if !ok || m.UserID != 1 || m.CallbackID != "q" || m.TopicID != 7 {
		t.Fatalf("callback: %+v", m)
	}
	k := &platform.InlineKeyboard{Rows: [][]platform.InlineButton{{{Text: "Model", Data: "models:token:s:0"}}}}
	if err = c.SendKeyboard(ctx, m, "choose", k); err != nil {
		t.Fatal(err)
	}
	if err = c.EditModels(ctx, m, "page 2", k); err != nil {
		t.Fatal(err)
	}
	if err = c.AnswerCallback(ctx, "q", ""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatal(calls)
	}
}

func TestKeyboardOutboxSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyboard := &platform.InlineKeyboard{Rows: [][]platform.InlineButton{{{Text: "Next", Data: "models:token:p:1"}}}}
	if err = s.QueueKeyboard(platform.Message{Platform: "telegram", UpdateID: 1, ChatID: 10, TopicID: 7}, []string{"choose"}, keyboard); err != nil {
		t.Fatal(err)
	}
	s, err = state.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Keyboard *platform.InlineKeyboard `json:"reply_markup"`
			Topic    int64                    `json:"message_thread_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Keyboard == nil || body.Keyboard.Rows[0][0].Data != "models:token:p:1" || body.Topic != 7 {
			t.Errorf("lost keyboard/route: %+v", body)
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
		delivered <- struct{}{}
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Deliver(ctx, s, func(err error) {
			if ctx.Err() == nil {
				t.Error(err)
			}
		})
	}()
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Error("keyboard not delivered")
	}
	cancel()
	<-done
}

func TestSendFallbackOnParseError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls++
		if calls == 1 {
			if body["parse_mode"] != "HTML" {
				t.Errorf("first try should be HTML: %+v", body)
			}
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: test"}`))
			return
		}
		if body["parse_mode"] != nil {
			t.Errorf("fallback should drop parse_mode: %+v", body)
		}
		if body["text"] != "bold and plain" {
			t.Errorf("fallback should strip tags: %+v", body)
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	if err := c.Send(context.Background(), platform.Message{ChatID: 1}, "<b>bold</b> and plain"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}
