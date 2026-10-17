package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"q-agent/internal/llm"
)

func TestSteerContinuesAfterTextAnswer(t *testing.T) {
	var pending []string
	p := &mockProvider{}
	p.fn = func(_ context.Context, req llm.Request) (llm.Response, error) {
		if len(p.requests) == 1 {
			pending = []string{"use the new goal", "keep the files"}
			return llm.Response{Message: llm.Message{Content: "superseded answer"}}, nil
		}
		got := req.Messages
		if len(got) != 4 || got[2].Role != "assistant" || got[2].Content != "superseded answer" || got[3].Role != "user" || got[3].Content != "use the new goal\n\nkeep the files" {
			t.Fatalf("steering order: %+v", got)
		}
		return llm.Response{Message: llm.Message{Content: "revised answer"}}, nil
	}
	result, err := Run(context.Background(), p, &mockTools{}, nil, "original", Options{NextInput: func(bool) []string { inputs := pending; pending = nil; return inputs }})
	if err != nil || result.Answer != "revised answer" || len(result.Steers) != 1 || result.ModelSteps != 2 {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestSteerSkipsUnstartedToolsWithCompletePairing(t *testing.T) {
	for _, timing := range []string{"before tools", "during first tool"} {
		t.Run(timing, func(t *testing.T) {
			var pending []string
			tools := &mockTools{fn: func(ctx context.Context, call llm.ToolCall) (string, error) {
				if ctx.Err() != nil {
					t.Fatal("steer cancelled a running tool")
				}
				pending = []string{"stop the old actions and revise"}
				return "side effect completed", nil
			}}
			p := &mockProvider{}
			p.fn = func(_ context.Context, req llm.Request) (llm.Response, error) {
				if len(p.requests) == 1 {
					if timing == "before tools" {
						pending = []string{"stop the old actions and revise"}
					}
					return llm.Response{Message: llm.Message{Thinking: "private", ReasoningDetails: json.RawMessage(`[{"encrypted_content":"opaque"}]`), ToolCalls: []llm.ToolCall{
						{ID: "a", Name: "shell", Arguments: json.RawMessage(`{"command":"first"}`)},
						{ID: "b", Name: "write", Arguments: json.RawMessage(`{}`)},
						{ID: "c", Name: "edit", Arguments: json.RawMessage(`{}`)},
					}}}, nil
				}
				m := req.Messages
				if len(m) != 7 || m[2].Thinking != "private" || len(m[2].ReasoningDetails) == 0 {
					t.Fatalf("reasoning/order: %+v", m)
				}
				for i, id := range []string{"a", "b", "c"} {
					if m[3+i].Role != "tool" || m[3+i].ToolCallID != id {
						t.Fatalf("tool pairing: %+v", m)
					}
					if i > 0 || timing == "before tools" {
						if !strings.Contains(m[3+i].Content, "Skipped:") {
							t.Fatalf("superseded action: %+v", m[3+i])
						}
					} else if !strings.Contains(m[3+i].Content, "side effect completed") {
						t.Fatal("completed action disappeared")
					}
				}
				if m[6].Role != "user" || m[6].Content != "stop the old actions and revise" {
					t.Fatalf("missing steer: %+v", m)
				}
				return llm.Response{Message: llm.Message{Content: "revised"}}, nil
			}
			result, err := Run(context.Background(), p, tools, nil, "original", Options{NextInput: func(bool) []string { inputs := pending; pending = nil; return inputs }})
			wantCalls := 0
			if timing == "during first tool" {
				wantCalls = 1
			}
			if err != nil || result.Answer != "revised" || result.ToolCalls != wantCalls || len(tools.calls) != wantCalls {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestSteerDoesNotResetTaskLimits(t *testing.T) {
	var pending []string
	p := &mockProvider{fn: func(context.Context, llm.Request) (llm.Response, error) {
		pending = []string{"new instruction"}
		return llm.Response{Message: llm.Message{Content: "old answer"}}, nil
	}}
	result, err := Run(context.Background(), p, &mockTools{}, nil, "original", Options{MaxSteps: 1, NextInput: func(bool) []string { inputs := pending; pending = nil; return inputs }})
	if err == nil || !strings.Contains(err.Error(), "maximum of 1 model steps") || result.Answer != "" || len(p.requests) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestCancellationAtFinalSteerBoundaryDoesNotPublishSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &mockProvider{fn: func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.Message{Content: "not final"}}, nil
	}}
	result, err := Run(ctx, p, &mockTools{}, nil, "original", Options{NextInput: func(final bool) []string {
		if final {
			cancel()
		}
		return nil
	}})
	if err != context.Canceled || result.Answer != "" {
		t.Fatalf("cancelled final: %+v, %v", result, err)
	}
}

func TestSteerBatchesConsumedBeforeSameRequestAreMerged(t *testing.T) {
	var pending []string
	callbacks := 0
	p := &mockProvider{}
	p.fn = func(_ context.Context, req llm.Request) (llm.Response, error) {
		if len(p.requests) == 1 {
			pending = []string{"first correction"}
			return llm.Response{Message: llm.Message{Content: "old answer"}}, nil
		}
		if len(req.Messages) != 4 || req.Messages[3].Content != "first correction\n\nnew detail before next request" {
			t.Fatalf("unmerged unsent steer: %+v", req.Messages)
		}
		return llm.Response{Message: llm.Message{Content: "final"}}, nil
	}
	result, err := Run(context.Background(), p, &mockTools{}, nil, "original", Options{
		NextInput: func(bool) []string { inputs := pending; pending = nil; return inputs },
		OnSteer: func() {
			callbacks++
			if callbacks == 1 {
				pending = []string{"new detail before next request"}
			}
		},
	})
	if err != nil || len(result.Steers) != 1 || result.Steers[0] != "first correction\n\nnew detail before next request" {
		t.Fatalf("%+v %v", result, err)
	}
}
