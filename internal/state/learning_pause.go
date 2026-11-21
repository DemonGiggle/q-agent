package state

import (
	"context"
	"math"

	"q-agent/internal/learning"
)

// SetResearchPaused changes only the run's execution state. The caller
// holds the learning directory lock, so no other process can own a worker
// while the operation runs. A live lease cannot be interrupted here.
func (c *LearningCoordinator) SetResearchPaused(ctx context.Context, cfg learning.Config, id string,
	allow func(string) bool, paused bool) (LearningWorkerResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	if err != nil {
		return LearningWorkerResult{}, err
	}
	if terminalLearningRun(record.Run.State) || record.Run.State == learning.RunCommitting {
		return LearningWorkerResult{}, learning.ErrDenied
	}
	now := c.now().UTC()
	if c.activeCalls[id] != nil || (record.ExpiresAt != nil && now.Before(*record.ExpiresAt)) {
		return LearningWorkerResult{}, ErrLearningLeaseHeld
	}

	if paused == (record.Run.State == learning.RunPaused) {
		return researchWorkerResult(record), SyncDir(c.dir)
	}
	if record.Version == math.MaxUint64 {
		return LearningWorkerResult{}, learning.ErrVersionConflict
	}
	if paused {
		if !pausableResearchState(record.Run.State) {
			return LearningWorkerResult{}, learning.ErrDenied
		}
		record.ResumeState = record.Run.State
		record.Run.State, record.Run.StopReason = learning.RunPaused, "operator_paused"
	} else {
		if !pausableResearchState(record.ResumeState) {
			return LearningWorkerResult{}, learning.ErrDenied
		}
		record.Run.State, record.Run.StopReason = record.ResumeState, ""
		record.ResumeState = ""
	}
	if now.After(record.Run.UpdatedAt) {
		record.Run.UpdatedAt = now
	}
	record.ExpiresAt = nil
	record.Version++
	if err := ctx.Err(); err != nil {
		return LearningWorkerResult{}, err
	}
	if err := c.save(record); err != nil {
		return LearningWorkerResult{}, err
	}
	return c.ResearchStatus(ctx, cfg, id, allow)
}

func pausableResearchState(state learning.RunState) bool {
	switch state {
	case learning.RunQueued, learning.RunPlanned, learning.RunRunning, learning.RunValidating:
		return true
	}
	return false
}
