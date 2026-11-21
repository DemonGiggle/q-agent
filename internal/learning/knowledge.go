package learning

import (
	"fmt"
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
	for _, v := range s.Claims {
		if err := v.Validate(); err != nil {
			return err
		}
		for _, link := range v.Evidence {
			if !evidence[link.EvidenceID] {
				return fmt.Errorf("unknown evidence cited by claim")
			}
		}
	}
	for _, v := range s.Notes {
		if !v.Record.valid() || v.Revision == 0 || !hasKnowledgeIDs(topics, v.TopicIDs) ||
			!hasKnowledgeIDs(claims, v.ClaimIDs) || !hasKnowledgeIDs(sources, v.SourceIDs) {
			return fmt.Errorf("invalid note or note references")
		}
	}
	for _, v := range s.Relations {
		if !v.Record.valid() || v.FromID == "" || v.ToID == "" || v.Predicate == "" ||
			!hasKnowledgeIDs(evidence, v.EvidenceIDs) {
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
