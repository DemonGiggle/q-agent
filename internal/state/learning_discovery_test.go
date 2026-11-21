package state

import (
	"context"
	"errors"
	"testing"

	"q-agent/internal/learning"
)

type discoverySearch func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error)

func (s discoverySearch) Search(ctx context.Context, query string, limit int) ([]learning.SearchHit, learning.Budget, error) {
	return s(ctx, query, limit)
}

func TestLearningDiscoveryOnly(t *testing.T) {
	c := NewLearningCoordinator(t.TempDir())
	ctx := context.Background()
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.PerQuestion.Requests = 1
	cfg.Permissions.ExcludedTerms = []string{"private marker"}
	hits := []learning.SearchHit{{URI: "https://example.org/source", Title: "A lead"}}
	calls := 0
	search := discoverySearch(func(_ context.Context, query string, limit int) ([]learning.SearchHit, learning.Budget, error) {
		calls++
		if query != "Find source" || limit != 5 {
			t.Fatalf("unexpected request: %q, limit %d", query, limit)
		}
		return hits, learning.Budget{Requests: 1}, nil
	})
	if _, err := c.Discover(ctx, cfg, search, "Private marker"); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("excluded query: %v", err)
	}
	first, err := c.Discover(ctx, cfg, search, "  Find source  ")
	if err != nil || first.State != learning.RunUnresolved || first.Spent.Requests != 1 ||
		len(first.Hits) != 1 || first.Hits[0] != hits[0] {
		t.Fatalf("discovery result: %+v, err=%v", first, err)
	}
	second, err := c.Discover(ctx, cfg, search, "find  source")
	if !errors.Is(err, ErrLearningBudget) || second.State != learning.RunBudgetExhausted || calls != 1 {
		t.Fatalf("duplicate question quota: %+v, calls=%d, err=%v", second, calls, err)
	}
	snapshot, err := c.Repository().Snapshot(ctx, learning.Partition{Public: true})
	if err != nil || snapshot.Version != 0 || len(snapshot.Claims) != 0 || len(snapshot.Evidence) != 0 {
		t.Fatalf("discovery created knowledge: %+v, err=%v", snapshot, err)
	}
}
