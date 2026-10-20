package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

// shellDecision is a pending shell-timeout question for one workspace.
type shellDecision struct {
	ch      chan bool
	owner   platform.Message
	command string
}

// shellHookSetter is implemented by host runners. Sandbox runners do not
// implement it and keep the old terminate-on-timeout behavior.
type shellHookSetter interface {
	SetShellTimeoutHook(fn func(context.Context, string, []string) bool, maxExtends int)
}

func shellDecisionText(timeout time.Duration, lastLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Shell timed out after %s but is still producing output. Last %d line(s):\n", timeout.Truncate(time.Second), len(lastLines))
	for _, line := range lastLines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nReply `extend` to continue for another %s, or `kill` to stop it.", timeout.Truncate(time.Second))
	return b.String()
}

func parseShellDecision(text string) (bool, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	switch t {
	case "extend", "yes", "y", "continue", "keep going", "extend shell", "continue shell":
		return true, true
	case "kill", "terminate", "stop", "no", "n", "kill shell", "stop shell":
		return false, true
	}
	return false, false
}

// requestShellExtend asks the task owner whether a timed-out shell should
// continue. Scheduled runs (runID != "") have no interactive user and are
// terminated. It returns true to extend for another ShellTimeout window.
func (r *Runtime) requestShellExtend(ctx context.Context, owner platform.Message, runID, command string, lastLines []string) bool {
	if runID != "" {
		return false
	}
	key := owner.WorkspaceKey()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	if r.shellPrompts == nil {
		r.shellPrompts = map[string]*shellDecision{}
	}
	if old, ok := r.shellPrompts[key]; ok {
		close(old.ch)
		delete(r.shellPrompts, key)
	}
	wait := r.Config.ShellPromptWait()
	r.mu.Unlock()

	prompt := shellDecisionText(r.Config.ShellDuration(), lastLines)
	prompt = r.Options.Redaction.Text(redaction.Remote, prompt)
	// Queue first so observers seeing shellPrompts always find the prompt
	// already in the outbox (reply formatting must not widen the race).
	if err := r.reply(owner, prompt); err != nil {
		r.logf("[SHELL] Timeout prompt queueing failed | workspace=%s error=%q", key, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		return false
	}
	dec := &shellDecision{ch: make(chan bool, 1), owner: owner, command: command}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	if old, ok := r.shellPrompts[key]; ok {
		close(old.ch)
		delete(r.shellPrompts, key)
	}
	r.shellPrompts[key] = dec
	r.mu.Unlock()
	r.logf("[SHELL] Timeout reached; awaiting extension decision | workspace=%s wait=%s", key, wait.Truncate(time.Second))

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case extend := <-dec.ch:
		return extend
	case <-ctx.Done():
		r.mu.Lock()
		if cur, ok := r.shellPrompts[key]; ok && cur == dec {
			delete(r.shellPrompts, key)
		}
		r.mu.Unlock()
		return false
	case <-timer.C:
		r.mu.Lock()
		if cur, ok := r.shellPrompts[key]; ok && cur == dec {
			delete(r.shellPrompts, key)
		}
		r.mu.Unlock()
		r.logf("[SHELL] Extension decision timed out; terminating shell | workspace=%s duration=%s", key, wait.Truncate(time.Second))
		return false
	}
}
