package learning

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestRankNoteKeywords(t *testing.T) {
	now := time.Now()
	s := KnowledgeSnapshot{
		Evidence: []Evidence{{ID: "quote", ExcerptOrSummary: "藤原氏的政治制度"}},
		Notes: []Note{
			{ID: "excerpt", Title: "Heian period", EvidenceIDs: []string{"quote"}},
			{ID: "focused", Title: "藤原氏", Record: Record{UpdatedAt: now}},
			{ID: "unrelated", Title: "Seattle restaurants"},
		},
	}
	ids, err := RankNoteKeywords(context.Background(), s, "藤原氏", 2)
	if err != nil || !slices.Equal(ids, []string{"focused", "excerpt"}) {
		t.Fatalf("ranked IDs = %v, %v", ids, err)
	}
	ids, err = RankNoteKeywords(context.Background(), s, "tea", 2)
	if err != nil || len(ids) != 0 {
		t.Fatalf("unrelated IDs = %v, %v", ids, err)
	}
}
