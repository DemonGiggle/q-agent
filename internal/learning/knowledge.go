package learning

import (
	"fmt"
	"reflect"
	"strings"
)

// ValidateReferences checks citations within the replacement partition before
// it becomes visible. Cross-partition references require an explicit future
// authorization contract.
func (s KnowledgeSnapshot) ValidateReferences() error {
	topics, err := knowledgeIDs(s.Topics, "topic", func(v Topic) string { return v.ID })
	if err != nil {
		return err
	}
	questions, err := knowledgeIDs(s.Frontier, "question", func(v FrontierEntry) string { return v.Question.ID })
	if err != nil {
		return err
	}
	sources, err := knowledgeIDs(s.Sources, "source", func(v Source) string { return v.ID })
	if err != nil {
		return err
	}
	evidence, err := knowledgeIDs(s.Evidence, "evidence", func(v Evidence) string { return v.ID })
	if err != nil {
		return err
	}
	claims, err := knowledgeIDs(s.Claims, "claim", func(v Claim) string { return v.ID })
	if err != nil {
		return err
	}
	notes, err := knowledgeIDs(s.Notes, "note", func(v Note) string { return v.ID })
	if err != nil {
		return err
	}
	if _, err := knowledgeIDs(s.Revisions, "revision", func(v RevisionEvent) string { return v.ID }); err != nil {
		return err
	}
	if _, err := knowledgeIDs(s.Relations, "relation", func(v Relation) string { return v.ID }); err != nil {
		return err
	}

	for _, entry := range s.Frontier {
		q := entry.Question
		if err := q.Validate(); err != nil {
			return err
		}
		if !hasKnowledgeIDs(topics, q.TopicIDs) || !hasKnowledgeIDs(questions, entry.DependsOn) ||
			(q.ParentID != "" && !questions[q.ParentID]) {
			return fmt.Errorf("invalid frontier references")
		}
	}
	for _, v := range s.Sources {
		if err := v.Validate(); err != nil {
			return err
		}
	}
	for _, v := range s.Evidence {
		loc := v.Locator
		if !v.Record.valid() || v.ExtractedAt.IsZero() ||
			!sources[v.SourceID] || strings.TrimSpace(v.ExcerptOrSummary) == "" ||
			loc.Start < 0 || loc.End < loc.Start ||
			(strings.TrimSpace(loc.Selector) == "" && loc.End == loc.Start) {
			return fmt.Errorf("invalid stored evidence or source locator")
		}
	}
	replacements := make(map[string]string)
	for _, v := range s.Claims {
		if err := v.Validate(); err != nil {
			return err
		}
		for _, link := range v.Evidence {
			if !evidence[link.EvidenceID] {
				return fmt.Errorf("unknown evidence cited by claim")
			}
		}
		if v.SupersededBy != "" && (v.SupersededBy == v.ID || !claims[v.SupersededBy] || v.Status != "superseded") {
			return fmt.Errorf("invalid replacement claim")
		}
		if v.SupersededBy != "" {
			replacements[v.ID] = v.SupersededBy
		}
	}
	seen := make(map[string]string, len(replacements))
	for claim := range replacements {
		for id := claim; replacements[id] != ""; id = replacements[id] {
			if seen[id] == claim {
				return fmt.Errorf("cyclic replacement claim")
			}
			if seen[id] != "" {
				break
			}
			seen[id] = claim
		}
	}
	for _, v := range s.Notes {
		if !v.Record.valid() || v.Revision == 0 || !hasKnowledgeIDs(topics, v.TopicIDs) ||
			!hasKnowledgeIDs(claims, v.ClaimIDs) || !hasKnowledgeIDs(sources, v.SourceIDs) ||
			!hasKnowledgeIDs(evidence, v.EvidenceIDs) {
			return fmt.Errorf("invalid note or note references")
		}
	}
	for _, v := range s.Relations {
		if !v.Record.valid() || v.FromID == "" || v.ToID == "" || v.Predicate == "" ||
			!hasKnowledgeIDs(evidence, v.EvidenceIDs) ||
			(v.ClaimID != "" && (!claims[v.ClaimID] || v.ClaimRevision == 0 || !topics[v.FromID] || !topics[v.ToID])) {
			return fmt.Errorf("invalid relation or relation references")
		}
	}
	for _, v := range s.Revisions {
		var entities map[string]bool
		switch v.EntityType {
		case "claim":
			entities = claims
		case "note":
			entities = notes
		}
		if !v.Record.valid() || !entities[v.EntityID] || v.NewRevision <= v.OldRevision ||
			v.OccurredAt.IsZero() || strings.TrimSpace(v.Reason) == "" || !hasKnowledgeIDs(evidence, v.EvidenceIDs) {
			return fmt.Errorf("invalid revision or revision references")
		}
		if prior := v.Before; prior != nil {
			if v.EntityType != "claim" || prior.ID != v.EntityID || prior.Revision != v.OldRevision || prior.Validate() != nil {
				return fmt.Errorf("invalid prior claim revision")
			}
			for _, link := range prior.Evidence {
				if !evidence[link.EvidenceID] {
					return fmt.Errorf("unknown evidence cited by a prior claim")
				}
			}
		}
	}
	return nil
}

// ValidateClaimHistory protects published claims and their prior revisions in
// whole-snapshot writes. Older stores can begin tracking at their current revision.
func (s KnowledgeSnapshot) ValidateClaimHistory(prior KnowledgeSnapshot) error {
	if len(s.Revisions) < len(prior.Revisions) {
		return fmt.Errorf("learning revision history is append-only")
	}
	for i, event := range prior.Revisions {
		if !reflect.DeepEqual(s.Revisions[i], event) {
			return fmt.Errorf("learning revision history is append-only")
		}
	}
	if len(prior.Claims) == 0 {
		return nil
	}
	current := make(map[string]Claim, len(s.Claims))
	for _, claim := range s.Claims {
		current[claim.ID] = claim
	}
	events := make(map[string]RevisionEvent)
	for _, event := range s.Revisions[len(prior.Revisions):] {
		if event.EntityType == "claim" {
			events[event.EntityID] = event
		}
	}
	for _, last := range prior.Claims {
		claim, found := current[last.ID]
		if !found {
			return fmt.Errorf("learning claims cannot be removed")
		}
		if reflect.DeepEqual(claim, last) {
			continue
		}
		event := events[claim.ID]
		if claim.Revision != last.Revision+1 || event.NewRevision != claim.Revision ||
			event.OldRevision != last.Revision || event.Before == nil || !reflect.DeepEqual(*event.Before, last) {
			return fmt.Errorf("learning claim update requires its prior revision")
		}
	}
	return nil
}

func knowledgeIDs[T any](items []T, kind string, id func(T) string) (map[string]bool, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		key := id(item)
		if strings.TrimSpace(key) == "" || ids[key] {
			return nil, fmt.Errorf("empty or duplicate %s ID", kind)
		}
		ids[key] = true
	}
	return ids, nil
}

func hasKnowledgeIDs(ids map[string]bool, refs []string) bool {
	for _, ref := range refs {
		if !ids[ref] {
			return false
		}
	}
	return true
}
