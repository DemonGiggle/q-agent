package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"q-agent/internal/learning"
)

type LearningCancellation struct {
	RunID     string            `json:"run_id"`
	Requested bool              `json:"requested"`
	State     learning.RunState `json:"state"`
}

func (c *LearningCoordinator) RequestResearchCancel(ctx context.Context, cfg learning.Config, id string, allow func(string) bool) (LearningCancellation, error) {
	var result LearningCancellation
	record, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	if err != nil {
		return result, err
	}
	result = LearningCancellation{RunID: id, State: record.Run.State}
	if record.Run.State == learning.RunCancelled {
		return result, nil
	}
	if terminalLearningRun(record.Run.State) {
		return result, learning.ErrDenied
	}
	unlock, err := Lock(filepath.Dir(c.dir))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		// The active worker owns the lease. It observes this intent
		// directly rather than allowing a second writer onto its state.
		err = Atomic(c.cancelPath(id), id)
		result.Requested = err == nil
		return result, err
	}
	if err != nil {
		return result, err
	}
	defer unlock()
	if err := c.Cancel(ctx, id); err != nil {
		return result, err
	}
	_ = os.Remove(c.cancelPath(id))
	result.State = learning.RunCancelled
	return result, nil
}

func (c *LearningCoordinator) cancelPath(id string) string {
	return filepath.Join(filepath.Dir(c.dir), "cancel", id+".json")
}

func (c *LearningCoordinator) cancelRequested(id string) bool {
	_, err := os.Stat(c.cancelPath(id))
	return err == nil
}

func (c *LearningCoordinator) applyResearchCancel(ctx context.Context, id string) error {
	err := c.Cancel(ctx, id)
	if err == nil || errors.Is(err, ErrLearningCommitted) || errors.Is(err, learning.ErrDenied) {
		_ = os.Remove(c.cancelPath(id))
	}
	return err
}

// Only an active worker polls for operator requests. Cancellation uses the
// same fence and commit check as in-process cancellation.
func (c *LearningCoordinator) watchResearchCancel(ctx context.Context, id string, cancel context.CancelFunc) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			if c.cancelRequested(id) {
				err := c.applyResearchCancel(context.WithoutCancel(ctx), id)
				if err == nil {
					cancel()
				}
				return
			}
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
