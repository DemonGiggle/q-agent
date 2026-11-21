package state

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"time"

	"q-agent/internal/learning"
)

// CollectConversationFrontier reads authorized turns and atomically updates
// only the private topic's candidate list. The caller owns the learning lock.
func (r *LearningRepository) CollectConversationFrontier(ctx context.Context, reader learning.ConversationReader,
	query learning.ConversationQuery, now time.Time, allow func(string) bool) ([]learning.FrontierEntry, error) {
	entries, err := r.collectConversationFrontier(ctx, reader, query, now, allow)
	if err != nil {
		return nil, err
	}
	result := make([]learning.FrontierEntry, 0)
	for _, entry := range entries {
		q := entry.Question
		if q.Status == "candidate" && q.Score != nil && allow(q.Text) &&
			(q.CooldownUntil == nil || !now.Before(*q.CooldownUntil)) {
			result = append(result, entry)
		}
	}
	slices.SortFunc(result, func(a, b learning.FrontierEntry) int {
		return cmp.Or(cmp.Compare(b.Question.Priority, a.Question.Priority),
			cmp.Compare(b.Question.Score.Final, a.Question.Score.Final),
			b.Question.UpdatedAt.Compare(a.Question.UpdatedAt), cmp.Compare(a.Question.ID, b.Question.ID))
	})
	return result, nil
}

func (r *LearningRepository) SelectConversationFrontier(ctx context.Context, reader learning.ConversationReader,
	query learning.ConversationQuery, now time.Time, allow func(string) bool, policy learning.SelectionConfig) ([]learning.SelectedCandidate, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	entries, err := r.collectConversationFrontier(ctx, reader, query, now, allow)
	if err != nil {
		return nil, err
	}
	return learning.SelectFrontier(entries, policy, now, allow), nil
}

func (r *LearningRepository) collectConversationFrontier(ctx context.Context, reader learning.ConversationReader,
	query learning.ConversationQuery, now time.Time, allow func(string) bool) ([]learning.FrontierEntry, error) {
	if reader == nil || allow == nil || query.Scope.TopicID == nil {
		return nil, learning.ErrDenied
	}
	key, err := learningPartitionKey(learning.Partition{Scope: &query.Scope})
	if err != nil {
		return nil, err
	}
	turns, err := reader.Recent(ctx, query)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load(key)
	if err != nil {
		return nil, err
	}
	entries, changed := learning.ConversationCandidates(data.Snapshot.Frontier, turns, now.UTC(), allow)
	if changed {
		data.Snapshot.Version++
		data.Snapshot.Frontier = entries
		if err := data.Snapshot.ValidateReferences(); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		if len(raw) > maxLearningPartitionBytes {
			return nil, ErrLearningPartitionFull
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := atomicBytes(filepath.Join(r.dir, key+".json"), raw); err != nil {
			return nil, err
		}
	}
	return entries, nil
}
