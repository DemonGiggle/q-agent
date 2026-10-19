package state

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"q-agent/internal/redaction"
)

func TestInterruptedHistoryBoundAndProvenance(t *testing.T) {
	for _, huge := range []string{strings.Repeat("中文\\\"\n", 20000), strings.Repeat("\x00", 20000), strings.Repeat("x", 40000)} {
		p := Pair{Interrupted: true, User: "original-head:" + huge + ":original-tail", Answer: "failed: " + huge, Steers: []string{"first " + huge, "last " + huge}, Progress: "observed " + huge}
		c := Conversation{Pairs: []Pair{{User: "older", Answer: "old answer"}, p}}
		c.Bound()
		if len(c.Pairs) == 0 {
			t.Fatal("oversized failure evicted itself")
		}
		got := c.Pairs[len(c.Pairs)-1]
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > MaxInterruptedPairBytes {
			t.Fatalf("pair=%d bytes", len(data))
		}
		data, _ = json.Marshal(c)
		if len(data) > MaxHistoryBytes {
			t.Fatalf("history=%d bytes", len(data))
		}
		if !strings.HasPrefix(got.User, "original-head:") || !strings.HasSuffix(got.User, ":original-tail") {
			t.Fatal("lost original head/tail")
		}
		for _, msg := range c.Messages() {
			if !utf8.ValidString(msg.Content) {
				t.Fatal("invalid UTF8")
			}
		}
		found := false
		for _, msg := range c.Messages() {
			if msg.Origin == "interrupted_task_tool_observations" {
				found = true
				if !msg.Untrusted || !strings.Contains(msg.Content, `"trust":"untrusted"`) || len(msg.ToolCalls) > 0 || msg.ToolCallID != "" {
					t.Fatal("unsafe progress replay")
				}
			}
		}
		if !found {
			t.Fatal("missing observation context")
		}
	}
}

func TestInterruptedHistoryRedactionAndRestart(t *testing.T) {
	policy, err := redaction.New(redaction.Config{}, []string{"test-private-credential"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := Open(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	mustSave := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mustSave(store.Save("topic", Conversation{Pairs: []Pair{{Interrupted: true, User: "original", Answer: "failed", Progress: "tool output: test-private-credential"}}}))
	store, err = Open(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.Load("topic")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 1 || !c.Pairs[0].Interrupted || strings.Contains(c.Pairs[0].Progress, "test-private-credential") || !strings.Contains(c.Pairs[0].Progress, redaction.Marker) {
		t.Fatalf("restart: %+v", c)
	}
	mustSave(store.Reset("topic"))
	c, err = store.Load("topic")
	if err != nil || len(c.Messages()) != 0 {
		t.Fatal("reset retained failed context")
	}
}
