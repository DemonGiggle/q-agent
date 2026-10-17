package runtime

import (
	"fmt"
	"strings"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

// statusLocked takes a runtime snapshot under mu. Only explicitly selected
// settings are exposed: never serialize Config, credentials, URLs, or paths.
func (r *Runtime) statusLocked(m platform.Message) string {
	activity := "Idle"
	if active, ok := r.active[m.WorkspaceKey()]; ok {
		activity = "Busy (another topic in this chat)"
		if active.topic == m.TopicID {
			activity = "Running"
			if active.userCancelled {
				activity = "Cancelling"
			}
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
	if selected, err := r.chatModel(m); err == nil {
		model = selected.Model
	}
	thinking := r.Config.Provider.Thinking
	if thinking == "" {
		thinking = "Provider default"
	}
	limit := r.Config.Limits.MaxConcurrent
	if limit <= 0 {
		limit = 2
	}
	// Keep config strings on one line; apply the same remote redaction policy
	// used for agent errors before the response reaches the durable outbox.
	line := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	text := fmt.Sprintf("q-agent status\n\nProvider: %s\nModel: %s\nThinking: %s\nExecution: %s\n\nThis topic: %s\nActive tasks: %d/%d\nScheduler: %s\n\nLimits per task\nModel steps: %d\nTool calls: %d\nTask timeout: %s\nShell timeout: %s\n\n/help — Available commands",
		line(r.Config.Provider.Name), line(model), line(thinking), mode,
		activity, len(r.active), limit, scheduler,
		r.Config.Limits.MaxSteps, r.Config.Limits.MaxToolCalls,
		line(r.Config.Limits.TaskTimeout), line(r.Config.Limits.ShellTimeout))
	return r.Options.Redaction.Text(redaction.Remote, text)
}
