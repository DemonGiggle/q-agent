package state

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
)

type researchFetcher func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error)

func (f researchFetcher) Fetch(ctx context.Context, uri string, limit int64) (learning.FetchedDocument, learning.Budget, error) {
	return f(ctx, uri, limit)
}

func TestLearningWorker(t *testing.T) {
	const uri = "https://example.org/reference"
	for _, tc := range []struct {
		name     string
		maxCalls int64
		want     learning.RunState
	}{
		{"store source", 3, learning.RunCompleted},
		{"budget stop", 2, learning.RunBudgetExhausted},
		{"fetch failed", 3, learning.RunUnresolved},
		{"cancel active fetch", 3, learning.RunCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			partition := learning.Partition{Public: true}
			cfg := learning.DefaultConfig()
			cfg.Enabled = true
			cfg.PerQuestion.Requests = tc.maxCalls
			cfg.Permissions.AllowDomains = []string{"example.org"}
			id, err := NewLearningCoordinator(dir).EnqueueResearch(ctx, cfg, "Public question")
			if err != nil {
				t.Fatal(err)
			}
			c := NewLearningCoordinator(dir)
			allow := func(string) bool { return true }
			searchCalls, fetchCalls := 0, 0
			search := discoverySearch(func(_ context.Context, query string, limit int) ([]learning.SearchHit, learning.Budget, error) {
				searchCalls++
				if query != "Public question" || limit != 5 {
					t.Fatalf("search inputs: %q, %d", query, limit)
				}
				return []learning.SearchHit{
					{URI: "https://unapproved.example/reference"},
					{URI: uri + "?tracking=1"},
					{URI: uri, Title: "Unverified title", Snippet: "Unverified claim"},
				}, learning.Budget{Requests: 1}, nil
			})
			fetch := researchFetcher(func(callCtx context.Context, link string, limit int64) (learning.FetchedDocument, learning.Budget, error) {
				fetchCalls++
				if link != uri || limit != learningDocumentLimit {
					t.Fatalf("fetch inputs: %q, %d", link, limit)
				}
				if tc.want == learning.RunCancelled {
					if err := c.Cancel(ctx, id); err != nil {
						t.Fatal(err)
					}
					if callCtx.Err() != context.Canceled {
						t.Fatal("provider context was not cancelled")
					}
				}
				if tc.want == learning.RunUnresolved {
					return learning.FetchedDocument{}, learning.Budget{Requests: 2}, errors.New("fetch unavailable")
				}
				now := time.Now().UTC()
				return learning.FetchedDocument{Source: learning.Source{
					Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
					ID:     "source", CanonicalURI: link, FetchedAt: now, Title: "Fetched title",
					ContentHash: strings.Repeat("d", 64), TrustClass: "public_reference",
				}}, learning.Budget{Requests: 2, Documents: 1}, nil
			})
			result, err := c.WorkResearch(ctx, cfg, id, allow, search, fetch)
			if result.State != tc.want || searchCalls != 1 ||
				(tc.want == learning.RunBudgetExhausted && !errors.Is(err, ErrLearningBudget)) ||
				(tc.want != learning.RunBudgetExhausted && err != nil) {
				t.Fatalf("result: %+v, search=%d, err=%v", result, searchCalls, err)
			}
			wantFetch := 1
			if tc.want == learning.RunBudgetExhausted {
				wantFetch = 0
			}
			if fetchCalls != wantFetch || result.Transitions[0].State != learning.RunQueued ||
				result.Transitions[len(result.Transitions)-1].State != tc.want {
				t.Fatalf("fetch=%d, history: %+v", fetchCalls, result.Transitions)
			}
			snapshot, err := c.Repository().Snapshot(ctx, partition)
			if err != nil || len(snapshot.Claims) != 0 || len(snapshot.Evidence) != 0 {
				t.Fatalf("snapshot: %+v, err=%v", snapshot, err)
			}
			if tc.want != learning.RunCompleted {
				if snapshot.Version != 0 {
					t.Fatalf("failed run wrote knowledge: %+v", snapshot)
				}
				return
			}
			var states []learning.RunState
			for _, tr := range result.Transitions {
				if tr.At.IsZero() || tr.Reason == "" {
					t.Fatalf("incomplete transition: %+v", tr)
				}
				states = append(states, tr.State)
			}
			if !slices.Equal(states, []learning.RunState{learning.RunQueued, learning.RunPlanned, learning.RunRunning,
				learning.RunValidating, learning.RunCommitting, learning.RunCompleted}) ||
				snapshot.Version != 1 || len(snapshot.Notes) != 1 || len(snapshot.Sources) != 1 ||
				snapshot.Sources[0].Title != "Fetched title" || snapshot.Notes[0].SourceIDs[0] != "source" {
				t.Fatalf("states: %v, snapshot: %+v", states, snapshot)
			}

			// Restore the last run checkpoint before completion, leaving
			// the already committed knowledge and its receipt on disk.
			record, err := c.load(id)
			if err != nil {
				t.Fatal(err)
			}
			record.Run.State, record.Run.StopReason, record.Run.EndedAt = learning.RunCommitting, "", nil
			record.Run.Checkpoint.NextIntent = "commit"
			record.Transitions = record.Transitions[:len(record.Transitions)-1]
			if err := c.save(record); err != nil {
				t.Fatal(err)
			}
			c = NewLearningCoordinator(dir)
			result, err = c.WorkResearch(ctx, cfg, id, allow, search, fetch)
			latest, snapErr := c.Repository().Snapshot(ctx, partition)
			if err != nil || snapErr != nil || result.State != learning.RunCompleted || latest.Version != 1 ||
				len(latest.Notes) != 1 || searchCalls != 1 || fetchCalls != 1 {
				t.Fatalf("restart: %+v, snapshot: %+v, err=%v/%v", result, latest, err, snapErr)
			}
		})
	}
}

func TestLearningWorkerDoesNotReplayAnUncertainCall(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	c := NewLearningCoordinator(dir)
	id, err := c.EnqueueResearch(ctx, cfg, "Public question")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after budget admission but before saving the response.
	record, err := c.load(id)
	if err != nil {
		t.Fatal(err)
	}
	record.Run.State = learning.RunRunning
	record.Run.Spent = learning.Budget{Requests: 1, WallTimeMillis: 15000}
	record.Run.Checkpoint = &learning.Checkpoint{NextIntent: "search", Spent: record.Run.Spent}
	if err := c.save(record); err != nil {
		t.Fatal(err)
	}
	c = NewLearningCoordinator(dir)
	search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
		t.Fatal("replayed an uncertain search")
		return nil, learning.Budget{}, nil
	})
	fetch := researchFetcher(func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error) {
		t.Fatal("opened a document without a saved search response")
		return learning.FetchedDocument{}, learning.Budget{}, nil
	})
	result, err := c.WorkResearch(ctx, cfg, id, func(string) bool { return true }, search, fetch)
	if err != nil || result.State != learning.RunUnresolved || result.StopReason != "interrupted_provider_call" ||
		result.Spent != record.Run.Spent {
		t.Fatalf("restart: %+v, err=%v", result, err)
	}
}
