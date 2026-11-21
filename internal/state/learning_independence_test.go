package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningTwoSourceWorker(t *testing.T) {
	for _, tc := range []struct {
		name             string
		requests         int64
		want             learning.RunState
		wantFetch        int
		thirdIndependent bool
	}{
		{"syndication then distinct source", 8, learning.RunCompleted, 3, true},
		{"syndication and duplicate text", 8, learning.RunUnresolved, 3, false},
		{"exhausted after first source", 3, learning.RunBudgetExhausted, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			c := NewLearningCoordinator(dir)
			cfg := learning.DefaultConfig()
			cfg.Enabled, cfg.PerQuestion.Requests = true, tc.requests
			cfg.Permissions.AllowDomains = []string{"source-one.org", "source-two.org", "source-three.org"}
			const first = "https://news.source-one.org/primary"
			const mirror = "https://source-two.org/reprint"
			const third = "https://source-three.org/document"
			const primaryText = "The public question is discussed by the archive."
			id, err := c.EnqueueResearch(ctx, cfg, "Public question", 2)
			if err != nil {
				t.Fatal(err)
			}
			search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
				return []learning.SearchHit{{URI: first}, {URI: "https://other.source-one.org/copy"}, {URI: mirror}, {URI: third}},
					learning.Budget{Requests: 1}, nil
			})
			fetches := 0
			fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
				fetches++
				now := time.Now().UTC()
				text, origin := primaryText, ""
				if uri == mirror {
					text, origin = "The public question is also discussed in this syndicated copy.", "source-one.org"
				} else if uri == third && tc.thirdIndependent {
					text = "Independent museum records address this public question."
				}
				if uri != first && uri != mirror && uri != third {
					t.Fatalf("same-site candidate fetched: %s", uri)
				}
				// Different HTML wrappers may reproduce the same excerpt.
				body := []byte(fmt.Sprintf("<title>Page %d</title><p>%s</p>", fetches, text))
				return learning.FetchedDocument{
					Body: body, MediaType: "text/html",
					Source: learning.Source{Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
						ID: fmt.Sprint(fetches), CanonicalURI: uri, SyndicationOrigin: origin,
						FetchedAt: now, ContentHash: fmt.Sprintf("%x", sha256.Sum256(body)), TrustClass: "public_reference"},
				}, learning.Budget{Requests: 2, Documents: 1}, nil
			})
			result, err := c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch, localtools.LearningDocumentReader{}, nil)
			if result.State != tc.want || fetches != tc.wantFetch ||
				(tc.want == learning.RunBudgetExhausted && !errors.Is(err, ErrLearningBudget)) ||
				(tc.want != learning.RunBudgetExhausted && err != nil) {
				t.Fatalf("result=%+v fetches=%d err=%v", result, fetches, err)
			}
			snap, err := c.Repository().Snapshot(ctx, learning.Partition{Public: true})
			if err != nil || len(snap.Claims) != 0 {
				t.Fatalf("knowledge: %+v, %v", snap, err)
			}
			if tc.want == learning.RunCompleted {
				if result.CoverageScore != 1 || len(result.EvidenceIDs) != 2 ||
					len(snap.Notes) != 1 || len(snap.Sources) != 2 || len(snap.Notes[0].EvidenceIDs) != 2 ||
					snap.Sources[1].CanonicalURI != third {
					t.Fatalf("independent excerpts not committed: %+v, %+v", result, snap)
				}
				return
			}
			if result.CoverageScore != 0.5 || snap.Version != 0 || len(result.EvidenceIDs) != 0 {
				t.Fatalf("partial progress not checkpointed or leaked: %+v, %+v", result, snap)
			}

			// Recreate a process failure after reserving the next fetch.
			if tc.want == learning.RunBudgetExhausted {
				record, err := c.load(id)
				if err != nil {
					t.Fatal(err)
				}
				record.Run.State, record.Run.StopReason, record.Run.EndedAt = learning.RunRunning, "", nil
				record.Run.Spent.Requests += 2
				record.Run.Checkpoint.Spent = record.Run.Spent
				if err := c.save(record); err != nil {
					t.Fatal(err)
				}
				cfg.PerQuestion.Requests = 8
				c = NewLearningCoordinator(dir)
				result, err = c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch, localtools.LearningDocumentReader{}, nil)
				if err != nil || result.StopReason != "interrupted_provider_call" ||
					result.CoverageScore != 0.5 || fetches != 1 {
					t.Fatalf("unexpected replay: %+v, %v", result, err)
				}
			}
		})
	}
}

func TestResearchReferenceIndependenceRejectsExactBodyDuplicates(t *testing.T) {
	first := learning.ResearchReference{
		Source:   learning.Source{CanonicalURI: "https://one.example.org/a", ContentHash: strings.Repeat("c", 64)},
		Evidence: &learning.Evidence{ExcerptOrSummary: "first"},
	}
	second := learning.ResearchReference{
		Source:   learning.Source{CanonicalURI: "https://two.example.net/a", ContentHash: first.Source.ContentHash},
		Evidence: &learning.Evidence{ExcerptOrSummary: "different excerpt"},
	}
	if independentResearchReference(second, []learning.ResearchReference{first}) {
		t.Fatal("duplicate fetched body counted as an independent source")
	}
}
