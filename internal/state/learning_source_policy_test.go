package state

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningSourcePolicyWorker(t *testing.T) {
	const first = "https://records.example.org/archive"
	const second = "https://reference.net/notes"
	const third = "https://library.example.com/report"
	now := time.Now().UTC()
	ageOverride := 60
	for _, tc := range []struct {
		name             string
		domains          map[string]learning.DomainSourceRule
		wantFirst        string
		wantFetches      int
		wantRefs         int
		unmatchedExcerpt bool
	}{
		{"stale source skipped", nil, second, 3, 2, false},
		{"domain date override", map[string]learning.DomainSourceRule{"records.example.org": {MaxAgeDays: &ageOverride}}, first, 2, 2, false},
		{"single authority", map[string]learning.DomainSourceRule{"records.example.org": {MaxAgeDays: &ageOverride, SingleAuthority: true}}, first, 1, 1, false},
		{"authority needs exact host", map[string]learning.DomainSourceRule{
			"example.org":         {SingleAuthority: true},
			"records.example.org": {MaxAgeDays: &ageOverride},
		}, first, 2, 2, false},
		{"authority needs excerpt", map[string]learning.DomainSourceRule{"records.example.org": {MaxAgeDays: &ageOverride, SingleAuthority: true}}, second, 3, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := NewLearningCoordinator(t.TempDir())
			c.now = func() time.Time { return now }
			cfg := learning.DefaultConfig()
			cfg.Enabled = true
			cfg.Permissions.AllowDomains = []string{"example.org", "reference.net", "library.example.com"}
			cfg.SourcePolicy = &learning.ResearchSourcePolicy{MaxAgeDays: 30, Domains: tc.domains}
			id, err := c.EnqueueResearch(ctx, cfg, "Public question", 2)
			if err != nil {
				t.Fatal(err)
			}
			search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
				return []learning.SearchHit{{URI: first}, {URI: second}, {URI: third}}, learning.Budget{Requests: 1}, nil
			})
			fetches := 0
			fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
				fetches++
				published := now.AddDate(0, 0, -2)
				body := fmt.Sprintf("The public question is discussed in document %d.", fetches)
				if uri == first {
					published = now.AddDate(0, 0, -40)
					if tc.unmatchedExcerpt {
						body = "An unrelated article about birds and forests."
					}
				}
				return learning.FetchedDocument{MediaType: "text/plain", Body: []byte(body), Source: learning.Source{
					Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
					ID:     fmt.Sprint(fetches), CanonicalURI: uri, FetchedAt: now, PublishedAt: &published,
					ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(body))), TrustClass: "public_reference",
				}}, learning.Budget{Requests: 2, Documents: 1}, nil
			})
			res, err := c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch, localtools.LearningDocumentReader{}, nil)
			wantReason := "independent_excerpts_recorded"
			if tc.wantRefs == 1 {
				wantReason = "authority_source_recorded"
			}
			if err != nil || res.State != learning.RunCompleted || res.StopReason != wantReason || fetches != tc.wantFetches ||
				len(res.EvidenceIDs) != tc.wantRefs || res.CoverageScore != float64(tc.wantRefs)/2 {
				t.Fatalf("research = %+v, fetches = %d, error = %v", res, fetches, err)
			}
			events, err := c.ExportResearchAudit(ctx, cfg, id, func(string) bool { return true })
			if err != nil || len(events) == 0 || events[len(events)-1].Reason != wantReason {
				t.Fatalf("audit = %+v, error = %v", events, err)
			}
			snap, err := c.Repository().Snapshot(ctx, learning.Partition{Public: true})
			if err != nil || len(snap.Notes) != 1 || len(snap.Sources) != tc.wantRefs || len(snap.Claims) != 0 ||
				snap.Sources[0].CanonicalURI != tc.wantFirst || snap.Sources[0].PublishedAt == nil {
				t.Fatalf("knowledge = %+v, error = %v", snap, err)
			}
		})
	}
}

func TestLearningSourcePolicyRechecksCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	c := NewLearningCoordinator(dir)
	c.now = func() time.Time { return now }
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Permissions.AllowDomains = []string{"example.org"}
	cfg.SourcePolicy = &learning.ResearchSourcePolicy{MaxAgeDays: 10}
	id, err := c.EnqueueResearch(ctx, cfg, "Public question", 1)
	if err != nil {
		t.Fatal(err)
	}
	record, err := c.load(id)
	if err != nil {
		t.Fatal(err)
	}
	published := now.AddDate(0, 0, -9)
	source := learning.Source{
		Record: record.Run.Record, ID: "source", CanonicalURI: "https://example.org/article", ContentHash: fmt.Sprintf("%064x", 1),
		PublishedAt: &published, FetchedAt: now, TrustClass: "public_reference",
	}
	record.Run.State = learning.RunValidating
	record.Run.Checkpoint = &learning.Checkpoint{NextIntent: "validate", ResearchSource: &source}
	if err := c.save(record); err != nil {
		t.Fatal(err)
	}

	search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
		t.Fatal("restarted run reissued search")
		return nil, learning.Budget{}, nil
	})
	fetch := researchFetcher(func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error) {
		t.Fatal("restarted run reissued fetch")
		return learning.FetchedDocument{}, learning.Budget{}, nil
	})
	c = NewLearningCoordinator(dir)
	c.now = func() time.Time { return now.AddDate(0, 0, 2) }
	res, err := c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch, localtools.LearningDocumentReader{}, nil)
	if err != nil || res.State != learning.RunUnresolved || res.StopReason != "policy_changed" || res.CoverageScore != 0 {
		t.Fatalf("resumed research = %+v, error = %v", res, err)
	}
	snap, err := c.Repository().Snapshot(ctx, learning.Partition{Public: true})
	if err != nil || snap.Version != 0 {
		t.Fatalf("stale source published knowledge = %+v, error = %v", snap, err)
	}
}
