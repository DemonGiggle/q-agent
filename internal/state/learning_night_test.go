package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningNightPrefersManualApproval(t *testing.T) {
	ctx := context.Background()
	c := NewLearningCoordinator(t.TempDir())
	cfg := learning.DefaultConfig()
	cfg.Enabled, cfg.Schedule.Enabled = true, true
	allow := func(string) bool { return true }
	const question = "An operator-approved question"
	id, err := c.EnqueueResearch(ctx, cfg, question, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ScheduleResearch(ctx, cfg, id, allow); err != nil {
		t.Fatal(err)
	}
	discover := func(context.Context) ([]string, error) {
		t.Fatal("manual approval must run before automatic topic discovery")
		return nil, nil
	}
	search := discoverySearch(func(_ context.Context, query string, _ int) ([]learning.SearchHit, learning.Budget, error) {
		if query != question {
			t.Fatalf("unexpected search query %q", query)
		}
		return nil, learning.Budget{Requests: 1}, nil
	})
	if err := c.RunScheduledResearchWithDiscovery(ctx, cfg, "2026-11-22", allow, search,
		localtools.LearningFetch{}, localtools.LearningDocumentReader{}, discover); err != nil {
		t.Fatal(err)
	}
	status, err := c.ResearchStatus(ctx, cfg, id, allow)
	if err != nil || status.Spent.Requests != 1 {
		t.Fatalf("manual run did not search: %+v, %v", status, err)
	}
}

func TestLearningNightRecoversStagedDiscovery(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "discovery disabled"
		if enabled {
			name = "replay without another extraction"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			cfg := learning.DefaultConfig()
			cfg.Enabled, cfg.Schedule.Enabled = true, true
			allow := func(string) bool { return true }
			const window, query = "2026-11-22", "A recovered public query"
			id := automaticResearchID("run", query)
			path := filepath.Join(dir, "learning", "night.json")
			if err := Atomic(path, learningNight{
				Window: window, DiscoveryWindow: window, Automatic: true, StagedQuery: query, Active: id,
			}); err != nil {
				t.Fatal(err)
			}

			var discover func(context.Context) ([]string, error)
			if enabled {
				discover = func(context.Context) ([]string, error) {
					t.Fatal("recovered query must not be extracted again")
					return nil, nil
				}
			}
			search := discoverySearch(func(_ context.Context, got string, _ int) ([]learning.SearchHit, learning.Budget, error) {
				if !enabled || got != query {
					t.Fatalf("unexpected search %q with discovery enabled=%t", got, enabled)
				}
				return nil, learning.Budget{Requests: 1}, nil
			})
			c := NewLearningCoordinator(dir)
			if err := c.RunScheduledResearchWithDiscovery(ctx, cfg, window, allow, search,
				localtools.LearningFetch{}, localtools.LearningDocumentReader{}, discover); err != nil {
				t.Fatal(err)
			}
			night, err := loadLearningNight(path)
			if err != nil || night.Active != "" || night.StagedQuery != "" || night.Automatic {
				t.Fatalf("admission was not cleared: %+v, %v", night, err)
			}
			status, err := c.ResearchStatus(ctx, cfg, id, allow)
			if enabled {
				if err != nil || status.Spent.Requests != 1 {
					t.Fatalf("recovered run did not search: %+v, %v", status, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("disabled discovery created a run: %+v, %v", status, err)
			}
		})
	}
}
