package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"q-agent/internal/learning"
)

type learningNight struct {
	Window          string `json:"window,omitempty"`
	FreshnessWindow string `json:"freshness_window,omitempty"`
	DeferredWindow  string `json:"deferred_window,omitempty"`
	Pending         string `json:"pending_run_id,omitempty"`
	Active          string `json:"active_run_id,omitempty"`
}

func loadLearningNight(path string) (learningNight, error) {
	var night learningNight
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return night, nil
	}
	if err != nil {
		return night, err
	}
	defer f.Close()
	const limit = 1024
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return night, err
	}
	if len(data) > limit || json.Unmarshal(data, &night) != nil ||
		(night.Pending != "" && !auditID.MatchString(night.Pending)) ||
		(night.Active != "" && !auditID.MatchString(night.Active)) {
		return learningNight{}, fmt.Errorf("invalid overnight learning queue")
	}
	for _, date := range []string{night.Window, night.FreshnessWindow, night.DeferredWindow} {
		if _, err := time.Parse(time.DateOnly, date); date != "" && err != nil {
			return learningNight{}, fmt.Errorf("invalid overnight learning date")
		}
	}
	return night, nil
}

// ScheduleResearch explicitly approves an existing public run for overnight
// execution. The caller holds the learning-directory lock.
func (c *LearningCoordinator) ScheduleResearch(ctx context.Context, cfg learning.Config, id string, allow func(string) bool) error {
	if !cfg.Schedule.Enabled {
		return learning.ErrDenied
	}
	path := filepath.Join(filepath.Dir(c.dir), "night.json")
	night, err := loadLearningNight(path)
	if err != nil {
		return err
	}
	run, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	if err != nil {
		return err
	}
	if id == night.Pending || id == night.Active {
		return nil
	}
	if run.Run.State != learning.RunQueued {
		return learning.ErrDenied
	}
	if night.Pending != "" {
		prior, err := c.load(night.Pending)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !terminalLearningRun(prior.Run.State) {
			return fmt.Errorf("overnight learning queue already has a pending run")
		}
	}
	night.Pending = id
	return Atomic(path, night)
}

// RemoveScheduledResearch revokes unattended execution for the named run.
// Its checkpoint remains available for manual work.
func (c *LearningCoordinator) RemoveScheduledResearch(id string) error {
	if !auditID.MatchString(id) {
		return learning.ErrDenied
	}
	path := filepath.Join(filepath.Dir(c.dir), "night.json")
	night, err := loadLearningNight(path)
	if err != nil {
		return err
	}
	switch id {
	case night.Pending:
		night.Pending = ""
	case night.Active:
		night.Active = ""
	default:
		return nil
	}
	return Atomic(path, night)
}

// RunScheduledResearch admits at most one approved run per local window.
// The durable admission is saved before the worker can reach a provider.
// The caller holds the learning-directory lock across the operation.
func (c *LearningCoordinator) RunScheduledResearch(ctx context.Context, cfg learning.Config, window string, allow func(string) bool,
	search learning.SearchProvider, fetch learning.DocumentFetcher, reader learning.DocumentReader) error {
	if !cfg.Enabled || !cfg.Schedule.Enabled || window == "" {
		return nil
	}
	path := filepath.Join(filepath.Dir(c.dir), "night.json")
	night, err := loadLearningNight(path)
	if err != nil {
		return err
	}
	if night.Window > window || night.DeferredWindow >= window || (night.Active == "" && night.Window == window) {
		return nil
	}
	id := night.Active
	if id == "" {
		id = night.Pending
	}
	if id == "" {
		if cfg.SourcePolicy == nil || cfg.SourcePolicy.RevalidateDays == 0 || fetch == nil || allow == nil || window <= night.FreshnessWindow {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		night.FreshnessWindow = window
		if err := Atomic(path, night); err != nil {
			return err
		}
		return c.revalidateLearningSource(ctx, cfg, allow, fetch)
	}
	run, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	var policyAuditErr error
	if errors.Is(err, learning.ErrDenied) || (err == nil && search == nil) {
		record, loadErr := c.load(id)
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
		if loadErr == nil && record.PartitionKey == "public" && !terminalLearningRun(record.Run.State) {
			if err := c.cancelResearch(ctx, id, "policy_changed"); err != nil && !errors.Is(err, ErrLearningCommitted) {
				return err
			}
			if cfg.TelemetryEnabled {
				current, err := c.load(id)
				if err == nil {
					_, err = c.exportRunEvents(ctx, current, true)
				}
				policyAuditErr = err
			}
		}
		err = learning.ErrDenied
	}
	if errors.Is(err, learning.ErrDenied) || errors.Is(err, os.ErrNotExist) {
		if night.Active != "" {
			night.Active = ""
		} else {
			night.Pending = ""
		}
		return errors.Join(policyAuditErr, Atomic(path, night))
	}
	if err != nil {
		return err
	}
	if run.Run.State == learning.RunPaused && !c.cancelRequested(id) {
		return nil
	}
	if terminalLearningRun(run.Run.State) && night.Active == "" {
		night.Pending = ""
		return Atomic(path, night)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if night.Active == "" || night.Window != window {
		if night.Active == "" {
			night.Pending = ""
		}
		night.Active, night.Window = id, window
		if err := Atomic(path, night); err != nil {
			return err
		}
	}
	result, workErr := c.workResearch(ctx, cfg, id, allow, search, fetch, reader, nil, false, true)
	if errors.Is(workErr, ErrLearningLeaseHeld) {
		return nil
	}
	cleanup := context.WithoutCancel(ctx)
	_, auditErr := c.ExportResearchAudit(cleanup, cfg, id, allow)
	if terminalLearningRun(result.State) {
		night.Active = ""
		return errors.Join(workErr, auditErr, Atomic(path, night))
	}
	if ctx.Err() != nil {
		night.DeferredWindow = window
		return errors.Join(workErr, auditErr, Atomic(path, night))
	}
	return errors.Join(workErr, auditErr)
}
