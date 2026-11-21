package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/state"
)

func TestLearningConversationReaderHostBindings(t *testing.T) {
	r, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	origin := input(1, -10, 7, 1, "")
	must(t, r.Store.AppendRecent(origin, now, "overheard-only marker", 8, 4096))
	must(t, r.Store.CommitExchange(origin, "update:telegram:1",
		state.Conversation{Pairs: []state.Pair{{User: "authorized task", Answer: "done"}}}, now))
	query := learning.ConversationQuery{
		Scope: learning.Scope{Platform: origin.Platform, ChatID: origin.ChatID, TopicID: &origin.TopicID, Visibility: learning.GroupChat},
		From:  now.Add(-48 * time.Hour), Until: now.Add(time.Minute),
	}
	reader := r.learningConversationReader(origin, learning.PermissionConfig{}, nil)
	if _, err := reader.Recent(ctx, query); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("group without learning opt-in: %v", err)
	}
	reader = r.learningConversationReader(origin, learning.PermissionConfig{IncludeGroupChats: true}, nil)
	turns, err := reader.Recent(ctx, query)
	if err != nil || len(turns) != 1 || !strings.Contains(turns[0].Transcript, "authorized task") ||
		strings.Contains(turns[0].Transcript, "overheard-only marker") {
		t.Fatalf("unexpected turns: %+v, %v", turns, err)
	}
	otherTopic := origin.TopicID + 1
	query.Scope.TopicID = &otherTopic
	if _, err := reader.Recent(ctx, query); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("different topic: %v", err)
	}
}
