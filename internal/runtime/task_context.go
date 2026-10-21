package runtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

// taskContext owns a replaceable task deadline. Extending a parent
// context.WithTimeout would leave its original timer able to cancel sessions.
type taskContext struct {
	context.Context
	cancelCause context.CancelCauseFunc
	mu          sync.Mutex
	deadline    time.Time
	timer       *time.Timer
	started     time.Time
	interactive bool
}

func newTaskContext(parent context.Context, budget time.Duration) (*taskContext, context.CancelFunc) {
	base, cancel := context.WithCancelCause(parent)
	c := &taskContext{Context: base, cancelCause: cancel, started: time.Now()}
	c.setDeadline(c.started.Add(budget))
	return c, func() { c.mu.Lock(); defer c.mu.Unlock(); c.timer.Stop(); c.cancelCause(context.Canceled) }
}
func (c *taskContext) setDeadline(deadline time.Time) {
	if parent, ok := c.Context.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	c.deadline = deadline
	c.timer = time.AfterFunc(time.Until(deadline), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// An old timer racing with an extension must not cancel the new budget.
		if !time.Now().Before(c.deadline) {
			c.cancelCause(context.DeadlineExceeded)
		}
	})
}
func (c *taskContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, true
}
func (c *taskContext) Err() error {
	if err := c.Context.Err(); err != nil {
		if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return context.Canceled
	}
	return nil
}
func (c *taskContext) openInteractive(budget time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.interactive || c.Context.Err() != nil {
		return
	}
	c.interactive = true
	deadline := c.started.Add(budget)
	if deadline.After(c.deadline) {
		c.timer.Stop()
		c.setDeadline(deadline)
	}
}
