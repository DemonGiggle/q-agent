package state

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

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
	Claims   []learning.Claim    `json:"claims,omitempty"`
}

type learningRecallView struct {
	current  learning.KnowledgeSnapshot
	notes    map[string]learning.Note
	sources  map[string]learning.Source
	evidence map[string]learning.Evidence
	claims   map[string]learning.Claim
}

// RecallPublic rechecks the current egress/retention policy before ranking
// notes from the public knowledge partition. The caller owns local auth.
func (r *LearningRepository) RecallPublic(ctx context.Context, cfg learning.Config, query string, allow func(string) bool, limit int) ([]LearningRecall, error) {
	if !cfg.Enabled || allow == nil || !allow(query) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: query}) {
		return nil, learning.ErrDenied
	}
	intent, err := learning.ParseRecallQuery(query)
	if err != nil || limit < 1 || limit > 20 {
		return nil, learning.ErrRecallQuery
	}
	if intent.Text == "" && !intent.Recent {
		return []LearningRecall{}, nil
	}
	view, err := r.publicRecallView(ctx, cfg, allow, intent, "")
	if err != nil {
		return nil, err
	}
	var ids []string
	if intent.Recent {
		slices.SortFunc(view.current.Notes, func(a, b learning.Note) int {
			if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
				return order
			}
			return cmp.Compare(a.ID, b.ID)
		})
		for _, note := range view.current.Notes[:min(len(view.current.Notes), limit)] {
			ids = append(ids, note.ID)
		}
	} else {
		ids, err = learning.RankNoteKeywords(ctx, view.current, intent.Text, limit)
		if err != nil {
			return nil, err
		}
	}
	results := make([]LearningRecall, 0, len(ids))
	for _, id := range ids {
		results = append(results, view.note(id))
	}
	return results, nil
}

// RecentPublicLearning returns notes first saved in the requested interval.
// It shares the current policy checks and archive behavior of public recall.
func (r *LearningRepository) RecentPublicLearning(ctx context.Context, cfg learning.Config, from, until time.Time,
	allow func(string) bool, limit int) ([]LearningRecall, error) {
	if !cfg.Enabled || allow == nil {
		return nil, learning.ErrDenied
	}
	if from.IsZero() || !from.Before(until) || limit < 1 || limit > 10 {
		return nil, learning.ErrRecallQuery
	}
	view, err := r.publicRecallView(ctx, cfg, allow, learning.RecallQuery{Recent: true}, "")
	if err != nil {
		return nil, err
	}
	notes := slices.DeleteFunc(view.current.Notes, func(note learning.Note) bool {
		return note.CreatedAt.Before(from) || !note.CreatedAt.Before(until)
	})
	slices.SortFunc(notes, func(a, b learning.Note) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	result := make([]LearningRecall, 0, min(len(notes), limit))
	for _, note := range notes[:min(len(notes), limit)] {
		result = append(result, view.note(note.ID))
	}
	return result, nil
}

// LatestPublicLearning returns the most recently updated authorized public notes.
func (r *LearningRepository) LatestPublicLearning(ctx context.Context, cfg learning.Config,
	allow func(string) bool, limit int) ([]LearningRecall, error) {
	if !cfg.Enabled || allow == nil {
		return nil, learning.ErrDenied
	}
	if limit < 1 || limit > 10 {
		return nil, learning.ErrRecallQuery
	}
	view, err := r.publicRecallView(ctx, cfg, allow, learning.RecallQuery{Recent: true}, "")
	if err != nil {
		return nil, err
	}
	notes := slices.Clone(view.current.Notes)
	slices.SortFunc(notes, func(a, b learning.Note) int {
		return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	result := make([]LearningRecall, 0, min(len(notes), limit))
	for _, note := range notes[:min(len(notes), limit)] {
		result = append(result, view.note(note.ID))
	}
	return result, nil
}

// ReadPublicNote resolves a note ID or body reference with the same policy
// and citation checks as recall.
func (r *LearningRepository) ReadPublicNote(ctx context.Context, cfg learning.Config, ref string, allow func(string) bool) (LearningRecall, error) {
	id := strings.TrimPrefix(ref, "note:")
	if !cfg.Enabled || allow == nil || !auditID.MatchString(id) {
		return LearningRecall{}, learning.ErrDenied
	}
	view, err := r.publicRecallView(ctx, cfg, allow, learning.RecallQuery{}, id)
	if err != nil {
		return LearningRecall{}, err
	}
	if _, ok := view.notes[id]; !ok {
		return LearningRecall{}, learning.ErrDenied
	}
	return view.note(id), nil
}

func (r *LearningRepository) publicRecallView(ctx context.Context, cfg learning.Config, allow func(string) bool, intent learning.RecallQuery,
	onlyID string) (learningRecallView, error) {
	snapshot, err := r.Snapshot(ctx, learning.Partition{Public: true})
	if err != nil {
		return learningRecallView{}, err
	}
	return publicRecallView(snapshot, cfg, allow, intent, onlyID, false, false), nil
}

func publicRecallView(snapshot learning.KnowledgeSnapshot, cfg learning.Config, allow func(string) bool, intent learning.RecallQuery, onlyID string, frontier, exporting bool) learningRecallView {
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
	claims := make(map[string]learning.Claim, len(snapshot.Claims))
	for _, claim := range snapshot.Claims {
		if claim.Validate() != nil || len(claim.Evidence) == 0 || !claimTextsAllowed(cfg, allow, claim) {
			continue
		}
		if slices.ContainsFunc(claim.Evidence, func(link learning.EvidenceLink) bool {
			_, ok := evidence[link.EvidenceID]
			return !ok
		}) {
			continue
		}
		claims[claim.ID] = claim
		current.Claims = append(current.Claims, claim)
	}
	noteSet := snapshot.Notes
	if frontier {
		noteSet = nil
	}
	notes := make(map[string]learning.Note, len(noteSet))
	hasNoteTopics := false
	for _, note := range noteSet {
		if (onlyID != "" && note.ID != onlyID) || (onlyID == "" && !exporting && note.ArchivedAt != nil) || len(note.SourceIDs) == 0 || !allowed(note.Title) || !allowed(note.Summary) {
			continue
		}
		valid, matches := true, false
		for _, id := range note.SourceIDs {
			source, ok := sources[id]
			valid = valid && ok
			matches = matches || (ok && intent.MatchesSource(source))
		}
		for _, id := range note.EvidenceIDs {
			_, ok := evidence[id]
			valid = valid && ok
		}
		if !valid || !matches {
			continue
		}
		noteClaims := note.ClaimIDs
		note.ClaimIDs = nil
		for _, id := range noteClaims {
			if claim, ok := claims[id]; ok {
				note.ClaimIDs = append(note.ClaimIDs, id)
				note.EvidenceIDs = slices.Clone(note.EvidenceIDs)
				for _, link := range claim.Evidence {
					if !slices.Contains(note.EvidenceIDs, link.EvidenceID) {
						note.EvidenceIDs = append(note.EvidenceIDs, link.EvidenceID)
					}
				}
			}
		}
		if note.BodyRef == "" && auditID.MatchString(note.ID) {
			note.BodyRef = "note:" + note.ID
		}
		hasNoteTopics = hasNoteTopics || len(note.TopicIDs) > 0
		current.Notes = append(current.Notes, note)
		notes[note.ID] = note
	}
	if frontier || exporting || (hasNoteTopics && onlyID == "" && !intent.Recent) {
		topics := make(map[string]bool, len(snapshot.Topics))
		safe := func(value string) bool { return !allowed(value) }
		for _, topic := range snapshot.Topics {
			if allowed(topic.Title) && !slices.ContainsFunc(topic.Aliases, safe) && !slices.ContainsFunc(topic.TaxonomyPath, safe) {
				topics[topic.ID] = true
				current.Topics = append(current.Topics, topic)
			}
		}
		for _, edge := range snapshot.Relations {
			if !topics[edge.FromID] || !topics[edge.ToID] || len(edge.EvidenceIDs) == 0 ||
				!allowed(edge.Predicate) || slices.ContainsFunc(edge.EvidenceIDs, func(id string) bool {
				item, ok := evidence[id]
				return !ok || !sources[item.SourceID].Available()
			}) {
				continue
			}
			if edge.ClaimID != "" {
				claim := claims[edge.ClaimID]
				if claim.Status != "supported" || claim.Revision != edge.ClaimRevision {
					continue
				}
			}
			valid := true
			for name, value := range edge.Qualifiers {
				valid = valid && allowed(name+" "+value)
			}
			if valid {
				current.Relations = append(current.Relations, edge)
			}
		}
	}
	return learningRecallView{current: current, notes: notes, sources: sources, evidence: evidence, claims: claims}
}

func (v learningRecallView) note(id string) LearningRecall {
	note := v.notes[id]
	result := LearningRecall{Note: note}
	for _, id := range note.ClaimIDs {
		result.Claims = append(result.Claims, v.claims[id])
	}
	for _, id := range note.SourceIDs {
		result.Sources = append(result.Sources, v.sources[id])
	}
	for _, id := range note.EvidenceIDs {
		item := v.evidence[id]
		result.Evidence = append(result.Evidence, item)
		if !slices.ContainsFunc(result.Sources, func(source learning.Source) bool { return source.ID == item.SourceID }) {
			result.Sources = append(result.Sources, v.sources[item.SourceID])
		}
	}
	return result
}
