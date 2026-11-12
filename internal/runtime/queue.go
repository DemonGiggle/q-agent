package runtime

import (
	"context"

	"q-agent/internal/platform"
)

const maxQueuedTasks = 16
const maxQueuedBytes = 64 << 10
const maxQueuedImageBytes = 20 << 20

const queuedAck = "Queued. Will start when the active task finishes."

// queueTaskLocked enqueues a non-steerable message to run after capacity
// frees. The caller holds mu and has already decided the message cannot
// start immediately (workspace busy or global limit reached).
func (r *Runtime) queueTaskLocked(m platform.Message) error {
	if len(r.queued) >= maxQueuedTasks {
		return r.reply(m, "Too many queued tasks. Please wait before sending more.")
	}
	size := len(m.Text)
	for _, q := range r.queued {
		size += len(q.Text)
	}
	// Include paragraph separators roughly as steer does.
	size += 2 * len(r.queued)
	if size > maxQueuedBytes {
		return r.reply(m, "Too many queued tasks. Please wait before sending more.")
	}
	imageBytes := 0
	for _, img := range m.Images {
		imageBytes += len(img.Data)
	}
	for _, q := range r.queued {
		for _, img := range q.Images {
			imageBytes += len(img.Data)
		}
	}
	if imageBytes > maxQueuedImageBytes {
		return r.reply(m, "Too many pending images. Please wait before sending more.")
	}
	if err := r.Store.QueueNotice(m, []string{queuedAck}); err != nil {
		return err
	}
	r.queued = append(r.queued, m)
	r.logf("[RUN] Task queued | update=%d queue_depth=%d", m.UpdateID, len(r.queued))
	return nil
}

// hasQueuedForLocked reports whether any queued task belongs to the given
// workspace. Caller holds mu.
func (r *Runtime) hasQueuedForLocked(key string) bool {
	for _, q := range r.queued {
		if q.WorkspaceKey() == key {
			return true
		}
	}
	return false
}

// cancelQueuedLocked removes queued messages for the same conversation
// (same workspace and topic). Caller holds mu. Returns removed count.
func (r *Runtime) cancelQueuedLocked(m platform.Message) int {
	key := m.WorkspaceKey()
	kept := r.queued[:0]
	removed := 0
	for _, q := range r.queued {
		if q.WorkspaceKey() == key && q.TopicID == m.TopicID {
			removed++
			continue
		}
		kept = append(kept, q)
	}
	// Clear tail to avoid retaining references.
	for i := len(kept); i < len(r.queued); i++ {
		r.queued[i] = platform.Message{}
	}
	r.queued = kept
	return removed
}

// drainQueuedLocked starts queued tasks that are now eligible, in FIFO
// order with skipping to avoid head-of-line blocking: a queued task for a
// still-busy workspace does not block queued tasks for free workspaces.
// Caller holds mu.
func (r *Runtime) drainQueuedLocked(parentCtx context.Context) {
	if r.closed || parentCtx.Err() != nil {
		return
	}
	if len(r.queued) == 0 {
		return
	}
	limit := r.Config.Limits.MaxConcurrent
	if limit <= 0 {
		limit = 2
	}
	for i := 0; i < len(r.queued); {
		if r.closed || parentCtx.Err() != nil {
			return
		}
		q := r.queued[i]
		qKey := q.WorkspaceKey()
		if _, busy := r.active[qKey]; busy || len(r.active) >= limit {
			i++
			continue
		}
		// Remove before starting so re-entrancy sees consistent state.
		r.queued = append(r.queued[:i], r.queued[i+1:]...)
		if err := r.startLocked(parentCtx, q, ""); err != nil {
			r.report(err)
			// startLocked for interactive tasks only fails on internal
			// errors; the message is already dequeued to avoid a spin.
			continue
		}
		// Do not increment i: slice shrank, next candidate is now at i.
	}
}
