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
	mem, conv, err := r.store.learningSnapshot(route, scope.TopicID)
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
	slices.SortFunc(turns, func(a, b learning.ConversationTurn) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.OriginRef, b.OriginRef))
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return turns, nil
}

// The journal and optional topic history are snapshotted under one lock.
// Reading a pending intent gives the committed history without replaying writes.
func (s *Store) learningSnapshot(route platform.Message, topic *int64) (ChatMemory, Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mem ChatMemory
	var conv Conversation
	if err := read(s.memoryPath(route.WorkspaceKey()), &mem); err != nil {
		return mem, conv, err
	}
	if mem.Version == 0 {
		return ChatMemory{}, conv, nil
	}
	if mem.Version != MemoryVersion {
		return mem, conv, fmt.Errorf("unsupported memory version %d", mem.Version)
	}
	if topic != nil {
		route.TopicID = *topic
		key := route.ConversationKey()
		if mem.Intent != nil && mem.Intent.Key == key {
			return mem, mem.Intent.Conversation, nil
		}
		if err := read(s.path(key), &conv); err != nil {
			return mem, conv, err
		}
	}
	return mem, conv, nil
}
