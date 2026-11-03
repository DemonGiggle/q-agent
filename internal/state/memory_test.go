package state

import (
	"os"
	"path/filepath"
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

func TestBoundMemoryDropsLowImportanceFirst(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key := "workspace"
	if _, err := s.AppendMemory(key, "2026-10-17", "old important", 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMemory(key, "2026-10-18", "x", 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMemory(key, "2026-10-19", "newest medium", 1<<20); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	loaded, err := s.loadMemory(key)
	if err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	loaded.Entries[0].Importance, loaded.Entries[0].Kind = 5, "preference"
	loaded.Entries[1].Importance, loaded.Entries[1].Kind = 1, "fact"
	loaded.Entries[2].Importance = 3
	// Budget fits the oldest and the newest, but not all three.
	s.boundMemory(&loaded, len("old important")+len("newest medium"))
	got := loaded
	s.mu.Unlock()
	if len(got.Entries) != 2 {
		t.Fatalf("entries=%+v", got.Entries)
	}
	for _, e := range got.Entries {
		if e.Text == "x" {
			t.Fatalf("trivia survived instead of newest: %+v", got.Entries)
		}
	}
}

func TestRememberPinnedSurvivesBudgetAndForgetByKeyword(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key := "workspace"
	if _, err := s.RememberPinned(key, "2026-10-19", "user prefers concise answers", 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMemory(key, "2026-10-19", "some trivia note here", 1<<20); err != nil {
		t.Fatal(err)
	}
	mem, err := s.LoadMemory(key)
	if err != nil {
		t.Fatal(err)
	}
	if mem.PinnedCount() != 1 {
		t.Fatalf("pinned=%+v", mem.Entries)
	}
	// Tight budget: trivia must go before the pinned note.
	s.mu.Lock()
	loaded, _ := s.loadMemory(key)
	s.boundMemory(&loaded, len("user prefers concise answers"))
	s.mu.Unlock()
	if len(loaded.Entries) != 1 || !loaded.Entries[0].Pinned {
		t.Fatalf("pinned dropped: %+v", loaded.Entries)
	}
	removed, err := s.ForgetByKeyword(key, "concise")
	if err != nil || removed != 1 {
		t.Fatalf("forget=%d err=%v", removed, err)
	}
	mem, _ = s.LoadMemory(key)
	if len(mem.Entries) != 1 || mem.PinnedCount() != 0 {
		t.Fatalf("after forget: %+v", mem.Entries)
	}
	if n, _ := s.ForgetByKeyword(key, "nothing matches this"); n != 0 {
		t.Fatalf("spurious removal: %d", n)
	}
}

func writeLegacyMemory(dir, key, data string) error {
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "memory", key+".json"), []byte(data), 0600)
}

func TestLegacyMemoryFileLoadsWithDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := writeLegacyMemory(dir, "workspace", `{"version":1,"epoch":1,"entries":[{"date":"2026-10-01","text":"legacy note"}]}`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := s.LoadMemory("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.Entries) != 1 || mem.Entries[0].Text != "legacy note" {
		t.Fatalf("legacy: %+v", mem.Entries)
	}
	if mem.Entries[0].EffectiveImportance() != 3 || mem.Entries[0].Pinned {
		t.Fatalf("defaults: %+v", mem.Entries[0])
	}
}
