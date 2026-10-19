package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
)

func TestDeliveryPhotoThenText(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path)
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	c := New("tok")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	dir := t.TempDir()
	s, _ := state.Open(dir, nil)
	fp := filepath.Join(dir, "a.png")
	os.WriteFile(fp, []byte{1, 2, 3}, 0600)
	route := platform.Message{Platform: "telegram", ChatID: 1, UpdateID: 5}
	photos, _, _ := ExtractPhotos("![cap]("+fp+")", dir)
	if len(photos) != 1 {
		t.Fatalf("extract %+v", photos)
	}
	if err := s.QueueWithPhotos(route, []string{"hello"}, photos, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Deliver(ctx, s, nil); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.Pending(); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, ok := s.Pending(); ok {
		t.Fatalf("pending remains")
	}
	joined := strings.Join(methods, ",")
	if !strings.Contains(joined, "sendPhoto") || !strings.Contains(joined, "sendMessage") {
		t.Fatalf("methods=%v", methods)
	}
	if methods[0] != "/bottok/sendPhoto" {
		t.Fatalf("photos must go first: %v", methods)
	}
}

func TestDeliveryPhotoFallbackOnPermanentFailure(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: wrong file"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["caption"].(string); ok {
			bodies = append(bodies, v)
		} else if v, ok := body["text"].(string); ok {
			bodies = append(bodies, v)
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	c := New("tok")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	dir := t.TempDir()
	s, _ := state.Open(dir, nil)
	route := platform.Message{Platform: "telegram", ChatID: 1, UpdateID: 6}
	photos := []state.PhotoAttachment{{RemoteURL: "https://example.com/a.png", Caption: "cap", Fallback: "fallback link"}}
	if err := s.QueueWithPhotos(route, []string{"after"}, photos, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Deliver(ctx, s, nil); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.Pending(); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, ok := s.Pending(); ok {
		t.Fatalf("pending remains")
	}
	if len(bodies) == 0 || bodies[0] != "fallback link" {
		t.Fatalf("fallback not sent: %v", bodies)
	}
}
