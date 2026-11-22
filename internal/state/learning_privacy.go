package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

const ConversationFrontierLookback = 48 * time.Hour

type LearningDeletionReceipt struct {
	ID                  string    `json:"receipt_id"`
	At                  time.Time `json:"deleted_at"`
	Removed             int       `json:"removed"`
	UnreadablePartition bool      `json:"unreadable_partition,omitempty"`
}

type privateLearningDeletion struct {
	LearningDeletionReceipt
	Candidate string   `json:"candidate_id,omitempty"`
	Origins   []string `json:"origin_refs,omitempty"`
}

func (r *LearningRepository) DeletePrivateLearning(ctx context.Context, scope learning.Scope, candidate, requestID string, now time.Time) (LearningDeletionReceipt, error) {
	var empty LearningDeletionReceipt
	key, err := learningPartitionKey(learning.Partition{Scope: &scope})
	if err != nil || scope.TopicID == nil || !auditID.MatchString(requestID) ||
		(candidate != "" && !validConversationCandidateID(candidate)) {
		return empty, learning.ErrDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	data, err := r.load(key)
	unreadable := errors.Is(err, errLearningPartitionCorrupt) || errors.Is(err, ErrLearningPartitionFull)
	if err != nil {
		if candidate != "" || !unreadable {
			return empty, err
		}
		// Whole-topic erasure replaces unreadable bytes as well as known
		// records. A single-candidate request cannot discard its neighbors.
		data = learningPartition{Schema: learning.SchemaVersion}
	}
	for _, prior := range data.Deletions {
		if prior.ID == requestID {
			if prior.Candidate != candidate {
				return empty, learning.ErrVersionConflict
			}
			if err := SyncDir(r.dir); err != nil {
				return empty, err
			}
			return prior.LearningDeletionReceipt, nil
		}
	}
	receipt := privateLearningDeletion{
		LearningDeletionReceipt: LearningDeletionReceipt{ID: requestID, At: now.UTC(), UnreadablePartition: unreadable},
		Candidate:               candidate,
	}
	data.Snapshot.Frontier = slices.DeleteFunc(data.Snapshot.Frontier, func(entry learning.FrontierEntry) bool {
		if candidate != "" && entry.Question.ID != candidate {
			return false
		}
		receipt.Removed++
		if candidate != "" {
			receipt.Origins = entry.Question.OriginRefs
		}
		return true
	})
	if candidate != "" && receipt.Removed == 0 {
		return empty, learning.ErrDenied
	}
	if candidate == "" {
		data.Snapshot = learning.KnowledgeSnapshot{Version: data.Snapshot.Version}
		data.Receipts = nil
	} else {
		for i := range data.Snapshot.Frontier {
			entry := &data.Snapshot.Frontier[i]
			entry.DependsOn = slices.DeleteFunc(entry.DependsOn, func(id string) bool { return id == candidate })
			if entry.Question.ParentID == candidate {
				entry.Question.ParentID = ""
			}
		}
	}
	// Any interrupted atomic writes are abandoned under the caller's
	// learning-directory lock. Backups in this directory follow the
	// partition filename so privacy erasure can remove them as well.
	if err := r.removeLearningCopies(key); err != nil {
		return empty, err
	}
	data.Deletions = append(data.Deletions, receipt)
	data.Snapshot.Version++
	if err := r.saveLearningPartition(ctx, key, data); err != nil {
		return empty, err
	}
	return receipt.LearningDeletionReceipt, nil
}

func validConversationCandidateID(id string) bool {
	encoded, ok := strings.CutPrefix(id, "conversation-")
	return ok && len(encoded) == 32 && !strings.ContainsFunc(encoded, func(c rune) bool {
		return (c < '0' || c > '9') && (c < 'a' || c > 'f')
	})
}

func (r *LearningRepository) removeLearningCopies(key string) error {
	list, err := os.ReadDir(r.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, item := range list {
		name := item.Name()
		if item.IsDir() || (!strings.HasPrefix(name, ".atomic-") && name != key+".json.bak" &&
			!strings.HasPrefix(name, key+".json.bak.")) {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return SyncDir(r.dir)
}

func privateLearningAfter(data learningPartition, from time.Time) time.Time {
	for _, erased := range data.Deletions {
		if erased.Candidate == "" && !erased.At.Before(from) {
			from = erased.At.Add(time.Nanosecond)
		}
	}
	return from
}

func filterDeletedLearning(data learningPartition, turns []learning.ConversationTurn, now time.Time) ([]learning.ConversationTurn, map[string]bool) {
	var blockedOrigins, blockedCandidates map[string]bool
	for _, erased := range data.Deletions {
		if erased.Candidate == "" || now.Sub(erased.At) > ConversationFrontierLookback {
			continue
		}
		if blockedCandidates == nil {
			blockedOrigins, blockedCandidates = make(map[string]bool), make(map[string]bool)
		}
		blockedCandidates[erased.Candidate] = true
		for _, origin := range erased.Origins {
			blockedOrigins[origin] = true
		}
	}
	if len(blockedOrigins) != 0 {
		turns = slices.DeleteFunc(turns, func(turn learning.ConversationTurn) bool { return blockedOrigins[turn.OriginRef] })
	}
	return turns, blockedCandidates
}

// ExportPrivateLearning returns persisted leads for an already authorized
// topic. It filters retained leads without importing any chat history.
func (r *LearningRepository) ExportPrivateLearning(ctx context.Context, scope learning.Scope, cutoff time.Time, allow func(string) bool) ([]learning.FrontierEntry, error) {
	if scope.TopicID == nil || allow == nil {
		return nil, learning.ErrDenied
	}
	snapshot, err := r.Snapshot(ctx, learning.Partition{Scope: &scope})
	if err != nil {
		return nil, err
	}
	entries := make([]learning.FrontierEntry, 0, len(snapshot.Frontier))
	for _, entry := range snapshot.Frontier {
		if !entry.Question.UpdatedAt.Before(cutoff) && allow(entry.Question.Text) {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}
