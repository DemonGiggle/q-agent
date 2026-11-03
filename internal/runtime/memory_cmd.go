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
		meta := ""
		if e.Kind != "" && e.Kind != "fact" {
			meta += "|" + e.Kind
		}
		if e.Pinned {
			meta += "|pinned"
		}
		if e.EffectiveImportance() != 3 {
			meta += fmt.Sprintf("|imp%d", e.EffectiveImportance())
		}
		if len(e.Keys) > 0 {
			meta += "|" + strings.Join(e.Keys, ",")
		}
		if meta != "" {
			fmt.Fprintf(&b, "- [%s%s] %s\n", e.Date, meta, e.Text)
		} else {
			fmt.Fprintf(&b, "- [%s] %s\n", e.Date, e.Text)
		}
	}
	fmt.Fprintf(&b, "\nMemory `%d/%d bytes`. Pinned: %d. Pending: %d sources, %d/%d encoded bytes.", mem.Bytes(), r.Config.Memory.MaxBytes, mem.PinnedCount(), len(mem.Pending), mem.PendingBytes(), r.Config.Memory.MaxPendingBytes)
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
	r.logf("[MEMORY] Notes and pending sources cleared | update=%d", m.UpdateID)
	return r.reply(m, "Long-term memory and pending sources cleared for this entire chat (all topics). History, files, schedules, and settings retained. Running tasks may add new memory sources when they finish.")
}

// memoryCommand routes /memory, /remember, and /forget (bare or keyword).
// It is called before the chat lock is taken, like chatSettings/companion.
func (r *Runtime) memoryCommand(m platform.Message, fields []string) error {
	if len(fields) == 0 {
		return r.reply(m, "Use /memory, /remember <note>, or /forget [keyword].")
	}
	switch fields[0] {
	case "/memory":
		r.mu.Lock()
		text := r.memoryShow(m)
		r.mu.Unlock()
		return r.reply(m, text)
	case "/remember":
		note := strings.TrimSpace(strings.TrimPrefix(m.Text, "/remember"))
		if note == "" {
			return r.reply(m, "Usage: /remember <one lasting fact, e.g. I prefer concise answers>. It is pinned so compaction never drops it.")
		}
		if len(note) > 2000 {
			note = note[:2000]
		}
		if _, err := r.Store.RememberPinned(m.WorkspaceKey(), time.Now().In(memoryLocation(r.Config.Memory.Timezone)).Format("2006-01-02"), note, r.Config.Memory.MaxBytes); err != nil {
			return r.reply(m, "Unable to save that note. Try /remember again later.")
		}
		r.logf("[MEMORY] Pinned note remembered | update=%d", m.UpdateID)
		return r.reply(m, "Remembered and pinned. It will survive compaction and appear in /memory.")
	case "/forget":
		keyword := strings.TrimSpace(strings.TrimPrefix(m.Text, "/forget"))
		if keyword == "" {
			return r.forgetMemory(m)
		}
		removed, err := r.Store.ForgetByKeyword(m.WorkspaceKey(), keyword)
		if err != nil {
			return r.reply(m, "Unable to search memory. Try /forget again later.")
		}
		if removed == 0 {
			return r.reply(m, fmt.Sprintf("No notes matched %q. Nothing deleted. Use /memory to list notes, or /forget alone to clear everything.", keyword))
		}
		r.mu.Lock()
		delete(r.digestRetries, m.WorkspaceKey())
		r.mu.Unlock()
		r.logf("[MEMORY] Notes removed by keyword | update=%d removed=%d", m.UpdateID, removed)
		return r.reply(m, fmt.Sprintf("Forgot %d note(s) matching %q. History, files, schedules, and settings retained.", removed, keyword))
	default:
		return r.reply(m, "Use /memory, /remember <note>, or /forget [keyword].")
	}
}
