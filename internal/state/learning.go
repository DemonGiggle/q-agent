package state

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

type ExchangeOrigin struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Epoch uint64    `json:"epoch"`
}

type learningReader struct {
	store     *Store
	authorize func(context.Context, learning.Scope) bool
	excluded  func(learning.Scope, learning.ConversationTurn) bool
}

// NewLearningReader keeps chat storage behind a mandatory, caller-bound ACL.
// The optional filter applies topic/content exclusions to redacted turns.
func (s *Store) NewLearningReader(authorize func(context.Context, learning.Scope) bool, excluded func(learning.Scope, learning.ConversationTurn) bool) learning.ConversationReader {
	return &learningReader{store: s, authorize: authorize, excluded: excluded}
}

func interactiveSource(route platform.Message, sourceID string) bool {
	id, ok := strings.CutPrefix(sourceID, "update:"+route.Platform+":")
	if !ok {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 63)
	return err == nil
}

func (r *learningReader) Recent(ctx context.Context, query learning.ConversationQuery) ([]learning.ConversationTurn, error) {
	scope := query.Scope
	if scope.TopicID != nil {
		topic := *scope.TopicID
		scope.TopicID = &topic
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scope.Platform == "" || scope.ChatID == 0 ||
		(scope.ChatID > 0 && scope.Visibility != learning.PrivateChat) ||
		(scope.ChatID < 0 && scope.Visibility != learning.GroupChat) ||
		r.authorize == nil || !r.authorize(ctx, scope) {
		return nil, learning.ErrDenied
	}
	if query.From.IsZero() || !query.From.Before(query.Until) {
		return nil, fmt.Errorf("learning requires a valid time window")
	}
	route := platform.Message{Platform: scope.Platform, ChatID: scope.ChatID}
	mem, conv, recent, err := r.store.learningSnapshot(route, scope.TopicID)
	if err != nil {
		return nil, err
	}
	var turns []learning.ConversationTurn
	seen := make(map[string]bool)
	add := func(id string, topic int64, at time.Time, transcript string) {
		if seen[id] || at.Before(query.From) || !at.Before(query.Until) ||
			(scope.TopicID != nil && *scope.TopicID != topic) || !interactiveSource(route, id) {
			return
		}
		seen[id] = true
		turn := learning.ConversationTurn{
			OriginRef: platform.WorkspaceKey(route.WorkspaceKey()+":"+id, topic),
			TopicID:   topic, At: at, Transcript: r.store.policy.Redact(transcript),
		}
		if r.excluded == nil || !r.excluded(scope, turn) {
			turns = append(turns, turn)
		}
	}
	for _, source := range mem.Pending {
		add(source.ID, source.Topic, source.At, source.Transcript)
	}
	if scope.TopicID != nil {
		for _, pair := range conv.Pairs {
			if pair.Learning != nil && pair.Learning.Epoch == mem.Epoch && !pair.Interrupted {
				add(pair.Learning.ID, *scope.TopicID, pair.Learning.At, sourceTranscript(pair))
			}
		}
	}
	for _, msg := range recent {
		if msg.Learning && msg.UpdateID > 0 && !msg.At.Before(query.Until.Add(-ConversationFrontierLookback)) {
			id := fmt.Sprintf("update:%s:%d", route.Platform, msg.UpdateID)
			add(id, msg.Topic, msg.At, "user: "+msg.Text)
		}
	}
	slices.SortFunc(turns, func(a, b learning.ConversationTurn) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.OriginRef, b.OriginRef))
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return turns, nil
}

// The journal, topic history, and observed messages are snapshotted under one
// lock. A pending intent represents committed history before it is replayed.
func (s *Store) learningSnapshot(route platform.Message, topic *int64) (mem ChatMemory, conv Conversation, recent []RecentMessage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := route.WorkspaceKey()
	if route.ChatID < 0 {
		all := s.recent
		if all == nil {
			// A standalone file reader has no in-process recent-message cache.
			if err := read(s.recentPath(), &all); err != nil {
				return mem, conv, nil, err
			}
		}
		recent = slices.Clone(all[key])
	}
	if err := read(s.memoryPath(key), &mem); err != nil {
		return mem, conv, nil, err
	}
	if mem.Version == 0 {
		return ChatMemory{}, conv, recent, nil
	}
	if mem.Version != MemoryVersion {
		return mem, conv, nil, fmt.Errorf("unsupported memory version %d", mem.Version)
	}
	if topic != nil {
		route.TopicID = *topic
		key := route.ConversationKey()
		if mem.Intent != nil && mem.Intent.Key == key {
			return mem, mem.Intent.Conversation, recent, nil
		}
		if err := read(s.path(key), &conv); err != nil {
			return mem, conv, nil, err
		}
	}
	return mem, conv, recent, nil
}

// NewFileLearningReader reads already persisted conversation data without
// opening the state store for recovery or writes. Individual files are
// replaced atomically by the chat service.
func NewFileLearningReader(dir string, policy *redaction.Policy,
	authorize func(context.Context, learning.Scope) bool, excluded func(learning.Scope, learning.ConversationTurn) bool) learning.ConversationReader {
	return (&Store{dir: dir, policy: policy}).NewLearningReader(authorize, excluded)
}
