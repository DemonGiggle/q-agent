package state

import (
	"fmt"
	"os"
	"path/filepath"
	"q-agent/internal/platform"
	"testing"
)

func TestExecutionModeTransitions(t *testing.T) {
	for _, initial := range []string{"", ExecutionSandbox, ExecutionHost} {
		for _, next := range []string{ExecutionHost, ExecutionSandbox} {
			t.Run(fmt.Sprintf("%s-to-%s", initial, next), func(t *testing.T) {
				dir := t.TempDir()
				s, err := Open(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				if initial != "" {
					if err := s.SetExecutionMode(initial); err != nil {
						t.Fatal(err)
					}
				}
				keys := []string{"chat1:topic1", "chat1:topic2", "chat2:topic1"}
				for _, key := range keys {
					if err := s.Save(key, Conversation{Provider: "opencode-go", Model: "model", Identity: "identity", Pairs: []Pair{{User: "question", Answer: "answer"}}}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.Accept(42); err != nil {
					t.Fatal(err)
				}
				if err := s.Queue(platform.Message{Platform: "telegram", ChatID: 1, UpdateID: 42}, []string{"one", "two"}); err != nil {
					t.Fatal(err)
				}
				reply, _ := s.Pending()
				if err := s.Delivered(reply.ID); err != nil {
					t.Fatal(err)
				}
				for _, tree := range []string{"workspaces", "host-workspaces"} {
					if err := Atomic(filepath.Join(dir, tree, "marker.json"), "keep"); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.SetExecutionMode(next); err != nil {
					t.Fatal(err)
				}
				s, err = Open(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				// A same-mode restart must preserve the state left by the transition.
				if err := s.SetExecutionMode(next); err != nil {
					t.Fatal(err)
				}
				prior := initial
				if prior == "" {
					prior = ExecutionSandbox
				}
				for _, key := range keys {
					c, err := s.Load(key)
					if err != nil {
						t.Fatal(err)
					}
					if prior == next {
						if c.Identity != "identity" || len(c.Pairs) != 1 || c.Provider != "opencode-go" {
							t.Fatalf("same mode lost state: %+v", c)
						}
					} else if c.Identity != "" || len(c.Pairs) != 0 || c.Provider != "" || c.Model != "" {
						t.Fatalf("old mode state survived: %+v", c)
					}
				}
				if s.Cursor() != 42 {
					t.Fatal("cursor changed")
				}
				reply, ok := s.Pending()
				if !ok || reply.Next != 1 || reply.Parts[1] != "two" {
					t.Fatal("outbox changed")
				}
				for _, tree := range []string{"workspaces", "host-workspaces"} {
					data, err := os.ReadFile(filepath.Join(dir, tree, "marker.json"))
					if err != nil || string(data) != `"keep"` {
						t.Fatal("workspace files changed")
					}
				}
			})
		}
	}
}

func TestExecutionResetFailureRetries(t *testing.T) {
	for _, initial := range []string{"", ExecutionSandbox, ExecutionHost} {
		t.Run(initial, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if initial != "" {
				if err := s.SetExecutionMode(initial); err != nil {
					t.Fatal(err)
				}
			}
			next := ExecutionHost
			if initial == ExecutionHost {
				next = ExecutionSandbox
			}
			// Sorted entries cause one completed reset before the failure.
			first := filepath.Join(dir, "conversations", "a.json")
			last := filepath.Join(dir, "conversations", "z.json")
			if err := Atomic(first, Conversation{Identity: "first"}); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(last, 0700); err != nil {
				t.Fatal(err)
			}
			before, beforeErr := os.ReadFile(filepath.Join(dir, "execution.json"))
			if err := s.SetExecutionMode(next); err == nil {
				t.Fatal("incomplete reset accepted")
			}
			after, afterErr := os.ReadFile(filepath.Join(dir, "execution.json"))
			if string(before) != string(after) || os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) {
				t.Fatal("mode committed before successful reset")
			}
			var cleared Conversation
			if err := read(first, &cleared); err != nil || cleared.Identity != "" {
				t.Fatal("failure fixture did not interrupt a partial reset")
			}
			if err := os.Remove(last); err != nil {
				t.Fatal(err)
			}
			if err := Atomic(last, Conversation{Identity: "remaining", Pairs: []Pair{{User: "old"}}}); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetExecutionMode(next); err != nil {
				t.Fatal(err)
			}
			if err := read(last, &cleared); err != nil || cleared.Identity != "" || len(cleared.Pairs) != 0 {
				t.Fatal("restart did not finish reset")
			}
		})
	}
}

func TestExecutionModeMetadataValidation(t *testing.T) {
	for _, text := range []string{`{}`, `null`, `{"mode":""}`, `{"mode":"unknown"}`, `{"mode":true}`, `{`} {
		t.Run(text, func(t *testing.T) {
			dir := t.TempDir()
			s, _ := Open(dir, nil)
			if err := os.WriteFile(filepath.Join(dir, "execution.json"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.SetExecutionMode(ExecutionHost); err == nil {
				t.Fatal("invalid mode metadata accepted")
			}
		})
	}
}

func TestExecutionModeCommitFailureRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary directory permissions")
	}
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetExecutionMode(ExecutionSandbox); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("topic", Conversation{Identity: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if err := s.SetExecutionMode(ExecutionHost); err == nil {
		t.Fatal("mode commit failure accepted")
	}
	c, err := s.Load("topic")
	if err != nil || c.Identity != "" {
		t.Fatal("fixture did not fail after clearing histories")
	}
	data, err := os.ReadFile(filepath.Join(dir, "execution.json"))
	if err != nil || string(data) != `{"mode":"sandbox"}` {
		t.Fatal("failed commit replaced old mode")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("topic", Conversation{Identity: "still-old"}); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetExecutionMode(ExecutionHost); err != nil {
		t.Fatal(err)
	}
	c, err = s.Load("topic")
	if err != nil || c.Identity != "" {
		t.Fatal("restart did not repeat reset")
	}
}
