package runtime

import (
	"strings"

	"q-agent/internal/platform"
)

const maxPendingSteers = 16
const maxPendingSteerBytes = 64 << 10

// mergeFollowups preserves order and paragraph boundaries without replacing
// earlier corrections. Everything pending at one boundary is one user input.
func mergeFollowups(pending []platform.Message) string {
	texts := make([]string, len(pending))
	for i, m := range pending {
		texts[i] = m.Text
	}
	return strings.Join(texts, "\n\n")
}

// steerLocked accepts only input from the active task's owner and topic.
// The durable acknowledgement is queued before exposing input to the agent.
func (r *Runtime) steerLocked(m platform.Message, active task) error {
	if active.userCancelled || active.stopping || active.ctx.Err() != nil {
		return r.reply(m, "This task is stopping. Please resend after it finishes.")
	}
	size := len(m.Text) + 2*len(active.pending) // Include merged paragraph separators.
	for _, pending := range active.pending {
		size += len(pending.Text)
	}
	if len(active.pending) >= maxPendingSteers || size > maxPendingSteerBytes {
		return r.reply(m, "Too many pending follow-ups. Please wait before sending more.")
	}
	ack := "Follow-up received; steering the current task at the next safe boundary."
	if active.finishing {
		ack = "The task is finishing; this follow-up will start next."
	}
	if err := r.Store.QueueNotice(m, []string{ack}); err != nil {
		return err
	}
	active.pending = append(active.pending, m)
	if active.wake != nil {
		select {
		case active.wake <- struct{}{}:
		default:
		}
	}
	r.active[m.WorkspaceKey()] = active
	r.logf("[RUN] Follow-up accepted | update=%d pending_inputs=%d finishing=%t", m.UpdateID, len(active.pending), active.finishing)
	return nil
}

// takeSteer is the final-answer arbitration point: draining and sealing are
// serialized with Handle, so an acknowledged follow-up cannot miss both runs.
func (r *Runtime) takeSteer(key string, final bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.active[key]
	if active.finishing || active.userCancelled || active.stopping || active.ctx.Err() != nil {
		return nil
	}
	var inputs []string
	if len(active.pending) > 0 {
		inputs = []string{mergeFollowups(active.pending)}
	}
	active.pending = nil
	if active.wake != nil {
		select {
		case <-active.wake:
		default:
		}
	}
	if final && len(inputs) == 0 {
		active.finishing = true
	}
	r.active[key] = active
	return inputs
}

func (r *Runtime) discardSteer(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.active[key]
	count := len(active.pending)
	active.pending = nil
	// Reject further input until the failed/cancelled run actually exits.
	active.stopping = true
	r.active[key] = active
	return count
}
