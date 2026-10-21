//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"q-agent/internal/agent"
	"q-agent/internal/host"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

type processProvider struct {
	requests  chan llm.Request
	responses chan llm.Message
}

func (p *processProvider) Name() string { return "ollama" }
func (p *processProvider) Complete(ctx context.Context, r llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	select {
	case p.requests <- r:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	select {
	case m := <-p.responses:
		return llm.Response{Message: m}, nil
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}
func (p *processProvider) call(name string, args any) {
	b, _ := json.Marshal(args)
	p.responses <- llm.Message{ToolCalls: []llm.ToolCall{{Name: name, Arguments: b}}}
}
func processRequest(t *testing.T, p *processProvider) llm.Request {
	t.Helper()
	select {
	case r := <-p.requests:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not resume")
		return llm.Request{}
	}
}
func latestProcess(t *testing.T, r llm.Request) process.Result {
	t.Helper()
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "tool" {
			var envelope struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(r.Messages[i].Content), &envelope); err != nil {
				t.Fatal(err)
			}
			var v process.Result
			if err := json.Unmarshal([]byte(envelope.Content), &v); err != nil {
				t.Fatalf("invalid process observation: %v %s", err, envelope.Content)
			}
			return v
		}
	}
	t.Fatal("no tool result")
	return process.Result{}
}
func setupProcessRuntime(t *testing.T) (*Runtime, *processProvider, string) {
	t.Helper()
	r, _, _ := fixture(t)
	dir := t.TempDir()
	p := &processProvider{requests: make(chan llm.Request, 16), responses: make(chan llm.Message, 16)}
	r.Tools = func(string) agent.Tools { return &host.Runner{Options: localtools.Options{WorkingDir: dir}} }
	r.Providers = func(state.Conversation) (llm.Provider, error) { return p, nil }
	return r, p, dir
}
func TestProcessWaitSteeringAuthorizationAndTaskCleanup(t *testing.T) {
	r, p, dir := setupProcessRuntime(t)
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "authorize application")))
	processRequest(t, p)
	p.call("shell", map[string]any{"command": "printf 'Open https://example.test/device ABCD-1234'; read approval; printf 'authorized %s' \"$approval\"; sleep 30", "background": true, "yield_ms": 10})
	session := latestProcess(t, processRequest(t, p))
	p.call("process", map[string]any{"action": "wait", "session_id": session.ID, "cursor": session.EndCursor, "human_input": true, "message": "Open https://example.test/device and enter ABCD-1234"})
	// Wait for the durable interim, then exercise topic and owner isolation.
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(strings.Join(drainReplies(t, r.Store), "\n"), "example.test") {
		if time.Now().After(deadline) {
			t.Fatal("authorization interim missing")
		}
		time.Sleep(time.Millisecond)
	}
	must(t, r.Handle(context.Background(), input(2, -10, 8, 1, "wrong topic")))
	must(t, r.Handle(context.Background(), input(3, -10, 7, 2, "wrong owner")))
	select {
	case <-p.requests:
		t.Fatal("foreign input woke process wait")
	case <-time.After(30 * time.Millisecond):
	}
	must(t, r.Handle(context.Background(), input(4, -10, 7, 1, "browser approved")))
	req := processRequest(t, p)
	if !strings.Contains(strings.Join(userTexts(req), "\n"), "browser approved") {
		t.Fatal("follow-up did not enter steering")
	}
	if latestProcess(t, req).Reason != "user_input" {
		t.Fatal("wait did not identify user wake")
	}
	p.call("process", map[string]any{"action": "write", "session_id": session.ID, "data": "yes\n"})
	processRequest(t, p)
	p.call("shell", map[string]any{"command": "(sleep 1; touch late) & wait", "background": true, "yield_ms": 1})
	processRequest(t, p)
	p.responses <- llm.Message{Content: "Authorization completed."}
	r.Wait()
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
		t.Fatal("final answer left sessions running")
	}
	// A new task cannot read the old ID.
	must(t, r.Handle(context.Background(), input(5, -10, 7, 1, "check old process")))
	processRequest(t, p)
	p.call("process", map[string]any{"action": "read", "session_id": session.ID})
	req = processRequest(t, p)
	if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "unknown session") {
		t.Fatal("session survived across tasks")
	}
	p.responses <- llm.Message{Content: "Old process ended."}
	r.Wait()
}
func TestInteractiveDeadlineSwitchAndOutcomes(t *testing.T) {
	c, cancel := newTaskContext(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := c.started
	c.openInteractive(150 * time.Millisecond)
	deadline, _ := c.Deadline()
	if deadline.Sub(start) != 150*time.Millisecond {
		t.Fatal("budget not measured from task start")
	}
	time.Sleep(45 * time.Millisecond)
	if c.Err() != nil {
		t.Fatal("old timer ended interactive task")
	}
	c.openInteractive(time.Second) // Switch occurs once.
	deadline, _ = c.Deadline()
	if deadline.Sub(start) != 150*time.Millisecond {
		t.Fatal("second session renewed budget")
	}
	select {
	case <-c.Done():
		if c.Err() != context.DeadlineExceeded {
			t.Fatal(c.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("deadline not enforced")
	}
	longer, stop := newTaskContext(context.Background(), time.Second)
	defer stop()
	before, _ := longer.Deadline()
	longer.openInteractive(30 * time.Millisecond)
	after, _ := longer.Deadline()
	if !before.Equal(after) {
		t.Fatal("longer task shortened")
	}
	stop()
	if longer.Err() != context.Canceled {
		t.Fatal("cancellation classified as deadline")
	}
	parent, parentCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer parentCancel()
	inherited, finish := newTaskContext(parent, time.Second)
	defer finish()
	inherited.openInteractive(time.Hour)
	<-inherited.Done()
	if inherited.Err() != context.DeadlineExceeded {
		t.Fatal("parent deadline lost")
	}
}
func TestProcessDeadlineStepLimitAndShutdownCleanup(t *testing.T) {
	for _, kind := range []string{"deadline", "steps", "shutdown", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			r, p, dir := setupProcessRuntime(t)
			r.Config.Limits.TaskTimeout = "50ms"
			r.Config.Limits.InteractiveTaskTimeout = "150ms"
			if kind == "steps" {
				r.Config.Limits.MaxSteps = 1
			}
			must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "start server")))
			processRequest(t, p)
			p.call("shell", map[string]any{"command": "(sleep 1; touch late) & wait", "background": true, "yield_ms": 1})
			if kind != "steps" {
				processRequest(t, p)
			}
			switch kind {
			case "shutdown":
				r.Close()
			case "cancel":
				must(t, r.Handle(context.Background(), input(2, -10, 7, 1, "/cancel")))
			}
			r.Wait()
			replies := strings.Join(drainReplies(t, r.Store), "\n")
			if kind == "deadline" && !strings.Contains(replies, "deadline") {
				t.Fatal(replies)
			}
			if kind == "cancel" && !strings.Contains(replies, "cancelled") {
				t.Fatal(replies)
			}
			time.Sleep(1100 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
				t.Fatal("failed task leaked process")
			}
		})
	}
}

// Capture the direct provider boundary as well as Telegram and persisted state.
func TestFragmentedProcessCredentialsStayOutOfAllBoundaries(t *testing.T) {
	r, p, dir := setupProcessRuntime(t)
	secret := "configured-sensitive-value"
	policy, _ := redaction.New(redaction.Config{}, []string{secret})
	r.Options.Redaction = policy
	var logs syncBuffer
	r.Options.Logger = log.New(&logs, "", 0)
	must(t, r.Handle(context.Background(), input(1, -10, 7, 1, "inspect application")))
	all := []llm.Request{processRequest(t, p)}
	must(t, os.WriteFile(filepath.Join(dir, "auth-fixture.sh"), []byte("printf 'configured-'; read gate; printf '\\033[31msensitive-value\\033[0m'; read gate; printf '\\nghp_abcdefghijklmnopqrstuv0123456789\\n'"), 0600))
	p.call("shell", map[string]any{"command": "sh auth-fixture.sh", "pty": true, "yield_ms": 10})
	req := processRequest(t, p)
	all = append(all, req)
	session := latestProcess(t, req)
	for i := 0; i < 2; i++ {
		p.call("process", map[string]any{"action": "write", "session_id": session.ID, "data": "go\n"})
		all = append(all, processRequest(t, p))
		p.call("process", map[string]any{"action": "read", "session_id": session.ID})
		all = append(all, processRequest(t, p))
		p.call("process", map[string]any{"action": "read", "session_id": session.ID, "view": "screen"})
		all = append(all, processRequest(t, p))
	}
	p.responses <- llm.Message{Content: "Application inspected."}
	r.Wait()
	for _, req := range all {
		encoded, _ := json.Marshal(req)
		if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "ghp_abcdefghijklmnopqrstuv0123456789") {
			t.Fatal("credential reached direct provider request")
		}
	}
	for _, req := range all {
		for _, m := range req.Messages {
			if m.Role == "tool" && (strings.Contains(m.Content, "configured-") || strings.Contains(m.Content, "sensitive-value") || strings.Contains(m.Content, "ghp_abcdef")) {
				t.Fatal("credential fragment reached provider: " + m.Content)
			}
		}
	}
	replies := strings.Join(drainReplies(t, r.Store), "\n")
	if strings.Contains(replies, secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("credential reached output")
	}
	err := filepath.WalkDir(r.Config.StateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if strings.Contains(string(data), secret) || strings.Contains(string(data), "ghp_abcdefghijklmnopqrstuv0123456789") {
			return fmt.Errorf("credential persisted in %s", path)
		}
		return err
	})
	must(t, err)
}

// Concurrent console writes are expected from live reporting.
type syncBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text.String() }

func TestScheduledProcessesRejectHumanInputAndRemainTaskScoped(t *testing.T) {
	r, p, dir := setupProcessRuntime(t)
	enqueue(t, r, input(0, -10, 7, 1, ""), "agent", time.Now())
	must(t, r.dispatchScheduled(context.Background()))
	processRequest(t, p)
	p.call("shell", map[string]any{"command": "(sleep 1; touch late) & wait", "background": true, "yield_ms": 1})
	session := latestProcess(t, processRequest(t, p))
	p.call("process", map[string]any{"action": "wait", "session_id": session.ID, "human_input": true, "message": "Please authorize"})
	req := processRequest(t, p)
	if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "human input unavailable") {
		t.Fatal("scheduled task waited for a human")
	}
	p.responses <- llm.Message{Content: "Human authorization unavailable."}
	r.Wait()
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
		t.Fatal("scheduled session outlived task")
	}
}
