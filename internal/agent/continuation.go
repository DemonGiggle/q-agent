package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"q-agent/internal/llm"
	"q-agent/internal/redaction"
)

// Progress is a bounded observation log, not a replay queue or opaque reasoning.
// Keep the latest tool results, marking omissions rather than claiming an exact
// snapshot. Old completed actions must never be dispatched from this log.
const MaxProgressBytes = 8 << 10
const progressOmitted = "\n[earlier tool observations omitted]\n"

func progressPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func progressClip(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	return progressPrefix(s, n-len("\n[truncated]")) + "\n[truncated]"
}

func (r *Result) recordTool(ctx context.Context, call llm.ToolCall, output string, toolErr error, policy *redaction.Policy) {
	status := "Completed (tool returned successfully)."
	if toolErr != nil {
		status = "Failed: " + toolErr.Error() + ". Side effects may already have occurred; verify before retrying."
	}
	if ctx.Err() != nil || errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded) {
		status = "Interrupted; outcome uncertain. Side effects may already have occurred; verify before retrying."
	}
	// Filter before truncation so cutting a token cannot evade redaction.
	clean := func(s string, limit int) string {
		return progressClip(policy.Text(redaction.Remote, policy.Text(redaction.Persistence, s)), limit)
	}
	note := fmt.Sprintf("Tool: %s\nArguments: %s\nOutcome: %s\nObserved output:\n%s\n", clean(call.Name, 128), clean(string(call.Arguments), 1024), clean(status, 512), clean(output, 3072))
	r.Progress += note
	if len(r.Progress) > MaxProgressBytes {
		tail := len(r.Progress) - (MaxProgressBytes - len(progressOmitted))
		for tail < len(r.Progress) && !utf8.RuneStart(r.Progress[tail]) {
			tail++
		}
		r.Progress = progressOmitted + r.Progress[tail:]
	}
}
