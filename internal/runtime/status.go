package runtime

import (
	"fmt"
	"sort"
	"strings"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

// statusLocked takes a runtime snapshot under mu. Only explicitly selected
// settings are exposed: never serialize Config, credentials, URLs, or paths.
func (r *Runtime) statusLocked(m platform.Message) string {
	activity := "Idle"
	if r.Maintenance != nil && r.Maintenance.Busy() {
		activity = "Maintenance (new tasks paused)"
	}
	if active, ok := r.active[m.WorkspaceKey()]; ok {
		activity = "Busy (another topic in this chat)"
		if active.topic == m.TopicID {
			activity = "Running"
			if len(active.pending) > 0 {
				activity += fmt.Sprintf(" (%d follow-up(s) pending)", len(active.pending))
			}
			if active.finishing {
				activity = "Finishing"
			}
			if active.stopping {
				activity = "Stopping"
			}
			if active.userCancelled {
				activity = "Cancelling"
			}
		}
	}
	queuedHere := 0
	for _, q := range r.queued {
		if q.WorkspaceKey() == m.WorkspaceKey() {
			queuedHere++
		}
	}
	if queuedHere > 0 {
		if activity == "Idle" {
			activity = fmt.Sprintf("Queued (%d queued in this chat)", queuedHere)
		} else {
			activity += fmt.Sprintf(" [%d queued in this chat]", queuedHere)
		}
	}
	mode := "Host"
	if r.Config.Sandbox.Enabled {
		mode = "Sandbox"
	}
	scheduler := "Stopped"
	if r.scheduleDone != nil {
		select {
		case <-r.scheduleDone:
		default:
			scheduler = "Running (minute scan)"
		}
	}
	model := "Unavailable (settings read failed)"
	provider := model
	thinking := model
	maxSteps := "Unavailable (settings read failed)"
	if selected, err := r.chatModel(m); err == nil {
		model = selected.Model
		provider = selected.Provider
		thinking = *selected.Thinking
		maxSteps = fmt.Sprint(selected.MaxSteps)
	}
	if thinking == "" {
		thinking = "Provider default"
	}
	limit := r.Config.Limits.MaxConcurrent
	if limit <= 0 {
		limit = 2
	}
	mcp := "disabled (no enabled servers)"
	if r.Config.MCPActive() {
		servers := r.Config.EnabledMCPServers()
		names := make([]string, 0, len(servers))
		for name, server := range servers {
			names = append(names, fmt.Sprintf("%s(%s)", name, server.Transport))
		}
		sort.Strings(names)
		// Sanitized by construction: only validated server names and
		// transport kinds. Commands, arguments, URLs, timeouts, and
		// environments are never exposed. Per-task availability is
		// negotiated at task start and reported in task logs.
		mcp = fmt.Sprintf("%d server(s) configured (%s); availability is negotiated per task", len(names), strings.Join(names, ", "))
	}
	// Keep config strings on one line; apply the same remote redaction policy
	// used for agent errors before the response reaches the durable outbox.
	line := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	version := line(r.Version)
	if version == "" {
		version = "dev"
	}
	activeLine := fmt.Sprintf("Active tasks: %d/%d", len(r.active), limit)
	if len(r.queued) > 0 {
		activeLine += fmt.Sprintf(" (%d queued)", len(r.queued))
	}
	text := fmt.Sprintf("**q-agent status**\n\nVersion: %s\nProvider: %s\nModel: %s\nThinking: %s\nExecution: %s\nMemory (RSS, q-agent): %s\n\nThis topic: %s\n%s\nScheduler: %s\nMCP: %s\n\n**Limits per task**\nModel steps: %s\nTask timeout: %s\nInteractive task timeout: %s\nShell timeout: %s\n\n`/help` — Available commands",
		version,
		line(provider), line(model), line(thinking), mode,
		formatRSS(currentRSS()),
		activity, activeLine, scheduler, mcp,
		maxSteps,
		line(r.Config.Limits.TaskTimeout), r.Config.InteractiveTaskDuration().String(), line(r.Config.Limits.ShellTimeout))
	text += companionStatusLine(r, m)
	return r.Options.Redaction.Text(redaction.Remote, text)
}
