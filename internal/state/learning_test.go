package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

func TestLearningReaderScopeAndDigest(t *testing.T) {
	off := false
	policy, err := redaction.New(redaction.Config{Persistence: &off}, []string{"learning-secret-12345"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir(), policy)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	route := platform.Message{Platform: "telegram", ChatID: -101, TopicID: 7}
	commit := func(m platform.Message, id, text string, at time.Time) {
		t.Helper()
		c, err := s.Load(m.ConversationKey())
		if err != nil {
			t.Fatal(err)
		}
		c.Pairs = append(c.Pairs, Pair{User: text, Answer: "final", Progress: "internal tool output"})
		if err := s.CommitExchange(m, id, c, at); err != nil {
			t.Fatal(err)
		}
	}
	other := route
	other.TopicID = 9
	commit(other, "update:telegram:1", "other topic", now)
	other = route
	other.ChatID = -102
	commit(other, "update:telegram:2", "other chat", now)
	commit(route, "update:telegram:3", "learning-secret-12345 good", now)
	commit(route, "schedule:run", "scheduled", now)
	commit(route, "update:telegram:4", "excluded", now)
	commit(route, "update:telegram:5", "outside window", now.Add(-time.Hour))
	if err := s.RecordFailure(route, "update:telegram:6:failed", "failed", "internal error", now); err != nil {
		t.Fatal(err)
	}
	reader := s.NewLearningReader(func(_ context.Context, scope learning.Scope) bool {
		return scope.ChatID == route.ChatID && scope.TopicID != nil && *scope.TopicID == route.TopicID
	}, func(_ learning.Scope, turn learning.ConversationTurn) bool {
		return strings.Contains(turn.Transcript, "excluded")
	})
	query := learning.ConversationQuery{
		Scope: learning.Scope{Platform: route.Platform, ChatID: route.ChatID, TopicID: &route.TopicID, Visibility: learning.GroupChat},
		From:  now, Until: now.Add(time.Minute),
	}
	readOne := func() learning.ConversationTurn {
		t.Helper()
		turns, err := reader.Recent(ctx, query)
		if err != nil || len(turns) != 1 {
			t.Fatalf("got %+v, err=%v", turns, err)
		}
		if want := "user: [REDACTED] good\nassistant: final\n"; turns[0].Transcript != want {
			t.Fatalf("got %q, want %q", turns[0].Transcript, want)
		}
		return turns[0]
	}
	before := readOne()
	snap, err := s.SnapshotDigest(route.WorkspaceKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitDigest(route.WorkspaceKey(), snap, snap, len(snap.Pending), route, nil, now, 8192); err != nil {
		t.Fatal(err)
	}
	if after := readOne(); before != after {
		t.Fatalf("digest changed origin: before=%+v, after=%+v", before, after)
	}

	for _, scope := range []learning.Scope{
		{Platform: route.Platform, ChatID: other.ChatID, TopicID: &route.TopicID, Visibility: learning.GroupChat},
		{Platform: route.Platform, ChatID: route.ChatID, TopicID: &other.TopicID, Visibility: learning.PrivateChat},
		{Platform: route.Platform, ChatID: route.ChatID, Visibility: learning.GroupChat},
	} {
		outside := query
		outside.Scope = scope
		if _, err := reader.Recent(ctx, outside); !errors.Is(err, learning.ErrDenied) {
			t.Fatalf("scope %+v returned %v", scope, err)
		}
	}
	if err := s.ClearMemory(route.WorkspaceKey()); err != nil {
		t.Fatal(err)
	}
	if turns, err := reader.Recent(ctx, query); err != nil || len(turns) != 0 {
		t.Fatalf("cleared memory returned %+v, err=%v", turns, err)
	}
}
