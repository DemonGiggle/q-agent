// Package host runs tools directly with the service account's permissions.
package host

import (
	"context"
	"os"
	"q-agent/internal/agent"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
)

type Runner struct {
	Options localtools.Options
}

func (r *Runner) Schemas() []llm.Tool { return localtools.HostSchemas(r.Options.WorkingDir) }
func (r *Runner) SystemPrompt() string {
	return agent.HostSystem(r.Options.WorkingDir, os.Geteuid(), os.Getegid())
}
func (r *Runner) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	return localtools.Execute(ctx, call, r.Options)
}
