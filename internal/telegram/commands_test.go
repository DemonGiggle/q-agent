package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefaultCommandsMatchParser(t *testing.T) {
	commands := DefaultCommands()
	if len(commands) == 0 {
		t.Fatal("empty command menu")
	}
	seen := map[string]bool{}
	for _, c := range commands {
		if len(c.Command) < 1 || len(c.Command) > 32 {
			t.Fatalf("command length: %q", c.Command)
		}
		if c.Command != strings.ToLower(c.Command) {
			t.Fatalf("command must be lowercase: %q", c.Command)
		}
		for _, r := range c.Command {
			if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				t.Fatalf("invalid command rune: %q", c.Command)
			}
		}
		if len(c.Description) < 1 || len(c.Description) > 256 {
			t.Fatalf("description length: %q", c.Command)
		}
		if seen[c.Command] {
			t.Fatalf("duplicate command: %q", c.Command)
		}
		seen[c.Command] = true
	}
	// Every slash command accepted by Normalize must be advertised, and every
	// advertised command must be accepted (otherwise the menu misleads).
	for _, want := range []string{"provider", "model", "think", "max_steps", "settings", "memory", "forget", "new", "cancel", "status", "help", "models", "update", "restart"} {
		if !seen[want] {
			t.Fatalf("menu missing /%s", want)
		}
	}
	bot := User{ID: 99, Username: "our_bot"}
	for cmd := range seen {
		m := &Message{From: &User{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Text: "/" + cmd}
		if cmd == "provider" || cmd == "model" || cmd == "think" || cmd == "max_steps" || cmd == "settings" {
			m.Text += " arg"
		}
		if _, ok := Normalize(Update{ID: 1, Message: m}, bot); !ok {
			t.Fatalf("menu command /%s rejected by Normalize", cmd)
		}
	}
}

func TestSyncCommandsIdempotent(t *testing.T) {
	var sets int
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bottoken/")
		switch method {
		case "getMyCommands":
			// Simulate a stale menu from a previous agent.
			w.Write([]byte(`{"ok":true,"result":[{"command":"old","description":"stale"}]}`))
		case "setMyCommands":
			sets++
			if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
				t.Error(err)
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("method %s", method)
		}
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	if err := c.SyncCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sets != 1 {
		t.Fatalf("sets=%d", sets)
	}
	raw, _ := json.Marshal(DefaultCommands())
	var want []any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(gotPayload["commands"])
	var have []any
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if len(have) != len(want) {
		t.Fatalf("payload %v", gotPayload)
	}
}

func TestSyncCommandsSkipsSetWhenCurrent(t *testing.T) {
	raw, _ := json.Marshal(DefaultCommands())
	sets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bottoken/")
		if method == "getMyCommands" {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": json.RawMessage(raw)})
			return
		}
		if method == "setMyCommands" {
			sets++
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	c := New("token")
	c.BaseURL = server.URL
	c.HTTP = server.Client()
	if err := c.SyncCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sets != 0 {
		t.Fatal("redundant setMyCommands call")
	}
}
