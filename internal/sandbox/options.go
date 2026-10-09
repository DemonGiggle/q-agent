package sandbox

import (
	"context"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"time"
)

type Options struct {
	Bubblewrap, NewUIDMap, NewGIDMap, Worker string
	UIDBase, GIDBase                         int
	ShellTimeout                             time.Duration
}
type Runner struct {
	Options Options
	RootFS  string
}

func (r *Runner) Schemas() []llm.Tool { return localtools.Schemas() }
func (r *Runner) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	return execute(ctx, r.Options, r.RootFS, call)
}

// FatalError indicates that the execution boundary failed. The agent must stop
// the run rather than offer this failure to the model as a recoverable tool error.
type FatalError struct{ Cause error }

func (e FatalError) Error() string { return e.Cause.Error() }
func (e FatalError) Unwrap() error { return e.Cause }
func (e FatalError) Fatal() bool   { return true }
