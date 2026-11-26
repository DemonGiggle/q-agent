package runtime

import (
	"context"
	"errors"
	"slices"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/state"
)

func (r *Runtime) discoverLearningTopics(ctx context.Context, now time.Time, chats []state.ChatRecordWithKey,
	allow func(string) bool) ([]string, error) {
	repo := state.NewLearningRepository(r.Config.StateDir, nil)
	permissions := r.Learning.Permissions
	from := now.Add(-state.ConversationFrontierLookback)
	slices.SortStableFunc(chats, func(a, b state.ChatRecordWithKey) int {
		return b.Record.LastSeen.Compare(a.Record.LastSeen)
	})
	for _, chat := range chats {
		route := chat.WorkspaceRoute()
		if !r.allowed(route) {
			continue
		}
		visibility := learning.PrivateChat
		if route.ChatID < 0 {
			visibility = learning.GroupChat
		}
		if (visibility == learning.PrivateChat && !permissions.IncludePrivateChats) ||
			(visibility == learning.GroupChat && !permissions.IncludeGroupChats) {
			continue
		}
		topics := chat.Record.Topics
		if len(topics) == 0 {
			topics = []int64{0}
		}
		var latest []learning.ConversationTurn
		for _, topic := range topics {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if permissions.ExcludesTopic(route.ChatID, topic) {
				continue
			}
			route.TopicID = topic
			scope := learning.Scope{Platform: route.Platform, ChatID: route.ChatID, TopicID: &topic, Visibility: visibility}
			reader := r.learningConversationReader(route, permissions, func(_ learning.Scope, turn learning.ConversationTurn) bool {
				return !allow(turn.Transcript)
			})
			turns, err := repo.RecentLearningTurns(ctx, reader, learning.ConversationQuery{Scope: scope, From: from, Until: now}, now)
			if errors.Is(err, learning.ErrDenied) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if len(turns) != 0 && (len(latest) == 0 || turns[len(turns)-1].At.After(latest[len(latest)-1].At)) {
				latest = turns
			}
		}
		if len(latest) != 0 {
			return r.LearningTopics(ctx, topicDiscoveryExchanges(latest))
		}
	}
	return nil, nil
}

// Keep complete exchanges where possible. The newest turns get priority
// when the history exceeds the local model's input budget.
func topicDiscoveryExchanges(turns []learning.ConversationTurn) []string {
	const maxExchanges = 24
	const maxExchangeBytes = 1600
	remaining := 6400
	exchanges := make([]string, 0, min(maxExchanges, len(turns)))
	for i := len(turns) - 1; i >= 0 && len(exchanges) < maxExchanges; i-- {
		text := state.UTF8Prefix(turns[i].Transcript, min(remaining, maxExchangeBytes))
		if text == "" {
			break
		}
		exchanges = append(exchanges, text)
		remaining -= len(text)
	}
	slices.Reverse(exchanges)
	return exchanges
}
