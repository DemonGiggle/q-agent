package state

import (
	"strings"
	"testing"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

func TestSteeringHistoryPersistsRedactedInOrder(t *testing.T) {
	dir := t.TempDir()
	policy, _ := redaction.New(redaction.Config{}, []string{"private-credential-value"})
	store, err := Open(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	c := Conversation{Pairs: []Pair{{User: "original", Steers: []string{"first follow-up", "private-credential-value"}, Answer: "final"}}}
	if err = store.Save("topic", c); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	c, err = store.Load("topic")
	if err != nil {
		t.Fatal(err)
	}
	m := c.Messages()
	if len(m) != 4 || m[0].Content != "original" || m[1].Content != "first follow-up" || m[2].Content != redaction.Marker || m[3].Role != "assistant" || m[3].Content != "final" {
		t.Fatalf("history: %+v", m)
	}
	c.Pairs = append(c.Pairs, Pair{User: "too large", Steers: []string{strings.Repeat("x", MaxHistoryBytes)}, Answer: "final"})
	c.Bound()
	if len(c.Pairs) != 0 {
		t.Fatal("oversized steering was not bounded with its whole pair")
	}
}

func TestNoticeDoesNotSuppressFinalAnswer(t *testing.T) {
	store, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	route := platform.Message{Platform: "telegram", UpdateID: 2, Text: "private input"}
	if err = store.QueueNotice(route, []string{"received"}); err != nil {
		t.Fatal(err)
	}
	if err = store.Queue(route, []string{"final"}); err != nil {
		t.Fatal(err)
	}
	first, ok := store.Pending()
	if !ok || first.Parts[0] != "received" || first.Route.Text != "" {
		t.Fatalf("notice: %+v", first)
	}
	if err = store.Delivered(first.ID); err != nil {
		t.Fatal(err)
	}
	second, ok := store.Pending()
	if !ok || second.Parts[0] != "final" || second.ID == first.ID {
		t.Fatalf("final: %+v", second)
	}
}

func TestTaskNoticesPreserveMultipleInterimsAndFinal(t *testing.T) {
	store, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	route := platform.Message{Platform: "telegram", UpdateID: 2, Text: "request"}
	for i, text := range []string{"first authorization", "second authorization"} {
		if err = store.QueueTaskNotice(route, "", i, []string{text}); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Queue(route, []string{"final"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first authorization", "second authorization", "final"} {
		r, ok := store.Pending()
		if !ok || r.Parts[0] != text {
			t.Fatalf("missing %s: %+v", text, r)
		}
		if err = store.Delivered(r.ID); err != nil {
			t.Fatal(err)
		}
	}
}
