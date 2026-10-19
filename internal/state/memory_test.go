package state

import (
	"testing"
	"time"

	"q-agent/internal/platform"
)

func testRoute(chat, topic, user int64) platform.Message {
	return platform.Message{Platform: "telegram", ChatID: chat, TopicID: topic, UserID: user}
}

func TestChatRegistryRecordAndList(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChat(testRoute(10, 0, 1), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChat(testRoute(10, 7, 1), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChat(testRoute(20, 0, 2), time.Now()); err != nil {
		t.Fatal(err)
	}
	chats, err := s.ListChats()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("chats=%d", len(chats))
	}
	for _, c := range chats {
		if c.Record.ChatID == 10 {
			if len(c.Record.Topics) != 2 || c.Record.Topics[0] != 0 || c.Record.Topics[1] != 7 {
				t.Fatalf("topics: %+v", c.Record)
			}
			if c.Record.Owner != 1 {
				t.Fatalf("owner: %+v", c.Record)
			}
		}
	}
	// Re-recording the same topic must not duplicate it.
	if err := s.RecordChat(testRoute(10, 7, 1), time.Now()); err != nil {
		t.Fatal(err)
	}
	chats, _ = s.ListChats()
	for _, c := range chats {
		if c.Record.ChatID == 10 && len(c.Record.Topics) != 2 {
			t.Fatalf("duplicate topic: %+v", c.Record)
		}
	}
}

func TestMemoryAppendCap(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := "workspace"
	if _, err := s.AppendMemory(key, "2026-10-17", "first note", 20); err != nil {
		t.Fatal(err)
	}
	mem, err := s.AppendMemory(key, "2026-10-18", "second note!", 20)
	if err != nil {
		t.Fatal(err)
	}
	if mem.Bytes() > 20 {
		t.Fatalf("over cap: %d", mem.Bytes())
	}
	if len(mem.Entries) != 1 || mem.Entries[0].Text != "second note!" {
		t.Fatalf("oldest not dropped: %+v", mem.Entries)
	}
}

func TestClearMemoryAdvancesEpoch(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	route := testRoute(10, 7, 1)
	if err := s.CommitExchange(route, "task:1", Conversation{Pairs: []Pair{{User: "question", Answer: "answer"}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := s.LoadMemory(route.WorkspaceKey())
	if _, err := s.AppendMemory(route.WorkspaceKey(), "2026-10-17", "note", 20); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearMemory(route.WorkspaceKey()); err != nil {
		t.Fatal(err)
	}
	mem, err := s.LoadMemory(route.WorkspaceKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.Entries) != 0 || len(mem.Pending) != 0 || mem.Epoch <= before.Epoch {
		t.Fatalf("forget: %+v", mem)
	}
	conv, err := s.Load(route.ConversationKey())
	if err != nil || len(conv.Pairs) != 1 {
		t.Fatalf("history lost: %+v, %v", conv, err)
	}
}

func TestLoadMemoryMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := s.LoadMemory("absent")
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.Entries) != 0 || mem.Bytes() != 0 {
		t.Fatalf("nonempty: %+v", mem)
	}
}
