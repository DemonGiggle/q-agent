package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"q-agent/internal/llm"
	"q-agent/internal/redaction"
)

func TestFailureProgressContainsOnlyExecutedToolObservations(t *testing.T) {
	p := &mockProvider{fn: func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.Message{Thinking: "PRIVATE_THINKING", ReasoningDetails: json.RawMessage(`{"private":"OPAQUE_REASONING"}`), ToolCalls: []llm.ToolCall{{Name: "shell", Arguments: json.RawMessage(`{"command":"first"}`)}, {Name: "shell", Arguments: json.RawMessage(`{"command":"NOT_EXECUTED"}`)}}}}, nil
	}}
	tools := &mockTools{fn: func(context.Context, llm.ToolCall) (string, error) { return "confirmed side effect", fatalError{} }}
	result, err := Run(context.Background(), p, tools, nil, "original", Options{MaxSteps: 2})
	if err == nil || len(tools.calls) != 1 {
		t.Fatal("expected fatal tool failure")
	}
	for _, want := range []string{"first", "confirmed side effect", "Failed"} {
		if !strings.Contains(result.Progress, want) {
			t.Fatalf("missing %q: %s", want, result.Progress)
		}
	}
	for _, absent := range []string{"NOT_EXECUTED", "PRIVATE_THINKING", "OPAQUE_REASONING"} {
		if strings.Contains(result.Progress, absent) {
			t.Fatalf("unexpected %q", absent)
		}
	}
}

func TestFailureProgressBoundRedactionAndUncertainOutcome(t *testing.T) {
	policy, err := redaction.New(redaction.Config{}, []string{"my-private-credential"})
	if err != nil {
		t.Fatal(err)
	}
	result := Result{}
	for i := 0; i < 30; i++ {
		result.recordTool(context.Background(), llm.ToolCall{Name: "shell", Arguments: json.RawMessage(`{"command":"my-private-credential"}`)}, strings.Repeat("中文", 10000)+" my-private-credential", nil, policy)
	}
	if len(result.Progress) > MaxProgressBytes || !utf8.ValidString(result.Progress) || !strings.Contains(result.Progress, "omitted") || strings.Contains(result.Progress, "my-private-credential") || !strings.Contains(result.Progress, redaction.Marker) {
		t.Fatalf("unsafe progress: %d bytes", len(result.Progress))
	}
	result = Result{}
	result.recordTool(context.Background(), llm.ToolCall{Name: "shell"}, "partial output", errors.Join(errors.New("wrapped"), context.DeadlineExceeded), policy)
	if !strings.Contains(result.Progress, "outcome uncertain") || !strings.Contains(result.Progress, "partial output") {
		t.Fatal(result.Progress)
	}
}
