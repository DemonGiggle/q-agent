package state

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"q-agent/internal/learning"
)

func TestLearningProviderBudgetSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	plan := providerTestPlan(cfg.PerQuestion)
	plan.Budget.Requests = 2
	lease := startProviderRun(t, c, plan)
	charge := learning.Budget{Requests: 1, WallTimeMillis: 1000}
	calls := 0
	call := func(ctx context.Context) (learning.Budget, error) {
		calls++
		run, err := c.Get(ctx, lease.RunID)
		if err != nil || run.Run.Checkpoint.Spent.Requests != int64(calls) {
			t.Fatalf("provider called before debit: %+v, %v", run, err)
		}
		return learning.Budget{Requests: 1}, nil
	}
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, call); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	c = NewLearningCoordinator(dir)
	var err error
	lease, err = c.Acquire(ctx, lease.RunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, call); err != nil {
		t.Fatal(err)
	}
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, call); !errors.Is(err, ErrLearningBudget) {
		t.Fatalf("over limit: %v", err)
	}
	run, err := c.Get(ctx, lease.RunID)
	if err != nil || run.Run.State != learning.RunBudgetExhausted ||
		run.Run.Checkpoint.Spent.Requests != 2 || calls != 2 {
		t.Fatalf("budget stop: %+v, calls=%d, err=%v", run, calls, err)
	}
}

func TestLearningResearchHTTPRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		firstErr error
		wantErr  error
		limit    int64
		calls    int64
	}{
		{"one retry", learning.ErrRetryableHTTP, learning.ErrRetryableHTTP, 3, 2},
		{"budget exhausted", learning.ErrRetryableHTTP, ErrLearningBudget, 1, 1},
		{"unknown outcome", context.DeadlineExceeded, context.DeadlineExceeded, 3, 1},
		{"policy denial", learning.ErrDenied, learning.ErrDenied, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := NewLearningCoordinator(t.TempDir())
			cfg := learning.DefaultConfig()
			cfg.Enabled, cfg.PerQuestion.Requests = true, tc.limit
			plan := providerTestPlan(cfg.PerQuestion)
			lease := startProviderRun(t, c, plan)
			charge := learning.Budget{Requests: 1, WallTimeMillis: 1000}
			var calls int64
			var first time.Time
			err := c.callResearchHTTP(ctx, lease, cfg, plan, charge, func(ctx context.Context) (learning.Budget, error) {
				calls++
				run, err := c.Get(ctx, lease.RunID)
				wall := calls * charge.WallTimeMillis
				if calls == 1 {
					first = time.Now()
				} else {
					wall += researchRetryDelay.Milliseconds()
					if time.Since(first) < researchRetryDelay {
						t.Error("retry skipped backoff")
					}
				}
				if err != nil || run.Run.Spent.Requests != calls || run.Run.Spent.WallTimeMillis != wall {
					t.Fatalf("dispatched before reserving budget: %+v, %v", run, err)
				}
				return charge, tc.firstErr
			})
			run, getErr := c.Get(ctx, lease.RunID)
			if !errors.Is(err, tc.wantErr) || getErr != nil || calls != tc.calls ||
				(tc.wantErr == ErrLearningBudget && run.Run.State != learning.RunBudgetExhausted) {
				t.Fatalf("run = %+v, calls = %d, errors = %v / %v", run, calls, err, getErr)
			}
		})
	}
}

func TestLearningProviderSharedLimits(t *testing.T) {
	ctx := context.Background()
	c := NewLearningCoordinator(t.TempDir())
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.GlobalDaily.Requests, cfg.PerRun.Requests, cfg.PerQuestion.Requests = 2, 2, 1
	plan := providerTestPlan(cfg.PerQuestion)
	charge := learning.Budget{Requests: 1, WallTimeMillis: 1000}
	call := func(context.Context) (learning.Budget, error) { return charge, nil }
	blocked := func(context.Context) (learning.Budget, error) {
		t.Fatal("provider called after exceeding quota")
		return charge, nil
	}
	lease := startProviderRun(t, c, plan)
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, call); err != nil {
		t.Fatal(err)
	}
	plan.ID = rand.Text()
	lease = startProviderRun(t, c, plan)
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, blocked); !errors.Is(err, ErrLearningBudget) {
		t.Fatalf("same question in a new run: %v", err)
	}
	plan.ID, plan.QuestionID = rand.Text(), rand.Text()
	lease = startProviderRun(t, c, plan)
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, call); err != nil {
		t.Fatal(err)
	}
	plan.ID, plan.QuestionID = rand.Text(), rand.Text()
	lease = startProviderRun(t, c, plan)
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, blocked); !errors.Is(err, ErrLearningBudget) {
		t.Fatalf("different question at daily limit: %v", err)
	}
}

func TestLearningProviderCancellationAndTimeout(t *testing.T) {
	ctx := context.Background()
	c := NewLearningCoordinator(t.TempDir())
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	plan := providerTestPlan(cfg.PerQuestion)
	lease := startProviderRun(t, c, plan)
	charge := learning.Budget{Requests: 1, WallTimeMillis: 1000}
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Millisecond, func(callCtx context.Context) (learning.Budget, error) {
		<-callCtx.Done()
		return learning.Budget{}, callCtx.Err()
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if err := c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, func(context.Context) (learning.Budget, error) {
		return learning.Budget{Requests: 2}, nil
	}); !errors.Is(err, ErrLearningProviderUsage) {
		t.Fatalf("understated usage: %v", err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.CallProvider(ctx, lease, cfg, plan, charge, time.Second, func(callCtx context.Context) (learning.Budget, error) {
			close(started)
			<-callCtx.Done()
			return learning.Budget{}, callCtx.Err()
		})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("provider did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start in time")
	}
	if err := c.Cancel(ctx, lease.RunID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("provider cancellation: %v", err)
	}
}

func providerTestPlan(budget learning.Budget) learning.Plan {
	now := time.Now().UTC()
	return learning.Plan{
		Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
		ID:     rand.Text(), QuestionID: rand.Text(), Goal: "find source", Steps: []string{"discover"},
		StopConditions: []string{"budget"}, AllowedSourceClasses: []string{"public"}, Budget: budget, Version: 1,
		EvidenceRequirements: learning.EvidenceRequirements{MinIndependentSources: 1},
	}
}

func startProviderRun(t *testing.T, c *LearningCoordinator, plan learning.Plan) learning.Lease {
	t.Helper()
	ctx := context.Background()
	id := rand.Text()
	if err := c.Create(ctx, learning.Partition{Public: true}, id, plan.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	advanceLearningRun(t, c, lease, learning.RunPlanned, "")
	advanceLearningRun(t, c, lease, learning.RunRunning, "")
	return lease
}
