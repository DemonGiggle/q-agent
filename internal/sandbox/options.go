package sandbox

import (
	"context"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
	"time"
)

type Options struct {
	Bubblewrap, NewUIDMap, NewGIDMap, Worker string
	UIDBase, GIDBase                         int
	ShellTimeout                             time.Duration
}
type Runner struct {
	sessions *process.Manager
	hooks    process.Hooks
	Options  Options
	RootFS   string
}

func (r *Runner) Schemas() []llm.Tool { return localtools.Schemas() }
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
	return execute(ctx, r.Options, r.RootFS, call)
}

// FatalError indicates that the execution boundary failed. The agent must stop
// the run rather than offer this failure to the model as a recoverable tool error.
type FatalError struct{ Cause error }

func (e FatalError) Error() string { return e.Cause.Error() }
func (e FatalError) Unwrap() error { return e.Cause }
func (e FatalError) Fatal() bool   { return true }

// StartTask binds session lifetime and redaction to one agent invocation.
func (r *Runner) StartTask(ctx context.Context, policy *redaction.Policy) {
	r.CloseTask()
	r.sessions = process.New(ctx, sessionLauncher(r.Options, r.RootFS), policy, r.hooks)
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
