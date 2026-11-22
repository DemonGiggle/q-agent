package state

import (
	"context"
	"crypto/rand"
	"math"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

func (r *LearningRepository) EditLearningFrontier(ctx context.Context, partition learning.Partition, entries []learning.FrontierEntry, id, action string, now time.Time) (learning.FrontierEntry, error) {
	var empty learning.FrontierEntry
	key, err := learningPartitionKey(partition)
	if err != nil || !slices.ContainsFunc(entries, func(entry learning.FrontierEntry) bool { return entry.Question.ID == id }) {
		return empty, learning.ErrDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load(key)
	if err != nil {
		return empty, err
	}
	index := slices.IndexFunc(data.Snapshot.Frontier, func(entry learning.FrontierEntry) bool { return entry.Question.ID == id })
	if index < 0 {
		return empty, learning.ErrDenied
	}
	entry := &data.Snapshot.Frontier[index]
	q := &entry.Question
	if (action == "dismiss" && q.Status == "dismissed") || (q.Status == "candidate" && entry.OperatorSelection == action) {
		return *entry, nil
	}
	if q.Status != "candidate" {
		return empty, learning.ErrDenied
	}
	switch action {
	case "pin", "mute":
		entry.OperatorSelection = action
	case "dismiss":
		q.Status = "dismissed"
	default:
		return empty, learning.ErrDenied
	}
	q.UpdatedAt = maxTime(q.UpdatedAt, now.UTC())
	entry.Transitions = append(entry.Transitions, learning.FrontierTransition{At: q.UpdatedAt, Status: q.Status, Reason: "operator_" + action})
	if err := r.saveFrontier(ctx, key, data, data.Snapshot.Frontier); err != nil {
		return empty, err
	}
	return *entry, nil
}

func (r *LearningRepository) SetLearningNoteArchived(ctx context.Context, cfg learning.Config, id string, archive bool, allow func(string) bool, now time.Time) (learning.Note, error) {
	var empty learning.Note
	id = strings.TrimPrefix(id, "note:")
	if !cfg.Enabled || allow == nil || !auditID.MatchString(id) {
		return empty, learning.ErrDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load("public")
	if err != nil {
		return empty, err
	}
	view := publicRecallView(data.Snapshot, cfg, allow, learning.RecallQuery{}, id, false, false)
	visible, ok := view.notes[id]
	if !ok {
		return empty, learning.ErrDenied
	}
	if (visible.ArchivedAt != nil) == archive {
		return visible, nil
	}
	next := data.Snapshot
	index := slices.IndexFunc(next.Notes, func(note learning.Note) bool { return note.ID == id })
	note := next.Notes[index]
	if note.Revision == math.MaxUint64 {
		return empty, learning.ErrVersionConflict
	}
	now = maxTime(note.UpdatedAt, now.UTC())
	reason := "Operator restored note"
	note.ArchivedAt = nil
	if archive {
		note.ArchivedAt = &now
		reason = "Operator archived note"
	}
	note.UpdatedAt = now
	note.Revision++
	next.Notes = slices.Clone(next.Notes)
	next.Notes[index] = note
	next.Revisions = append(next.Revisions, learning.RevisionEvent{
		Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
		ID:     rand.Text(), EntityType: "note", EntityID: id, OldRevision: note.Revision - 1,
		NewRevision: note.Revision, Reason: reason, OccurredAt: now, EvidenceIDs: note.EvidenceIDs,
	})
	next.Version++
	if err := r.savePublicKnowledge(ctx, data, next); err != nil {
		return empty, err
	}
	visible.ArchivedAt, visible.Revision, visible.UpdatedAt = note.ArchivedAt, note.Revision, note.UpdatedAt
	return visible, nil
}

// ExportPublicLearning returns authorized knowledge for the local operator.
// Internal commit receipts and provider checkpoints stay in the state store.
func (r *LearningRepository) ExportPublicLearning(ctx context.Context, cfg learning.Config, allow func(string) bool) (learning.KnowledgeSnapshot, error) {
	var empty learning.KnowledgeSnapshot
	if !cfg.Enabled || allow == nil {
		return empty, learning.ErrDenied
	}
	snapshot, err := r.Snapshot(ctx, learning.Partition{Public: true})
	if err != nil {
		return empty, err
	}
	view := publicRecallView(snapshot, cfg, allow, learning.RecallQuery{}, "", false, true)
	for _, source := range snapshot.Sources {
		if _, ok := view.sources[source.ID]; ok {
			view.current.Sources = append(view.current.Sources, source)
		}
	}
	for _, event := range snapshot.Revisions {
		if (event.EntityType == "claim" && view.claims[event.EntityID].ID != "") ||
			(event.EntityType == "note" && view.notes[event.EntityID].ID != "") {
			if claimRevisionAllowed(event, cfg, allow, func(id string) bool { return view.evidence[id].ID != "" }) {
				view.current.Revisions = append(view.current.Revisions, event)
			}
		}
	}
	return view.current, nil
}
