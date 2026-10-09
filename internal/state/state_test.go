package state

import (
	"encoding/json"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"strings"
	"testing"
)

func TestHistoryBounds(t *testing.T) {
	c := Conversation{}
	for i := 0; i < 50; i++ {
		c.Pairs = append(c.Pairs, Pair{User: strings.Repeat("x", 2000), Answer: "y"})
	}
	c.Bound()
	data, _ := json.Marshal(c.Pairs)
	if len(c.Pairs) != 12 || len(data) > MaxHistoryBytes {
		t.Fatalf("%d %d", len(c.Pairs), len(data))
	}
	c.Pairs = append(c.Pairs, Pair{User: strings.Repeat("😀", 20000)})
	c.Bound()
	if len(c.Pairs) != 0 {
		t.Fatal("must trim whole oversized pairs")
	}
}
func TestRestartCursorHistoryOutbox(t *testing.T) {
	dir := t.TempDir()
	policy, _ := redaction.New(redaction.Config{}, []string{"credential"})
	s, err := Open(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, e := s.Accept(10); e != nil || !accepted {
		t.Fatal(e)
	}
	if accepted, _ := s.Accept(10); accepted {
		t.Fatal("duplicate accepted")
	}
	c := Conversation{Identity: "identity", Pairs: []Pair{{User: "credential", Answer: "answer"}}}
	if err = s.Save("chat:topic1", c); err != nil {
		t.Fatal(err)
	}
	if err = s.Queue(platform.Message{Platform: "telegram", ChatID: 1, UpdateID: 10, Text: "credential"}, []string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Pending()
	if r.Route.Text != "" {
		t.Fatal("prompt leaked to outbox")
	}
	if err = s.Delivered(r.ID); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cursor() != 10 {
		t.Fatal("cursor lost")
	}
	c, err = s.Load("chat:topic1")
	if err != nil || c.Identity != "identity" || c.Pairs[0].User != redaction.Marker {
		t.Fatalf("%+v %v", c, err)
	}
	other, _ := s.Load("chat:topic2")
	if len(other.Pairs) != 0 {
		t.Fatal("topics mixed")
	}
	r, _ = s.Pending()
	if r.Next != 1 {
		t.Fatal("delivery cursor lost")
	}
	if err = s.Delivered(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Pending(); ok {
		t.Fatal("outbox not cleared")
	}
}
func TestFailedDestinationDoesNotStarveOtherChats(t *testing.T) {
	s, _ := Open(t.TempDir(), nil)
	s.Queue(platform.Message{Platform: "telegram", UpdateID: 1, ChatID: 1}, []string{"failed"})
	s.Queue(platform.Message{Platform: "telegram", UpdateID: 2, ChatID: 2}, []string{"deliver"})
	first, _ := s.Pending()
	if err := s.DeferReply(first.ID); err != nil {
		t.Fatal(err)
	}
	next, _ := s.Pending()
	if next.Route.ChatID != 2 {
		t.Fatal("other chat is blocked by failed reply")
	}
}
