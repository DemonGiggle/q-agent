package agent

import (
	"context"
	"encoding/json"
	"testing"

	"q-agent/internal/llm"
)

func TestProgressCallbacks(t *testing.T) {
	var streamed []string
	var started []string
	var ended []string
	p := &mockProvider{
		fn: func(ctx context.Context, r llm.Request) (llm.Response, error) {
			return llm.Response{Message: llm.Message{Content: "done"}}, nil
		},
	}
	// Wrap provider to emit stream events like a real provider would.
	streaming := &streamEmitProvider{base: p, chunks: []string{"hel", "lo\n\n", "world"}}
	tools := &mockTools{
		fn: func(ctx context.Context, c llm.ToolCall) (string, error) {
			// First turn requests a tool, second returns final answer.
			return "ok", nil
		},
	}
	// Force a tool turn first: use custom provider sequence.
	calls := 0
	streaming.base.fn = func(_ context.Context, r llm.Request) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{Name: "read", Arguments: json.RawMessage(`{"path":"/x"}`)}}}}, nil
		}
		return llm.Response{Message: llm.Message{Content: "done"}}, nil
	}
	_, err := Run(context.Background(), streaming, tools, nil, "go", Options{
		OnStream:    func(ev llm.StreamEvent) { streamed = append(streamed, ev.Text) },
		OnToolStart: func(name string) { started = append(started, name) },
		OnToolEnd:   func(name string, err error) { ended = append(ended, name) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != "read" || len(ended) != 1 {
		t.Fatalf("tool callbacks: %+v %+v", started, ended)
	}
	if len(streamed) != 3 || streamed[0] != "hel" || streamed[2] != "world" {
		t.Fatalf("stream callbacks: %+v", streamed)
	}
}

type streamEmitProvider struct {
	base   *mockProvider
	chunks []string
}

func (s *streamEmitProvider) Name() string { return "mock" }
func (s *streamEmitProvider) Complete(ctx context.Context, r llm.Request, onText llm.StreamCallback) (llm.Response, error) {
	if onText != nil {
		for _, c := range s.chunks {
			// Only emit during final turn to mimic answer streaming.
			if len(r.Messages) > 3 {
				onText(llm.StreamEvent{Kind: llm.StreamOutput, Text: c})
			}
		}
	}
	return s.base.fn(ctx, r)
}
