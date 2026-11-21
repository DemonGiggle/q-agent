package state

import (
	"context"
	"slices"

	"q-agent/internal/learning"
)

// LearningKeywordIndex is a read-only index over one authorized repository
// snapshot. Rebuild from a fresh snapshot when its version changes.
type LearningKeywordIndex struct {
	key      string
	snapshot learning.KnowledgeSnapshot
}

var _ learning.IndexProvider = (*LearningKeywordIndex)(nil)

func (i *LearningKeywordIndex) Rebuild(ctx context.Context, partition learning.Partition, snapshot learning.KnowledgeSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := learningPartitionKey(partition)
	if err != nil {
		return err
	}
	i.key, i.snapshot = key, snapshot
	return nil
}

func (i *LearningKeywordIndex) Lookup(ctx context.Context, partition learning.Partition, query string, limit int) ([]string, error) {
	key, err := learningPartitionKey(partition)
	if err != nil || key != i.key {
		return nil, learning.ErrDenied
	}
	return learning.RankNoteKeywords(ctx, i.snapshot, query, limit)
}

// LearningRecall returns local reference notes and their citations.
type LearningRecall struct {
	Note     learning.Note       `json:"note"`
	Sources  []learning.Source   `json:"sources"`
	Evidence []learning.Evidence `json:"evidence,omitempty"`
}

// RecallPublic rechecks the current egress/retention policy before ranking
// notes from the public knowledge partition. The caller owns local auth.
func (r *LearningRepository) RecallPublic(ctx context.Context, cfg learning.Config, query string, allow func(string) bool, limit int) ([]LearningRecall, error) {
	if !cfg.Enabled || allow == nil || !allow(query) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: query}) {
		return nil, learning.ErrDenied
	}
	partition := learning.Partition{Public: true}
	snapshot, err := r.Snapshot(ctx, partition)
	if err != nil {
		return nil, err
	}
	allowed := func(s string) bool {
		return allow(s) && !cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: s})
	}
	sources := make(map[string]learning.Source, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		if cfg.Permissions.ResearchURI(source.CanonicalURI) && allowed(source.CanonicalURI) &&
			allowed(source.Title) && allowed(source.Publisher) {
			sources[source.ID] = source
		}
	}
	evidence := make(map[string]learning.Evidence, len(snapshot.Evidence))
	current := learning.KnowledgeSnapshot{Version: snapshot.Version}
	for _, e := range snapshot.Evidence {
		if source, exists := sources[e.SourceID]; exists && e.MatchesSourceExcerpt(source) && allowed(e.ExcerptOrSummary) {
			evidence[e.ID] = e
			current.Evidence = append(current.Evidence, e)
		}
	}
	notes := make(map[string]learning.Note, len(snapshot.Notes))
	for _, note := range snapshot.Notes {
		if len(note.SourceIDs) == 0 || !allowed(note.Title) || !allowed(note.Summary) {
			continue
		}
		valid := true
		for _, id := range note.SourceIDs {
			_, ok := sources[id]
			valid = valid && ok
		}
		for _, id := range note.EvidenceIDs {
			_, ok := evidence[id]
			valid = valid && ok
		}
		if valid {
			current.Notes = append(current.Notes, note)
			notes[note.ID] = note
		}
	}
	var index LearningKeywordIndex
	if err := index.Rebuild(ctx, partition, current); err != nil {
		return nil, err
	}
	ids, err := index.Lookup(ctx, partition, query, limit)
	if err != nil {
		return nil, err
	}
	results := make([]LearningRecall, 0, len(ids))
	for _, id := range ids {
		note := notes[id]
		result := LearningRecall{Note: note}
		for _, id := range note.SourceIDs {
			result.Sources = append(result.Sources, sources[id])
		}
		for _, id := range note.EvidenceIDs {
			item := evidence[id]
			result.Evidence = append(result.Evidence, item)
			if !slices.ContainsFunc(result.Sources, func(source learning.Source) bool { return source.ID == item.SourceID }) {
				result.Sources = append(result.Sources, sources[item.SourceID])
			}
		}
		results = append(results, result)
	}
	return results, nil
}
