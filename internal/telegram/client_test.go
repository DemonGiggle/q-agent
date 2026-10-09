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
		case "sendMessage", "sendChatAction":
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
