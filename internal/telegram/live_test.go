package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"q-agent/internal/platform"
)

func TestLiveSendEditDelete(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		switch {
		case len(bodies) == 1:
			w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
		case len(bodies) == 2:
			// edit
			if body["message_id"] != float64(42) || body["text"] == "" {
				t.Errorf("edit: %+v", body)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			if body["message_id"] != float64(42) {
				t.Errorf("delete: %+v", body)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	ctx := context.Background()
	m := platform.Message{ChatID: 10, TopicID: 7}
	id, err := c.SendLive(ctx, m, "Working…")
	if err != nil || id != 42 {
		t.Fatalf("%d %v", id, err)
	}
	if bodies[0]["message_thread_id"] != float64(7) {
		t.Fatalf("lost topic: %+v", bodies[0])
	}
	if err := c.EditLive(ctx, m, id, "Working… done\n\nhello"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLive(ctx, m, id); err != nil {
		t.Fatal(err)
	}
	// message-not-modified is success.
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`))
	}))
	defer server2.Close()
	c.BaseURL = server2.URL
	c.HTTP = server2.Client()
	if err := c.EditLive(ctx, m, 42, "same"); err != nil {
		t.Fatal(err)
	}
	// Boolean stub result disables edits but does not fail.
	server3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server3.Close()
	c.BaseURL = server3.URL
	c.HTTP = server3.Client()
	if id, err := c.SendLive(ctx, m, "hi"); err != nil || id != 0 {
		t.Fatalf("bool stub: %d %v", id, err)
	}
	if err := c.EditLive(ctx, m, 0, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLive(ctx, m, 0); err != nil {
		t.Fatal(err)
	}
}
