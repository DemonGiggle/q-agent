package state

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
)

func TestCompanionAtomicQueueFailureAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := platform.Message{Platform: "telegram", ChatID: 1, UserID: 1}
	key := route.WorkspaceKey()
	id, err := s.StartCompanionRun(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write := s.atomic
	s.atomic = func(string, any) error { return fmt.Errorf("disk failure") }
	if err = s.QueueCompanion(key, id, "hash", "Hello", route, []string{"Hello"}); err == nil {
		t.Fatal("expected write failure")
	}
	if _, ok := s.Pending(); ok {
		t.Fatal("failed commit exposed a pending reply")
	}
	runs, _ := s.CompanionRuns(key)
	if !runs[0].SentAt.IsZero() {
		t.Fatal("failed write consumed send budget")
	}
	s.atomic = write
	if err = s.QueueCompanion(key, id, "hash", "Hello", route, []string{"Hello"}); err != nil {
		t.Fatal(err)
	}
	// Returned diagnostics cannot mutate a pending message.
	runs, _ = s.CompanionRuns(key)
	if runs[0].Reply != nil {
		t.Fatal("exposed mutable reply")
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := s.Pending()
	if !ok || reply.ID != id {
		t.Fatal("queued message lost on restart")
	}
	if err = s.Delivered(id); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok = s.Pending(); ok {
		t.Fatal("delivered message replayed")
	}
	runs, _ = s.CompanionRuns(key)
	if runs[0].Result != "delivered" || runs[0].DeliveredAt.IsZero() || runs[0].ContentHash != "hash" {
		t.Fatalf("%+v", runs)
	}
}

func TestCompanionUniqueDeliveryAcrossChatsAndInterruptedRun(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, chat := range []int64{1, 2} {
		route := platform.Message{Platform: "telegram", ChatID: chat, UserID: chat}
		key := route.WorkspaceKey()
		id, e := s.StartCompanionRun(key, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if e = s.QueueCompanion(key, id, "hash", "Hi", route, []string{"Hi"}); e != nil {
			t.Fatal(e)
		}
	}
	id, err := s.StartCompanionRun("unfinished", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := s.CompanionRuns("unfinished")
	if runs[0].ID != id || runs[0].Result != "interrupted" {
		t.Fatalf("%+v", runs)
	}
	seen := map[int64]bool{}
	first, _ := s.Pending()
	if err = s.DeferReply(first.ID); err != nil {
		t.Fatal(err)
	}
	other, _ := s.Pending()
	if first.ID == other.ID {
		t.Fatal("failed chat starved the other companion message")
	}
	for i := 0; i < 2; i++ {
		reply, ok := s.Pending()
		if !ok {
			t.Fatal("other chat suppressed by duplicate update 0")
		}
		if seen[reply.Route.ChatID] {
			t.Fatal("duplicate chat")
		}
		seen[reply.Route.ChatID] = true
		if !strings.HasPrefix(reply.ID, "companion:") {
			t.Fatal(reply.ID)
		}
		if err = s.Delivered(reply.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompanionDiagnosticPruningPreservesSendEvidence(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	runs := []CompanionRun{
		{ID: "older", Started: now.Add(-2 * time.Hour), SentAt: now.Add(-2 * time.Hour), Result: "delivered"},
		{ID: "latest", Started: now.Add(-time.Hour), SentAt: now.Add(-time.Hour), Result: "delivered", ContentHash: "do-not-repeat"},
	}
	for i := 0; i < 150; i++ {
		runs = append(runs, CompanionRun{ID: fmt.Sprint(i), Started: now, Result: "skipped"})
	}
	s.companionRuns = map[string][]CompanionRun{"chat": runs}
	if _, err = s.StartCompanionRun("chat", now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.CompanionRuns("chat")
	if len(got) != 128 {
		t.Fatalf("diagnostic retention unbounded: %d", len(got))
	}
	sent := 0
	for _, run := range got {
		if !run.SentAt.IsZero() {
			sent++
		}
	}
	if sent != 2 {
		t.Fatal("pruned rolling-day send evidence")
	}
	// Even after diagnostic retention expires, retain the latest check-in
	// to avoid repeating it in a chat with no new conversation.
	if _, err = s.StartCompanionRun("chat", now.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.CompanionRuns("chat")
	if len(got) != 2 || got[0].ContentHash != "do-not-repeat" {
		t.Fatalf("lost latest check-in: %+v", got)
	}
}

func TestCompanionEffectiveGlobalKillSwitch(t *testing.T) {
	for _, mode := range []string{"on", "off", "default", ""} {
		if CompanionEffective(false, CompanionPref{Mode: mode}) {
			t.Fatalf("kill switch bypassed by %s", mode)
		}
	}
	if !CompanionEffective(true, CompanionPref{Mode: "on"}) || CompanionEffective(true, CompanionPref{Mode: "off"}) {
		t.Fatal("per-chat preferences broken")
	}
}
