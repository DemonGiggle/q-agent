//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"q-agent/internal/config"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/sandbox"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntegrationServe(t *testing.T) {
	testServe(t, true)
}

func TestIntegrationServeHost(t *testing.T) {
	testServe(t, false)
}

func testServe(t *testing.T, isolated bool) {
	t.Helper()
	if isolated && os.Getenv("Q_AGENT_INTEGRATION") != "1" {
		t.Skip("requires the real Linux sandbox and Debian image")
	}
	image, worker := os.Getenv("Q_AGENT_IMAGE"), os.Getenv("Q_AGENT_WORKER")
	if isolated && (image == "" || worker == "") {
		t.Fatal("Q_AGENT_IMAGE and Q_AGENT_WORKER are required")
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "fake-telegram-test-token")
	c := config.Defaults()
	c.StateDir = t.TempDir()
	c.Telegram.AllowedGroups = []int64{-10}
	c.Telegram.AllowedUsers = []int64{1}
	c.Sandbox.BaseImage = image
	c.Sandbox.Worker = worker
	c.Sandbox.Enabled = isolated
	if isolated {
		options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: worker})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := sandbox.Remove(ctx, options, c.StateDir); err != nil {
				t.Error(err)
			}
		})
	} else {
		c.Sandbox.BaseImage = filepath.Join(c.StateDir, "missing-image")
		c.Sandbox.Worker = filepath.Join(c.StateDir, "missing-worker")
		c.Sandbox.Bubblewrap = "missing-bwrap"
		c.Sandbox.NewUIDMap = "missing-newuidmap"
		c.Sandbox.NewGIDMap = "missing-newgidmap"
		// A legacy sandbox mapping must not be validated or rewritten on host.
		if err := state.Atomic(filepath.Join(c.StateDir, "mapping.json"), map[string]string{"legacy": "unchanged"}); err != nil {
			t.Error(err)
		}
	}
	t.Setenv("Q_AGENT_SERVICE_SECRET", "service-filter-test-value")
	t.Setenv("Q_AGENT_SERVICE_VISIBLE", "service-visible-test-value")
	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("provider path %s", r.URL.Path)
		}
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if requests.Add(1) == 1 {
			command := "printf ready > service-tested"
			if !isolated {
				workdir := filepath.Join(c.StateDir, "host-workspaces", platform.WorkspaceKey("telegram", -10), "workspace")
				if !strings.Contains(input.Messages[0].Content, workdir) || !strings.Contains(input.Messages[0].Content, "ordinary host permissions") || strings.Contains(input.Messages[0].Content, "isolated, persistent Debian") {
					t.Errorf("host system prompt: %s", input.Messages[0].Content)
				}
				command = `test -z "$TELEGRAM_BOT_TOKEN$Q_AGENT_SERVICE_SECRET" && test "$Q_AGENT_SERVICE_VISIBLE" = service-visible-test-value && printf ready > service-tested`
			}
			payload := map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "shell", "arguments": map[string]string{"command": command}}}}}, "done": true, "prompt_eval_count": 1, "eval_count": 1}
			json.NewEncoder(w).Encode(payload)
		} else {
			last := input.Messages[len(input.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, `"trust":"untrusted"`) {
				t.Errorf("tool continuation: %+v", last)
			}
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"completed😀"},"done":true,"prompt_eval_count":2,"eval_count":2}`)
		}
	}))
	defer model.Close()
	c.Provider.BaseURL = model.URL
	restartReady := make(chan struct{}, 1)
	ready := make(chan struct{}, 1)
	gate := make(chan struct{})
	delivered := make(chan struct{}, 1)
	var polls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "getMe":
			fmt.Fprintln(w, `{"ok":true,"result":{"id":99,"username":"test_bot"}}`)
		case "getMyCommands":
			fmt.Fprintln(w, `{"ok":true,"result":[]}`)
		case "setMyCommands":
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
		case "getUpdates":
			data, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			var args struct {
				Offset int64 `json:"offset"`
			}
			json.Unmarshal(data, &args)
			poll := polls.Add(1)
			if poll == 1 && args.Offset != 0 || poll > 1 && args.Offset != 3 {
				t.Errorf("poll offset: %d", args.Offset)
			}
			if poll == 3 {
				restartReady <- struct{}{}
			}
			if poll == 1 {
				ready <- struct{}{}
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				fmt.Fprintln(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":2},"chat":{"id":-99,"type":"supergroup"},"text":"/ask@test_bot unauthorized"}},{"update_id":2,"message":{"message_id":2,"message_thread_id":7,"from":{"id":1},"chat":{"id":-10,"type":"supergroup"},"text":"/ask@test_bot write a file"}}]}`)
			} else {
				<-r.Context().Done()
			}
		case "sendChatAction":
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
		case "sendMessage":
			var message struct {
				ChatID, Topic int64
				Text          string
				ParseMode     string
			}
			var raw map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&raw)
			json.Unmarshal(raw["chat_id"], &message.ChatID)
			json.Unmarshal(raw["message_thread_id"], &message.Topic)
			json.Unmarshal(raw["text"], &message.Text)
			if message.Text == "completed😀" {
				if message.ChatID != -10 || message.Topic != 7 || string(raw["parse_mode"]) != `"HTML"` {
					t.Errorf("reply: %+v", message)
				}
				fmt.Fprintln(w, `{"ok":true,"result":true}`)
				delivered <- struct{}{}
				break
			}
			// Best-effort live preview: return an ID so edits work, no delivery signal.
			if message.ChatID != -10 || message.Topic != 7 {
				t.Errorf("live route: %+v", message)
			}
			fmt.Fprintln(w, `{"ok":true,"result":{"message_id":99}}`)
		case "editMessageText", "deleteMessage":
			// Live preview maintenance: best effort, never completes delivery.
			fmt.Fprintln(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("method: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot := telegram.New("fake-telegram-test-token")
	bot.BaseURL = server.URL
	bot.HTTP = server.Client()
	policy, _ := redaction.New(redaction.Config{}, []string{"fake-telegram-test-token"})
	done := make(chan error, 1)
	go func() { done <- serve(ctx, c, policy, bot) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("service did not start")
	}
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				t.Log("idle service test process", line)
			}
		}
	}
	close(gate)
	select {
	case <-delivered:
	case <-ctx.Done():
		t.Fatal("service did not reply")
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(c.StateDir, policy)
	if err != nil {
		t.Fatal(err)
	}
	if store.Cursor() != 2 || requests.Load() != 2 {
		t.Fatalf("cursor=%d requests=%d", store.Cursor(), requests.Load())
	}
	m := platform.Message{Platform: "telegram", ChatID: -10, TopicID: 7}
	conversation, err := store.Load(m.ConversationKey())
	if err != nil || len(conversation.Pairs) != 1 || conversation.Pairs[0].Answer != "completed😀" {
		t.Fatalf("history: %+v %v", conversation, err)
	}
	// Restart the host runtime with the persisted cursor. The mock long poll
	// returns no new update; completed work must not be executed again.
	restartCtx, stopRestart := context.WithTimeout(context.Background(), 5*time.Second)
	restarted := make(chan error, 1)
	go func() { restarted <- serve(restartCtx, c, policy, bot) }()
	select {
	case <-restartReady:
	case <-restartCtx.Done():
		stopRestart()
		t.Fatal("restart did not poll")
	}
	stopRestart()
	if err := <-restarted; err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("completed update replayed after restart")
	}
	store, err = state.Open(c.StateDir, policy)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err = store.Load(m.ConversationKey())
	if err != nil || len(conversation.Pairs) != 1 {
		t.Fatalf("same-mode history lost: %+v %v", conversation, err)
	}
	root := filepath.Join(c.StateDir, "host-workspaces", m.WorkspaceKey(), "workspace")
	if isolated {
		root = filepath.Join(c.StateDir, "workspaces", m.WorkspaceKey(), "rootfs", "workspace")
	}
	data, err := os.ReadFile(filepath.Join(root, "service-tested"))
	if err != nil || string(data) != "ready" {
		t.Fatalf("file: %s %v", data, err)
	}
	if !isolated {
		data, err := os.ReadFile(filepath.Join(c.StateDir, "mapping.json"))
		if err != nil || string(data) != `{"legacy":"unchanged"}` {
			t.Fatalf("host changed sandbox mapping: %s %v", data, err)
		}
	}
}
func TestMappingMustBeComplete(t *testing.T) {
	dir := t.TempDir()
	o := sandbox.Options{UIDBase: 100000, GIDBase: 100000}
	if err := checkMapping(dir, o); err != nil {
		t.Fatal(err)
	}
	if err := checkMapping(dir, o); err != nil {
		t.Fatal(err)
	}
	o.UIDBase++
	if err := checkMapping(dir, o); err == nil {
		t.Fatal("changed mapping accepted")
	}
	if err := state.Atomic(filepath.Join(dir, "mapping.json"), map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if err := checkMapping(dir, o); err == nil {
		t.Fatal("incomplete mapping accepted")
	}
}
