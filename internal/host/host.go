// Package host runs tools directly with the service account's permissions.
package host

import (
	"context"
	"os"
	"q-agent/internal/agent"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
)

type Runner struct {
	sessions *process.Manager
	hooks    process.Hooks
	Options  localtools.Options
}

func (r *Runner) Schemas() []llm.Tool { return localtools.HostSchemas(r.Options.WorkingDir) }
func (r *Runner) SystemPrompt() string {
	return agent.HostSystem(r.Options.WorkingDir, os.Geteuid(), os.Getegid())
}
func (r *Runner) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if r.sessions != nil {
		if err := r.sessions.Err(); err != nil {
			return "", err
		}
	}
	uses, err := localtools.UsesSessions(call)
	if err != nil {
		return "", err
	}
	if uses {
		if r.sessions == nil {
			r.StartTask(ctx, nil)
		}
		if out, handled, err := localtools.ManagedCall(ctx, call, r.sessions); handled {
			return out, err
		}
	}
	return localtools.Execute(ctx, call, r.Options)
}

// SetShellTimeoutHook installs the interactive shell-timeout decision callback
// for host shells. Sandbox runners do not implement this interface.
func (r *Runner) SetShellTimeoutHook(fn func(context.Context, string, []string) bool, maxExtends int) {
	r.Options.OnShellTimeout = fn
	r.Options.MaxShellExtends = maxExtends
}

// StartTask binds session lifetime and redaction to one agent invocation.
func (r *Runner) StartTask(ctx context.Context, policy *redaction.Policy) {
	r.CloseTask()
	r.sessions = process.New(ctx, process.HostLauncher(r.Options.WorkingDir, r.Options.Environment), policy, r.hooks)
}
func (r *Runner) CloseTask() {
	if r.sessions != nil {
		r.sessions.Close()
		r.sessions = nil
	}
}
func (r *Runner) SetProcessHooks(hooks process.Hooks) { r.hooks = hooks }

func (r *Runner) TaskError() error {
	if r.sessions != nil {
		return r.sessions.Err()
	}
	return nil
}
