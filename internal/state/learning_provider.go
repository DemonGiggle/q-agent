package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/platform"
)

const maxLearningLedgerBytes = 64 << 10

var (
	ErrLearningBudget         = errors.New("learning provider budget exhausted")
	ErrLearningProviderActive = errors.New("learning run already has an active provider call")
	ErrLearningProviderUsage  = errors.New("learning provider exceeded its reservation")
)

type learningQuota struct {
	Date      string                     `json:"utc_date"`
	Spent     learning.Budget            `json:"spent"`
	Questions map[string]learning.Budget `json:"questions"`
}

// CallProvider runs one trusted, context-aware adapter after durably charging
// its upper bound. The adapter must enforce that bound at the provider and
// propagate the context to its I/O. The caller may use output only on success.
func (c *LearningCoordinator) CallProvider(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan, cost learning.Budget, timeout time.Duration, call func(context.Context) (learning.Budget, error)) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !cfg.Enabled {
		return learning.ErrDenied
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	if call == nil || timeout <= 0 || cost.Requests <= 0 || cost.WallTimeMillis < int64((timeout-1)/time.Millisecond+1) {
		return fmt.Errorf("invalid learning provider reservation or timeout")
	}
	if _, ok := (learning.Budget{}).Reserve(cost, cost); !ok {
		return fmt.Errorf("negative learning provider reservation")
	}
	callCtx, err := c.admitProvider(ctx, lease, cfg, plan, cost, timeout)
	if err != nil {
		return err
	}
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.activeCalls[lease.RunID]()
		delete(c.activeCalls, lease.RunID)
	}()
	if err := callCtx.Err(); err != nil {
		return err
	}
	observed, err := call(callCtx)
	if ctxErr := callCtx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, ok := (learning.Budget{}).Reserve(observed, cost); !ok {
		return ErrLearningProviderUsage
	}
	return err
}

func (c *LearningCoordinator) admitProvider(ctx context.Context, lease learning.Lease, cfg learning.Config, plan learning.Plan, cost learning.Budget, timeout time.Duration) (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, err := c.load(lease.RunID)
	if err != nil {
		return nil, err
	}
	if !c.owns(record, lease) || record.Run.PlanID != plan.ID || record.Run.State != learning.RunRunning {
		return nil, learning.ErrDenied
	}
	if c.activeCalls[lease.RunID] != nil {
		return nil, ErrLearningProviderActive
	}
	if record.Version == math.MaxUint64 {
		return nil, learning.ErrVersionConflict
	}

	now := c.now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	deadline := now.Add(timeout)
	if deadline.After(midnight) || deadline.After(lease.ExpiresAt) {
		return nil, context.DeadlineExceeded
	}
	path := filepath.Join(filepath.Dir(c.dir), "quota.json")
	quota, err := loadLearningQuota(path, now.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	question := platform.WorkspaceKey(record.PartitionKey+":"+plan.QuestionID, 0)
	if record.QuestionKey != "" && (record.QuestionKey != question || record.PlanBudget == nil || *record.PlanBudget != plan.Budget) {
		return nil, learning.ErrVersionConflict
	}
	nextRun, runFits := record.Run.Spent.Reserve(cost, cfg.PerRun)
	_, questionRunFits := record.Run.Spent.Reserve(cost, cfg.PerQuestion)
	_, planFits := record.Run.Spent.Reserve(cost, plan.Budget)
	nextQuestion, questionFits := quota.Questions[question].Reserve(cost, cfg.PerQuestion)
	nextDay, dayFits := quota.Spent.Reserve(cost, cfg.GlobalDaily)
	cp := learning.Checkpoint{}
	if record.Run.Checkpoint != nil {
		cp = *record.Run.Checkpoint
	}
	record.Version++
	cp.Version = record.Version
	record.Run.Checkpoint = &cp
	if now.After(record.Run.UpdatedAt) {
		record.Run.UpdatedAt = now
	}
	if !runFits || !planFits || !questionRunFits || !questionFits || !dayFits {
		record.Run.State = learning.RunBudgetExhausted
		record.Run.StopReason = "provider_budget"
		record.Run.EndedAt = &record.Run.UpdatedAt
		record.ExpiresAt = nil
		return nil, errors.Join(ErrLearningBudget, c.save(record))
	}

	quota.Spent, quota.Questions[question] = nextDay, nextQuestion
	body, err := json.Marshal(quota)
	if err != nil {
		return nil, err
	}
	if len(body) > maxLearningLedgerBytes {
		return nil, fmt.Errorf("learning quota ledger exceeds storage limit")
	}
	// Debit the shared budget first. An interruption between writes may
	// overcharge the shared budget, but no provider will have been called.
	if err := atomicBytes(path, body); err != nil {
		return nil, err
	}
	record.QuestionKey, record.PlanBudget = question, &plan.Budget
	record.Run.Spent, cp.Spent = nextRun, nextRun
	if err := c.save(record); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	if c.activeCalls == nil {
		c.activeCalls = make(map[string]context.CancelFunc)
	}
	c.activeCalls[lease.RunID] = cancel
	return callCtx, nil
}

func loadLearningQuota(path, today string) (learningQuota, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return learningQuota{Date: today, Questions: make(map[string]learning.Budget)}, nil
	}
	if err != nil {
		return learningQuota{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLearningLedgerBytes+1))
	if err != nil {
		return learningQuota{}, err
	}
	if len(raw) > maxLearningLedgerBytes {
		return learningQuota{}, fmt.Errorf("learning quota ledger exceeds storage limit")
	}
	var ledger learningQuota
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return learningQuota{}, err
	}
	if _, err := time.Parse(time.DateOnly, ledger.Date); err != nil || ledger.Questions == nil {
		return learningQuota{}, fmt.Errorf("invalid learning quota ledger")
	}
	if ledger.Date > today {
		return learningQuota{}, fmt.Errorf("learning quota clock moved backward")
	}
	if ledger.Date < today {
		return learningQuota{Date: today, Questions: make(map[string]learning.Budget)}, nil
	}
	return ledger, nil
}
