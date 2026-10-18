package runtime

import (
	"fmt"
	"strings"

	"q-agent/internal/platform"
)

// memoryShow renders this chat's workspace-scoped long-term memory with its
// budget usage. Read-only: it never calls the model or changes history.
func (r *Runtime) memoryShow(m platform.Message) string {
	cap := r.Config.Memory.MaxBytes
	mem, err := r.Store.LoadMemory(m.WorkspaceKey())
	if err != nil {
		return "Unable to load long-term memory. Try /memory again later."
	}
	if len(mem.Entries) == 0 {
		return "No long-term memory stored for this chat yet. The nightly digest adds notes here."
	}
	var b strings.Builder
	b.WriteString("**Long-term memory**\n\n")
	for _, e := range mem.Entries {
		b.WriteString("- [")
		b.WriteString(e.Date)
		b.WriteString("] ")
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nMemory `%d/%d bytes`.", mem.Bytes(), cap)
	return b.String()
}

// forgetMemory clears this chat's long-term memory only. Workspace files,
// conversation history, schedules, and settings are retained.
func (r *Runtime) forgetMemory(m platform.Message) error {
	if err := r.Store.ClearMemory(m.WorkspaceKey()); err != nil {
		return err
	}
	r.logf("telegram update=%d /forget cleared long-term memory", m.UpdateID)
	return r.reply(m, "Long-term memory cleared. The next nightly digest starts fresh.")
}
