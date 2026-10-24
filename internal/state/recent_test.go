package state

import (
	"testing"
	"time"

	"q-agent/internal/platform"
)

func tapRoute(updateID, chat, topic, user int64) platform.Message {
	return platform.Message{Platform: "telegram", ChatID: chat, TopicID: topic, UserID: user, MessageID: updateID, UpdateID: updateID, Text: "tap"}
}

func TestRecentAppendSinceConsume(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.AppendRecent(tapRoute(1, -10, 7, 1), now.Add(-time.Hour), "hello group", 200, 32768); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRecent(tapRoute(2, -10, 7, 1), now, "second", 200, 32768); err != nil {
		t.Fatal(err)
	}
	// Dedup by UpdateID.
	if err := s.AppendRecent(tapRoute(2, -10, 7, 1), now, "second", 200, 32768); err != nil {
		t.Fatal(err)
	}
	route := tapRoute(1, -10, 7, 1)
	msgs, err := s.RecentSince(route.WorkspaceKey(), now.Add(-12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("recent=%d", len(msgs))
	}
	// Consume up to first message keeps second.
	if err := s.ConsumeRecent(route.WorkspaceKey(), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.RecentSince(route.WorkspaceKey(), now.Add(-12*time.Hour))
	if len(msgs) != 1 || msgs[0].Text != "second" {
		t.Fatalf("after consume: %+v", msgs)
	}
	// Expiry prune: old message disappears via Since filter, then Consume clears.
	oldRoute := tapRoute(2, -10, 7, 2)
	if err := s.AppendRecent(oldRoute, now.Add(-13*time.Hour), "expired", 200, 32768); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.RecentSince(route.WorkspaceKey(), now.Add(-12*time.Hour))
	for _, m := range msgs {
		if m.Text == "expired" {
			t.Fatal("expired visible")
		}
	}
	if err := s.ConsumeRecent(route.WorkspaceKey(), now.Add(-12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.RecentSince(route.WorkspaceKey(), now.Add(-12*time.Hour))
	if len(msgs) != 1 {
		t.Fatalf("expiry prune kept %d", len(msgs))
	}
	// Clear.
	if err := s.ClearRecent(route.WorkspaceKey()); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.RecentSince(route.WorkspaceKey(), now.Add(-12*time.Hour))
	if len(msgs) != 0 {
		t.Fatal("clear failed")
	}
	// Tap-only chat becomes visible to ListChats without LastSeen.
	tapOnly := tapRoute(3, -20, 0, 5)
	if err := s.AppendRecent(tapOnly, now, "first in new group", 200, 32768); err != nil {
		t.Fatal(err)
	}
	chats, err := s.ListChats()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range chats {
		if c.Key == tapOnly.WorkspaceKey() {
			found = true
			if !c.Record.LastSeen.IsZero() {
				t.Fatal("tap must not touch LastSeen")
			}
		}
	}
	if !found {
		t.Fatal("tap-only chat not listed")
	}
}

func TestRecentCapsBoundDisk(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	route := tapRoute(1, -10, 0, 1)
	now := time.Now().UTC()
	for i := 0; i < 50; i++ {
		r := tapRoute(int64(100+i), -10, 0, 1)
		if err := s.AppendRecent(r, now.Add(time.Duration(i)*time.Second), "x", 10, 1024); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := s.RecentSince(route.WorkspaceKey(), now.Add(-time.Hour))
	if len(msgs) != 10 {
		t.Fatalf("caps=%d", len(msgs))
	}
	// Persistence across reopen.
	s2, err := Open(s.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs2, _ := s2.RecentSince(route.WorkspaceKey(), now.Add(-time.Hour))
	if len(msgs2) != 10 {
		t.Fatalf("reopen caps=%d", len(msgs2))
	}
}
