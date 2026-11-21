package state

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/platform"
)

type LearningDiscovery struct {
	RunID string               `json:"run_id"`
	State learning.RunState    `json:"state"`
	Spent learning.Budget      `json:"spent_budget"`
	Hits  []learning.SearchHit `json:"discovery_hits"`
}

// Discover executes one manual, public search. The caller authorizes the
// supplied query for egress. Chat history is outside this entry point.
func (c *LearningCoordinator) Discover(ctx context.Context, cfg learning.Config, provider learning.SearchProvider, query string) (result LearningDiscovery, retErr error) {
	// Query length and result count match the existing web search limits.
	const maxHits = 5
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 2000 || provider == nil {
		return result, fmt.Errorf("learning search requires a 1–2000 byte query and search provider")
	}
	if err := cfg.Validate(); err != nil {
		return result, err
	}
	if !cfg.Enabled || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: query}) {
		return result, learning.ErrDenied
	}
	ms := min(int64(15_000), cfg.PerQuestion.WallTimeMillis)
	now := c.now().UTC()
	plan := learning.Plan{
		Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
		ID:     rand.Text(), QuestionID: platform.WorkspaceKey(strings.ToLower(strings.Join(strings.Fields(query), " ")), 0),
		Goal: query, Steps: []string{"discover"}, StopConditions: []string{"search finished"},
		AllowedSourceClasses: []string{"public"}, Version: 1, Budget: cfg.PerQuestion,
		EvidenceRequirements: learning.EvidenceRequirements{MinIndependentSources: 1},
	}
	id := rand.Text()
	if err := c.Create(ctx, learning.Partition{Public: true}, id, plan.ID); err != nil {
		return result, err
	}
	result.RunID = id
	lease, err := c.Acquire(ctx, id, time.Minute)
	if err != nil {
		return result, err
	}
	defer func() {
		persistCtx := context.WithoutCancel(ctx)
		if ctx.Err() != nil {
			retErr = errors.Join(retErr, c.Cancel(persistCtx, id))
		}
		retErr = errors.Join(retErr, c.Release(persistCtx, lease))
		state, err := c.Get(persistCtx, id)
		retErr = errors.Join(retErr, err)
		if err == nil {
			result.State, result.Spent = state.Run.State, state.Run.Spent
		}
	}()
	advance := func(ctx context.Context, target learning.RunState, reason string) error {
		run, err := c.Get(ctx, id)
		if err != nil {
			return err
		}
		run.Run.State, run.Run.StopReason = target, reason
		return c.Checkpoint(ctx, lease, run.Version, run.Run)
	}
	if err := advance(ctx, learning.RunPlanned, ""); err != nil {
		return result, err
	}
	if err := advance(ctx, learning.RunRunning, ""); err != nil {
		return result, err
	}
	charge := learning.Budget{Requests: 1, WallTimeMillis: ms}
	var hits []learning.SearchHit
	err = c.CallProvider(ctx, lease, cfg, plan, charge, time.Duration(ms)*time.Millisecond, func(callCtx context.Context) (learning.Budget, error) {
		var usage learning.Budget
		var err error
		hits, usage, err = provider.Search(callCtx, query, maxHits)
		return usage, err
	})
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, ErrLearningBudget) {
			err = errors.Join(err, advance(ctx, learning.RunFailed, "search_failed"))
		}
		return result, err
	}
	if err := advance(ctx, learning.RunUnresolved, "discovery_only"); err != nil {
		return result, err
	}
	result.Hits = hits
	return result, nil
}
