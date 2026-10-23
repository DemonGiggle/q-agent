package runtime

import (
	"fmt"
	"strings"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/trust"
)

// Menu diagnostics contain only fixed status labels, never model reasoning,
// message drafts, credentials, or data from another workspace.
func (r *Runtime) companionHistoryText(m platform.Message) string {
	loc := memoryLocation(r.Config.Proactive.Timezone)
	now := time.Now().In(loc)
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nToday's companion checks · %s (%s)\nActive time: %s\n", now.Format("2006-01-02"), loc, r.Config.Proactive.ActiveTime)
	if !r.Config.Proactive.Enabled {
		b.WriteString("Global switch off: no background checks.\n")
	}
	runs, err := r.Store.CompanionRuns(m.WorkspaceKey())
	if err != nil {
		b.WriteString("History unavailable.\n")
		return b.String()
	}
	start, end, err := parseActiveWindow(r.Config.Proactive.ActiveTime)
	if err != nil {
		b.WriteString("Invalid active time.\n")
		return b.String()
	}
	var lines []string
	for _, run := range runs {
		local := run.Started.In(loc)
		if local.Format("2006-01-02") != now.Format("2006-01-02") || !proactiveInWindow(local, start, end) {
			continue
		}
		label := run.Result
		switch label {
		case "silent":
			label = "OK · stayed silent"
		case "queued":
			label = "queued · NOT confirmed delivered"
		case "delivered":
			label = "OK · delivered " + run.DeliveredAt.In(loc).Format("15:04:05")
		case "skipped":
			label = "skipped"
		case "failed":
			label = "FAILED"
		case "running":
			label = "running"
		case "interrupted":
			label = "interrupted by restart"
		}
		if run.Detail != "" {
			label += " · " + run.Detail
		}
		lines = append(lines, local.Format("15:04:05")+" — "+label)
	}
	if len(lines) == 0 {
		b.WriteString("No starts recorded in today's active time.\n")
	} else {
		if len(lines) > 20 {
			fmt.Fprintf(&b, "Showing latest 20 of %d recorded starts:\n", len(lines))
			lines = lines[len(lines)-20:]
		}
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	b.WriteString("Actual starts, not a fixed timetable. Silence is a successful check, not a failure. /companion history refreshes this view.")
	return b.String()
}

// A user may reply to the check-in later. Include confirmed outbound text as
// bounded reference context, not as a fabricated user/assistant exchange.
func (r *Runtime) companionConversationContext(m platform.Message) []llm.Message {
	if m.TopicID != 0 {
		return nil
	} // companion messages go to topic 0 only
	runs, err := r.Store.CompanionRuns(m.WorkspaceKey())
	if err != nil {
		return nil
	}
	var recent []string
	for i := len(runs) - 1; i >= 0 && len(recent) < 3; i-- {
		run := runs[i]
		if run.DeliveredAt.IsZero() || time.Since(run.DeliveredAt) > 7*24*time.Hour || run.Draft == "" {
			continue
		}
		recent = append(recent, fmt.Sprintf("[%s] %s", run.DeliveredAt.Format(time.RFC3339), truncateRunes(r.Options.Redaction.Text(redaction.Remote, run.Draft), proactiveDraftMaxRunes)))
	}
	if len(recent) == 0 {
		return nil
	}
	text := "Previous companion messages you sent to this chat (newest first; reference only, not a new request):\n" + strings.Join(recent, "\n")
	return []llm.Message{{Role: "user", Content: trust.Wrap("companion_checkins", text), Origin: "companion_checkins", Untrusted: true}}
}
