package state

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

// Markdown renders a reference note and its complete citations from the
// authorized snapshot. An empty result means it did not fit the limit.
func (hit LearningRecall) Markdown(policy *learning.ResearchSourcePolicy, now time.Time, limit int) string {
	if len(hit.Note.Title)+len(hit.Note.Summary) > limit {
		return ""
	}
	var text strings.Builder
	fmt.Fprintf(&text, "## Research question: %s\n\nSummary: %s\n\n", hit.Note.Title, hit.Note.Summary)
	for i, source := range hit.Sources {
		fmt.Fprintf(&text, "- Source %d: %s (read %s", i+1, source.CanonicalURI, source.FetchedAt.UTC().Format(time.DateOnly))
		if source.PublishedAt != nil {
			fmt.Fprintf(&text, ", published %s", source.PublishedAt.UTC().Format(time.DateOnly))
		}
		if !policy.Accepts(source, now) {
			text.WriteString(", outside current date policy")
		}
		text.WriteString(")\n")
		if text.Len() > limit {
			return ""
		}
		for j, item := range hit.Evidence {
			if item.SourceID == source.ID {
				fmt.Fprintf(&text, "  - Excerpt %d (bytes %d–%d): %s\n", j+1, item.Locator.Start, item.Locator.End, item.ExcerptOrSummary)
				if text.Len() > limit {
					return ""
				}
			}
		}
	}
	for _, claim := range hit.Claims {
		if len(claim.Subject)+len(claim.Predicate)+len(claim.Object)+text.Len() > limit {
			return ""
		}
		fmt.Fprintf(&text, "\n- Claim [%s, confidence %.2f]: %s %s %s\n", claim.Status, claim.Confidence, claim.Subject, claim.Predicate, claim.Object)
		for _, link := range claim.Evidence {
			index := slices.IndexFunc(hit.Evidence, func(item learning.Evidence) bool { return item.ID == link.EvidenceID })
			fmt.Fprintf(&text, "  - Excerpt %d: %s\n", index+1, link.Relation)
			if text.Len() > limit {
				return ""
			}
		}
	}
	text.WriteByte('\n')
	if text.Len() > limit {
		return ""
	}
	return text.String()
}
