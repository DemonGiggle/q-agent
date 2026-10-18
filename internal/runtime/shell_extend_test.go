package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseShellDecision(t *testing.T) {
	for text, want := range map[string]bool{"extend": true, "YES": true, "continue": true, "kill": false, "Terminate": false, "no": false} {
		got, ok := parseShellDecision(text)
		if !ok || got != want {
			t.Fatalf("%q -> %v,%v want %v,true", text, got, ok, want)
		}
	}
	if _, ok := parseShellDecision("hello"); ok {
		t.Fatal("non-decision accepted")
	}
}

func TestShellPromptScheduledNeverAsks(t *testing.T) {
	r, _, _ := fixture(t)
	if r.requestShellExtend(context.Background(), input(1, 10, 0, 1, "go"), "run", "sh", []string{"a"}) {
		t.Fatal("scheduled run must not prompt")
	}
}

func TestShellPromptExtendAndKillViaHandle(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{{"extend", true}, {"kill", false}} {
		r, _, _ := fixture(t)
		owner := input(11, 10, 7, 1, "go")
		done := make(chan bool, 1)
		go func() {
			done <- r.requestShellExtend(context.Background(), owner, "", "sleep 30", []string{"l1", "l2", "l3"})
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			r.mu.Lock()
			pending := len(r.shellPrompts) > 0
			r.mu.Unlock()
			if pending || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		reply, ok := r.Store.Pending()
		if !ok || !strings.Contains(strings.Join(reply.Parts, "\n"), "l3") || !strings.Contains(strings.Join(reply.Parts, "\n"), "extend") {
			t.Fatalf("prompt missing last lines: %+v", reply)
		}
		if err := r.Store.Delivered(reply.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.Handle(context.Background(), input(12, 10, 7, 1, tc.text)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got != tc.want {
				t.Fatalf("%q -> %v want %v", tc.text, got, tc.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%q: decision not delivered", tc.text)
		}
		// Decision ack queued.
		ack, ok := r.Store.Pending()
		if !ok || !strings.Contains(ack.Parts[0], "Shell") {
			t.Fatalf("decision ack missing: %+v", ack)
		}
		if err := r.Store.Delivered(ack.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShellPromptTimeoutDefaultsKill(t *testing.T) {
	r, _, _ := fixture(t)
	r.Config.Limits.ShellPromptTimeout = "60ms"
	owner := input(21, 10, 7, 1, "go")
	start := time.Now()
	if r.requestShellExtend(context.Background(), owner, "", "sleep 30", []string{"x"}) {
		t.Fatal("expired prompt must kill")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("prompt wait too long")
	}
}

func TestShellPromptBlocksSteerUntilResolved(t *testing.T) {
	r, _, _ := fixture(t)
	owner := input(31, 10, 7, 1, "go")
	done := make(chan bool, 1)
	go func() { done <- r.requestShellExtend(context.Background(), owner, "", "sleep 30", []string{"o"}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mu.Lock()
		pending := len(r.shellPrompts) > 0
		r.mu.Unlock()
		if pending || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Drain prompt.
	prompt, ok := r.Store.Pending()
	if !ok {
		t.Fatal("prompt missing")
	}
	if err := r.Store.Delivered(prompt.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(context.Background(), input(32, 10, 7, 1, "some follow-up")); err != nil {
		t.Fatal(err)
	}
	reply, ok := r.Store.Pending()
	if !ok || !strings.Contains(reply.Parts[0], "Shell is waiting") {
		t.Fatalf("expected waiting hint, got %+v", reply)
	}
	if err := r.Store.Delivered(reply.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(context.Background(), input(33, 10, 7, 1, "kill")); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got {
		t.Fatal("expected kill")
	}
}
