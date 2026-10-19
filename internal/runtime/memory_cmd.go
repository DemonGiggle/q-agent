package runtime

import (
	"fmt"
	"strings"
	"time"

	"q-agent/internal/platform"
)

// memoryShow is called with r.mu held, so runtime-only storage retry state can
// be reported even when its persistence failed.
func (r *Runtime) memoryShow(m platform.Message) string {
	mem, err := r.Store.LoadMemory(m.WorkspaceKey())
	if err != nil {
		return "Unable to load long-term memory. Try /memory again later."
	}
	var b strings.Builder
	b.WriteString("**Long-term memory (all topics)**\n\n")
	if len(mem.Entries) == 0 {
		b.WriteString("No long-term memory stored for this chat yet.\n")
	}
	for _, e := range mem.Entries {
		fmt.Fprintf(&b, "- [%s] %s\n", e.Date, e.Text)
	}
	fmt.Fprintf(&b, "\nMemory `%d/%d bytes`. Pending: %d sources, %d/%d encoded bytes.", mem.Bytes(), r.Config.Memory.MaxBytes, len(mem.Pending), mem.PendingBytes(), r.Config.Memory.MaxPendingBytes)
	if len(mem.Pending) > 0 {
		fmt.Fprintf(&b, " Oldest pending age: %s.", time.Since(mem.Pending[0].At).Round(time.Second))
	}
	retry := mem.Retry
	if local, ok := r.digestRetries[m.WorkspaceKey()]; ok && (local.epoch == 0 || local.epoch == mem.Epoch) {
		retry = local.retry
	}
	if retry.Attempts == 0 && r.digestGlobalRetry.Attempts > 0 {
		retry = r.digestGlobalRetry
	}
	switch {
	case retry.Attempts > 0:
		fmt.Fprintf(&b, "\nRetry: attempt %d, next %s.", retry.Attempts, retry.NextAt.Format(time.RFC3339))
	case retry.Deferred:
		b.WriteString("\nDigest deferred until the running task or maintenance finishes.")
	case retry.Work:
		b.WriteString("\nDigest queued.")
	default:
		b.WriteString("\nRetry: none; pending sources wait for the next sweep.")
	}
	fmt.Fprintf(&b, "\nLoss since /forget: %d evicted sources (%d encoded bytes), %d truncated sources (%d encoded bytes removed), %d removed notes (%d text bytes removed or trimmed).", mem.Loss.LostEvents, mem.Loss.LostBytes, mem.Loss.TruncatedEvents, mem.Loss.TruncatedBytes, mem.Loss.DroppedNotes, mem.Loss.DroppedNoteBytes)
	if mem.LegacyBaseline {
		b.WriteString("\nMigration baseline: pre-upgrade pending history was not imported; existing summaries were retained within budget.")
	}
	return b.String()
}

func (r *Runtime) forgetMemory(m platform.Message) error {
	if err := r.Store.ClearMemory(m.WorkspaceKey()); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.digestRetries, m.WorkspaceKey())
	r.mu.Unlock()
	r.logf("telegram update=%d /forget cleared long-term notes and pending sources", m.UpdateID)
	return r.reply(m, "Long-term memory and pending sources cleared for this entire chat (all topics). History, files, schedules, and settings retained. Running tasks may add new memory sources when they finish.")
}
