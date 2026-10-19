package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

func journalExchange(t *testing.T, s *Store, route platform.Message, id, text string) {
	t.Helper()
	c, err := s.Load(route.ConversationKey())
	if err != nil {
		t.Fatal(err)
	}
	c.Pairs = append(c.Pairs, Pair{User: text, Steers: []string{"first steer", "second steer"}, Answer: "final answer"})
	if err := s.CommitExchange(route, id, c, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func journalResult(snapshot ChatMemory) ChatMemory {
	result := snapshot
	result.Entries = []MemoryEntry{{Date: "2026-10-19", Text: "durable note"}}
	return result
}

func TestJournalSurvivesHistoryBoundsResetsAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := testRoute(10, 7, 1)
	for i := 1; i <= 14; i++ {
		journalExchange(t, s, route, fmt.Sprint(i), fmt.Sprintf("exchange %d", i))
	}
	c, _ := s.Load(route.ConversationKey())
	mem, _ := s.LoadMemory(route.WorkspaceKey())
	if len(c.Pairs) != 12 || len(mem.Pending) != 14 || !strings.Contains(mem.Pending[13].Transcript, "exchange 14\nsteer: first steer\nsteer: second steer\nassistant: final answer") {
		t.Fatalf("bounds/steering: %+v, %+v", c, mem)
	}
	journalExchange(t, s, route, "huge", strings.Repeat("界🙂", 6000))
	c, _ = s.Load(route.ConversationKey())
	if len(c.Pairs) != 0 {
		t.Fatal("32 KiB eviction fixture failed")
	}
	otherTopic := testRoute(10, 8, 1)
	journalExchange(t, s, otherTopic, "topic8", "other topic")
	journalExchange(t, s, testRoute(20, 7, 2), "chat20", "isolated")
	if err := s.Reset(route.ConversationKey()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExecutionMode(ExecutionHost); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem, _ = s.LoadMemory(route.WorkspaceKey())
	if len(mem.Pending) != 16 || mem.NextSequence != 16 || mem.Pending[15].Topic != 8 {
		t.Fatalf("pending lost: %+v", mem)
	}
	other, _ := s.LoadMemory(testRoute(20, 7, 2).WorkspaceKey())
	if len(other.Pending) != 1 || strings.Contains(other.Pending[0].Transcript, "exchange 14") {
		t.Fatal("chat isolation failed")
	}
	chats, err := s.ListChats() // durable routes recover discovery without chats.json
	if err != nil || len(chats) != 2 {
		t.Fatalf("journal discovery: %+v %v", chats, err)
	}
}

func TestExchangeFaultRecoveryAtEveryWrite(t *testing.T) {
	for fail := 1; fail <= 3; fail++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("write%d/after%v", fail, after), func(t *testing.T) {
				dir := t.TempDir()
				s, err := Open(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				route := testRoute(10, 7, 1)
				c := Conversation{Pairs: []Pair{{User: "question", Answer: "answer"}}}
				writes := 0
				s.atomic = func(path string, value any) error {
					writes++
					if writes == fail && !after {
						return errors.New("injected write failure")
					}
					err := Atomic(path, value)
					if writes == fail && after && err == nil {
						return errors.New("injected sync failure after rename")
					}
					return err
				}
				if err := s.CommitExchange(route, "task:1", c, time.Now()); err == nil {
					t.Fatal("fault did not fire")
				}
				s.atomic = Atomic
				// Restart and same-process recovery must both work, without rerunning work.
				if after || fail > 1 {
					if fail%2 == 0 {
						s, err = Open(dir, nil)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err = s.Load(route.ConversationKey()); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := s.CommitExchange(route, "task:1", c, time.Now()); err != nil {
					t.Fatal(err)
				}
				mem, _ := s.LoadMemory(route.WorkspaceKey())
				history, _ := s.Load(route.ConversationKey())
				if len(mem.Pending) != 1 || mem.Intent != nil || len(history.Pairs) != 1 {
					t.Fatalf("incomplete/duplicate recovery: %+v %+v", mem, history)
				}
				if err := s.Reset(route.ConversationKey()); err != nil {
					t.Fatal(err)
				}
				s, err = Open(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				history, _ = s.Load(route.ConversationKey())
				if len(history.Pairs) != 0 {
					t.Fatal("replay resurrected reset history")
				}
			})
		}
	}
}

func TestDigestTransactionFaultsAndDurableOutbox(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			dir := t.TempDir()
			s, _ := Open(dir, nil)
			route := testRoute(10, 0, 1)
			journalExchange(t, s, route, "1", "first")
			snap, _ := s.SnapshotDigest(route.WorkspaceKey())
			s.atomic = func(path string, value any) error {
				if after {
					if err := Atomic(path, value); err != nil {
						return err
					}
				}
				return errors.New("injected digest failure")
			}
			err := s.CommitDigest(route.WorkspaceKey(), snap, journalResult(snap), 1, route, []string{"notice part 1", "notice part 2"}, time.Now(), 8192)
			if err == nil {
				t.Fatal("fault did not fire")
			}
			s.atomic = Atomic
			s, err = Open(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			mem, _ := s.LoadMemory(route.WorkspaceKey())
			if after {
				if len(mem.Pending) != 0 || mem.Cursor != 1 || len(mem.Entries) != 1 || len(mem.Notices) != 1 {
					t.Fatalf("split transaction: %+v", mem)
				}
				if err := s.CommitDigest(route.WorkspaceKey(), snap, journalResult(snap), 1, route, []string{"duplicate"}, time.Now(), 8192); !errors.Is(err, ErrStaleDigest) {
					t.Fatalf("old commit: %v", err)
				}
			} else {
				if len(mem.Pending) != 1 || mem.Cursor != 0 || len(mem.Entries) != 0 || len(mem.Notices) != 0 {
					t.Fatalf("partial failed commit: %+v", mem)
				}
				if err := s.CommitDigest(route.WorkspaceKey(), snap, journalResult(snap), 1, route, []string{"notice part 1", "notice part 2"}, time.Now(), 8192); err != nil {
					t.Fatal(err)
				}
			}
			reply, ok := s.Pending()
			if !ok || !strings.HasPrefix(reply.ID, "memory:") {
				t.Fatal("missing durable notification")
			}
			if err := s.DeferReply(reply.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.Delivered(reply.ID); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			resumed, ok := s.Pending()
			if !ok || resumed.ID != reply.ID || resumed.Next != 1 {
				t.Fatalf("delivery did not resume: %+v", resumed)
			}
			if err := s.Delivered(resumed.ID); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.Pending(); ok {
				t.Fatal("duplicate notice")
			}
		})
	}
}

func TestConditionalDigestPreservesAppendsAndRejectsRaces(t *testing.T) {
	s, _ := Open(t.TempDir(), nil)
	route := testRoute(10, 0, 1)
	key := route.WorkspaceKey()
	journalExchange(t, s, route, "1", "first")
	snap, _ := s.SnapshotDigest(key)
	journalExchange(t, s, route, "2", "appended during model execution")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.CommitDigest(key, snap, journalResult(snap), 1, route, []string{"notice"}, time.Now(), 8192)
		}()
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStaleDigest) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	mem, _ := s.LoadMemory(key)
	if success != 1 || stale != 1 || len(mem.Pending) != 1 || mem.Pending[0].ID != "2" {
		t.Fatalf("overlap/appends: %d %d %+v", success, stale, mem)
	}
	snap, _ = s.SnapshotDigest(key)
	if err := s.ClearMemory(key); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitDigest(key, snap, journalResult(snap), 1, route, []string{"old"}, time.Now(), 8192); !errors.Is(err, ErrStaleDigest) {
		t.Fatal("forget allowed stale commit")
	}
	journalExchange(t, s, route, "3", "task finished after forget")
	mem, _ = s.LoadMemory(key)
	if len(mem.Pending) != 1 || mem.Pending[0].Sequence != 3 {
		t.Fatalf("completion eligibility: %+v", mem)
	}
}

func TestJournalOverflowEpisodesAndEventRetention(t *testing.T) {
	s, _ := Open(t.TempDir(), nil, MemoryLimits{MaxPendingBytes: 800})
	route := testRoute(10, 0, 1)
	key := route.WorkspaceKey()
	for i := 0; i < 8; i++ {
		journalExchange(t, s, route, fmt.Sprint(i), strings.Repeat("界🙂", 30))
	}
	mem, _ := s.LoadMemory(key)
	if mem.PendingBytes() > 800 || mem.Loss.LostEvents == 0 || mem.Loss.LostBytes == 0 || len(mem.Notices) != 1 {
		t.Fatalf("overflow policy: %+v", mem)
	}
	snap := mem
	journalExchange(t, s, route, "next", strings.Repeat("界🙂", 30))
	if err := s.CommitDigest(key, snap, journalResult(snap), 1, route, []string{"old"}, time.Now(), 8192); !errors.Is(err, ErrStaleDigest) {
		t.Fatal("eviction allowed stale digest")
	}
	mem, _ = s.LoadMemory(key)
	if err := s.CommitDigest(key, mem, journalResult(mem), len(mem.Pending), route, []string{"digest"}, time.Now(), 8192); err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 16; i++ {
		journalExchange(t, s, route, fmt.Sprint(i), strings.Repeat("界🙂", 30))
	}
	mem, _ = s.LoadMemory(key)
	warnings := 0
	for _, notice := range mem.Notices {
		if strings.Contains(notice.ID, ":overflow:") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatalf("warning episodes: %d", warnings)
	}
	large, _ := Open(t.TempDir(), nil)
	journalExchange(t, large, route, "huge", "HEAD"+strings.Repeat("界🙂<&>\n", 60000)+"TAIL")
	mem, _ = large.LoadMemory(key)
	event := mem.Pending[0]
	if encodedSize(event) > MaxSourceBytes || !utf8.ValidString(event.Transcript) || !strings.Contains(event.Transcript, sourceOmission) || !strings.Contains(event.Transcript, "HEAD") || !strings.Contains(event.Transcript, "TAIL") || mem.Loss.TruncatedEvents != 1 || mem.Loss.TruncatedBytes == 0 {
		t.Fatalf("exceptional retention: size=%d loss=%+v", encodedSize(event), mem.Loss)
	}
}

func TestMigrationNormalizesOnceWithoutImportingHistory(t *testing.T) {
	dir := t.TempDir()
	route := testRoute(10, 7, 1)
	key := route.WorkspaceKey()
	s, _ := Open(dir, nil)
	if err := s.Save(route.ConversationKey(), Conversation{Pairs: []Pair{{User: "legacy pending", Answer: "legacy answer"}}}); err != nil {
		t.Fatal(err)
	}
	old := map[string]any{"entries": []MemoryEntry{{Date: "old", Text: "old note"}, {Date: "new", Text: strings.Repeat("界🙂", 30)}}, "digested": map[string]int{route.ConversationKey(): 12}}
	if err := Atomic(s.memoryPath(key), old); err != nil {
		t.Fatal(err)
	}
	var before []byte
	for i := 0; i < 3; i++ {
		var err error
		s, err = Open(dir, nil, MemoryLimits{MaxBytes: 13})
		if err != nil {
			t.Fatal(err)
		}
		mem, err := s.LoadMemory(key)
		if err != nil {
			t.Fatal(err)
		}
		if !mem.LegacyBaseline || mem.Bytes() > 13 || len(mem.Pending) != 0 || mem.NextSequence != 0 || !utf8.ValidString(mem.Entries[0].Text) || mem.Loss.DroppedNoteBytes == 0 {
			t.Fatalf("migration: %+v", mem)
		}
		if err := s.ReportMemoryMigration(key, route); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(s.memoryPath(key))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "digested") {
			t.Fatal("legacy count cursor retained")
		}
		if i > 0 && string(data) != string(before) {
			t.Fatal("migration changed on restart")
		}
		before = data
		c, _ := s.Load(route.ConversationKey())
		if len(c.Pairs) != 1 {
			t.Fatal("migration changed history")
		}
	}
	mem, _ := s.LoadMemory(key)
	if len(mem.Notices) != 1 {
		t.Fatal("migration notification duplicated")
	}
	// A later reduction of the configured budget is enforced at startup too.
	s, err := Open(dir, nil, MemoryLimits{MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	mem, _ = s.LoadMemory(key)
	if mem.Bytes() > 3 {
		t.Fatal("budget reduction ignored")
	}
}

func TestPersistenceRedactionPrecedesUTF8Budget(t *testing.T) {
	policy, err := redaction.New(redaction.Config{}, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := Open(t.TempDir(), policy)
	for _, budget := range []int{1, 2, 3, 4, 9, 10, 13, 20} {
		key := fmt.Sprint(budget)
		mem, err := s.AppendMemory(key, "date", "x界🙂x", budget)
		if err != nil {
			t.Fatal(err)
		}
		if mem.Bytes() > budget {
			t.Fatalf("redaction exceeded %d: %+v", budget, mem)
		}
		data, err := os.ReadFile(filepath.Join(s.dir, "memory", key+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var disk ChatMemory
		if err := json.Unmarshal(data, &disk); err != nil {
			t.Fatal(err)
		}
		for _, entry := range disk.Entries {
			if !utf8.ValidString(entry.Text) || strings.Contains(entry.Text, "x") {
				t.Fatalf("invalid redacted entry %q", entry.Text)
			}
		}
	}
}

func TestResetRecoversUnfinishedExchangeBeforeClearingHistory(t *testing.T) {
	for _, modeChange := range []bool{false, true} {
		t.Run(fmt.Sprint(modeChange), func(t *testing.T) {
			dir := t.TempDir()
			s, _ := Open(dir, nil)
			route := testRoute(10, 7, 1)
			s.atomic = func(path string, value any) error {
				if strings.Contains(path, "conversations") {
					return errors.New("injected history failure")
				}
				return Atomic(path, value)
			}
			err := s.CommitExchange(route, "1", Conversation{Pairs: []Pair{{User: "pending intent", Answer: "final"}}}, time.Now())
			if err == nil {
				t.Fatal("failure fixture did not fire")
			}
			s.atomic = Atomic
			if modeChange {
				err = s.SetExecutionMode(ExecutionHost)
			} else {
				err = s.Reset(route.ConversationKey())
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			c, err := s.Load(route.ConversationKey())
			if err != nil || len(c.Pairs) != 0 {
				t.Fatalf("reset history resurrected: %+v %v", c, err)
			}
			mem, err := s.LoadMemory(route.WorkspaceKey())
			if err != nil || len(mem.Pending) != 1 || mem.Intent != nil {
				t.Fatalf("reset lost source: %+v %v", mem, err)
			}
		})
	}
}

func TestMemoryNotificationFailureAllowsOtherDeliveries(t *testing.T) {
	s, _ := Open(t.TempDir(), nil)
	for _, chat := range []int64{10, 20} {
		route := testRoute(chat, 0, 1)
		journalExchange(t, s, route, "1", "source")
		snap, _ := s.SnapshotDigest(route.WorkspaceKey())
		if err := s.CommitDigest(route.WorkspaceKey(), snap, journalResult(snap), 1, route, []string{"notice"}, time.Now(), 8192); err != nil {
			t.Fatal(err)
		}
	}
	route := testRoute(10, 0, 1)
	route.UpdateID = 1
	if err := s.Queue(route, []string{"answer 1"}); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Pending()
	if err := s.Delivered(first.ID); err != nil {
		t.Fatal(err)
	}
	notice, _ := s.Pending()
	if !strings.HasPrefix(notice.ID, "memory:") {
		t.Fatal("memory did not get delivery turn")
	}
	if err := s.DeferReply(notice.ID); err != nil {
		t.Fatal(err)
	}
	route.UpdateID = 2
	if err := s.Queue(route, []string{"answer 2"}); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Pending()
	if second.ID == notice.ID || second.Parts[0] != "answer 2" {
		t.Fatal("failed memory notification blocked interactive reply")
	}
	if err := s.Delivered(second.ID); err != nil {
		t.Fatal(err)
	}
	other, _ := s.Pending()
	if other.Route.ChatID == notice.Route.ChatID || !strings.HasPrefix(other.ID, "memory:") {
		t.Fatal("failed chat blocked another chat's notice")
	}
}

func TestStartupRejectsUnreadableMemoryDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, nil); err == nil {
		t.Fatal("startup skipped journal discovery failure")
	}
}

func TestCorruptMemoryQuarantinedWithoutBlockingStartup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	good := testRoute(10, 7, 1)
	journalExchange(t, s, good, "1", "good source")
	goodKey := good.WorkspaceKey()
	badKey := "badchat"
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", badKey+".json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatalf("corrupt chat blocked startup: %v", err)
	}
	mem, err := s.LoadMemory(goodKey)
	if err != nil || len(mem.Pending) != 1 {
		t.Fatalf("good chat lost: %+v %v", mem, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	quarantined := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), badKey+".corrupt-") {
			quarantined = true
		}
		if e.Name() == badKey+".json" {
			t.Fatal("corrupt file still in place")
		}
	}
	if !quarantined {
		t.Fatal("corrupt file not quarantined")
	}
	fresh, err := s.LoadMemory(badKey)
	if err != nil || len(fresh.Pending) != 0 || len(fresh.Entries) != 0 {
		t.Fatalf("quarantined chat not fresh: %+v %v", fresh, err)
	}
}
