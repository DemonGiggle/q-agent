package runtime

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"q-agent/internal/agent"
	"q-agent/internal/learning"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

// Learning tool name owned by learningTools. The MCP wrapper reserves this
// name so adapted remote tools never collide with it.
const learningRecallName = "learning_recall"

const learningRecallMaxBytes = 7 << 10

type learningTools struct {
	base    agent.Tools
	runtime *Runtime
	owner   platform.Message

	mu       sync.Mutex
	recalled bool
}

func (t *learningTools) SystemPrompt() string {
	system := agent.System
	if described, ok := t.base.(interface{ SystemPrompt() string }); ok {
		if s := described.SystemPrompt(); s != "" {
			system = s
		}
	}
	if !t.runtime.learningRecallAllowed(t.owner) {
		return system
	}
	return system + "\nUse learning_recall to inspect authorized public research notes from previous or overnight learning runs whenever the user asks what you learned, researched, or read previously, last run, or last night, or asks about prior research on a topic. Call learning_recall with empty arguments ({}) for the most recent notes, days for a recent window (1-30), or query for keyword/site:/after:/before: search. " + priorResearchInstruction
}

func (t *learningTools) Schemas() []llm.Tool {
	t.mu.Lock()
	locked := t.recalled
	t.mu.Unlock()
	if locked {
		return nil
	}
	schemas := append([]llm.Tool(nil), t.base.Schemas()...)
	if !t.runtime.learningRecallAllowed(t.owner) {
		return schemas
	}
	return append(schemas, llm.Tool{
		Name:        learningRecallName,
		Description: "Recall authorized public learning notes from previous or overnight research runs, with source URLs, dates, verified excerpts, and claim statuses. Call with {} for the most recent notes, days (1-30) for notes saved in the last N 24-hour periods, or query for keyword and site:/after:/before: search.",
		Parameters: object(map[string]any{
			"query": map[string]any{"type": "string", "description": "Optional keyword query or site:/after:YYYY-MM-DD/before:YYYY-MM-DD filter (cannot be combined with days)."},
			"days":  map[string]any{"type": "integer", "minimum": 1, "maximum": 30, "description": "Optional number of 24-hour periods to summarize, e.g. 1 for last night / last 24 hours (cannot be combined with query)."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 5, "description": "Maximum number of notes to return (default 5)."},
		}),
	})
}

func (t *learningTools) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	t.mu.Lock()
	locked := t.recalled
	t.mu.Unlock()
	if locked {
		return "", learning.ErrDenied
	}
	if call.Name != learningRecallName {
		return t.base.Execute(ctx, call)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !t.runtime.learningRecallAllowed(t.owner) {
		return "", learning.ErrDenied
	}
	var args struct {
		Query string `json:"query"`
		Days  int    `json:"days"`
		Limit int    `json:"limit"`
	}
	if len(bytes.TrimSpace(call.Arguments)) > 0 {
		if err := decodeArguments(call.Arguments, &args); err != nil {
			return "", err
		}
	}
	query := strings.TrimSpace(args.Query)
	if args.Days < 0 || args.Days > 30 {
		return "", fmt.Errorf("days must be between 1 and 30")
	}
	if args.Days > 0 && query != "" {
		return "", fmt.Errorf("provide either days or query, not both")
	}
	if args.Limit == 0 {
		args.Limit = 5
	}
	if args.Limit < 1 || args.Limit > 5 {
		return "", fmt.Errorf("limit must be between 1 and 5")
	}
	cfg := t.runtime.Learning
	allow := t.runtime.learningAllowFunc()
	repo := state.NewLearningRepository(t.runtime.Config.StateDir, nil)
	now := time.Now().UTC()
	var (
		hits []state.LearningRecall
		err  error
	)
	switch {
	case args.Days > 0:
		from := now.Add(-time.Duration(args.Days) * 24 * time.Hour)
		hits, err = repo.RecentPublicLearning(ctx, cfg, from, now, allow, args.Limit)
	case query != "":
		hits, err = repo.RecallPublic(ctx, cfg, query, allow, args.Limit)
	default:
		hits, err = repo.LatestPublicLearning(ctx, cfg, allow, args.Limit)
	}
	if err != nil {
		return "", err
	}
	var (
		body     strings.Builder
		noteIDs  []string
		claimIDs []string
	)
	remaining := learningRecallMaxBytes
	for _, hit := range hits {
		entry := hit.Markdown(cfg.SourcePolicy, now, remaining)
		if entry == "" || !allow(entry) {
			continue
		}
		body.WriteString(entry)
		remaining -= len(entry)
		noteIDs = append(noteIDs, hit.Note.ID)
		for _, claim := range hit.Claims {
			if !slices.Contains(claimIDs, claim.ID) {
				claimIDs = append(claimIDs, claim.ID)
			}
		}
	}
	if len(noteIDs) == 0 {
		if len(hits) > 0 {
			return "Matching learning notes exceed the output size limit.", nil
		}
		return "No authorized learning notes found.", nil
	}
	t.mu.Lock()
	t.recalled = true
	t.mu.Unlock()
	t.runtime.auditResearchRecall(ctx, t.owner, researchRecall{
		content:  body.String(),
		noteIDs:  noteIDs,
		claimIDs: claimIDs,
	})
	return body.String(), nil
}

func (t *learningTools) StartTask(ctx context.Context, p *redaction.Policy) {
	if scoped, ok := t.base.(interface {
		StartTask(context.Context, *redaction.Policy)
	}); ok {
		scoped.StartTask(ctx, p)
	}
}

func (t *learningTools) CloseTask() {
	if scoped, ok := t.base.(interface{ CloseTask() }); ok {
		scoped.CloseTask()
	}
}

func (t *learningTools) SetProcessHooks(hooks process.Hooks) {
	if setter, ok := t.base.(interface{ SetProcessHooks(process.Hooks) }); ok {
		setter.SetProcessHooks(hooks)
	}
}

func (t *learningTools) SetShellTimeoutHook(fn func(context.Context, string, []string) bool, maxExtends int) {
	if setter, ok := t.base.(shellHookSetter); ok {
		setter.SetShellTimeoutHook(fn, maxExtends)
	}
}

func (t *learningTools) TaskError() error {
	if scoped, ok := t.base.(interface{ TaskError() error }); ok {
		return scoped.TaskError()
	}
	return nil
}
