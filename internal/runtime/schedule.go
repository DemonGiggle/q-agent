package runtime

import (
	"context"
	"fmt"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/telegram"
)

// startLocked is shared by Telegram and scheduled agent tasks. The caller
// holds mu and has checked authorization, capacity, and workspace exclusion.
func (r *Runtime) startLocked(ctx context.Context, m platform.Message, runID string) error {
	if runID != "" {
		// Scheduled tasks can be the first activity in a workspace. Register
		// their route before startup/nightly sweeps inspect busy chats.
		if err := r.Store.RecordChat(m, time.Now()); err != nil {
			r.report(err)
		}
		claimed, err := r.Store.ClaimRun(runID)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
	}
	taskCtx, cancel := context.WithTimeout(ctx, r.Config.TaskDuration())
	key := m.WorkspaceKey()
	r.active[key] = task{topic: m.TopicID, user: m.UserID, ctx: taskCtx, cancel: cancel}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cancel()
		succeeded := false
		defer func() {
			r.mu.Lock()
			finished := r.active[key]
			delete(r.active, key)
			// A message racing with final-answer sealing belongs to the next
			// run, but only after this run has saved history and queued output.
			if succeeded && finished.finishing && !finished.userCancelled && !r.closed && ctx.Err() == nil && len(finished.pending) > 0 {
				next := finished.pending
				message := next[0]
				message.Text = mergeFollowups(next)
				if err := r.startLocked(ctx, message, ""); err != nil {
					r.report(err)
				}
			}
			if r.digestWake != nil {
				select {
				case r.digestWake <- struct{}{}:
				default:
				}
			}
			if r.scheduleWake != nil {
				select {
				case r.scheduleWake <- struct{}{}:
				default:
				}
			}
			r.mu.Unlock()
		}()
		succeeded = r.run(taskCtx, m, runID)
	}()
	return nil
}

func (r *Runtime) dispatchScheduled(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || ctx.Err() != nil {
		return nil
	}
	if r.Maintenance != nil && r.Maintenance.Busy() {
		return nil
	}
	if r.active == nil {
		r.active = map[string]task{}
	}
	limit := r.Config.Limits.MaxConcurrent
	if limit <= 0 {
		limit = 2
	}
	for _, run := range r.Store.PendingRuns() {
		if !r.allowed(run.Route) {
			if err := r.Store.FinishRun(run.ID, "cancelled", nil); err != nil {
				return err
			}
			continue
		}
		if run.Kind == "reminder" {
			if err := r.Store.FinishRun(run.ID, "succeeded", telegram.FormatAndSplit(run.Text)); err != nil {
				return err
			}
			continue
		}
		if _, busy := r.active[run.Route.WorkspaceKey()]; busy || len(r.active) >= limit {
			continue
		}
		m := run.Route
		m.Text = fmt.Sprintf("Scheduled task %s (scheduled for %s). Execute the following saved request now:\n%s", run.ScheduleID, run.ScheduledAt.Format(time.RFC3339), run.Text)
		if err := r.startLocked(ctx, m, run.ID); err != nil {
			return err
		}
	}
	return nil
}

// StartScheduler recovers crash state before starting any delivery/worker loops.
// It scans immediately, then at wall-clock minute boundaries. A wake signal
// only drains queued work; it never changes the scheduled occurrence time.
func (r *Runtime) StartScheduler(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return context.Canceled
	}
	if r.scheduleDone != nil {
		return fmt.Errorf("scheduler already started")
	}
	if err := r.Store.RecoverRuns(); err != nil {
		return err
	}
	if err := r.Store.ScanDue(time.Now()); err != nil {
		return err
	}
	schedulerCtx, cancel := context.WithCancel(ctx)
	r.scheduleCancel = cancel
	r.scheduleDone = make(chan struct{})
	r.scheduleWake = make(chan struct{}, 1)
	go func() {
		defer close(r.scheduleDone)
		r.report(r.dispatchScheduled(schedulerCtx))
		timer := time.NewTimer(untilNextMinute())
		defer timer.Stop()
		for schedulerCtx.Err() == nil {
			select {
			case <-schedulerCtx.Done():
				timer.Stop()
				return
			case <-r.scheduleWake:
			case <-timer.C:
				r.report(r.Store.ScanDue(time.Now()))
				timer.Reset(untilNextMinute())
			}
			r.report(r.dispatchScheduled(schedulerCtx))
		}
	}()
	if r.Config.Memory.Enabled {
		digestCtx, digestCancel := context.WithCancel(ctx)
		r.digestCancel = digestCancel
		r.digestDone = make(chan struct{})
		r.digestWake = make(chan struct{}, 1)
		go func() {
			defer close(r.digestDone)
			r.digestLoop(digestCtx)
		}()
	}
	return nil
}

func untilNextMinute() time.Duration {
	now := time.Now()
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}
