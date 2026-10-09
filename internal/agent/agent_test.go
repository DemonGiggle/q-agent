package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"q-agent/internal/llm"
	"q-agent/internal/redaction"
	"strings"
	"testing"
)

type mockProvider struct {
	requests []llm.Request
	fn       func(context.Context, llm.Request) (llm.Response, error)
}

func (p *mockProvider) Name() string { return "mock" }
func (p *mockProvider) Complete(ctx context.Context, r llm.Request, _ llm.StreamCallback) (llm.Response, error) {
	p.requests = append(p.requests, r)
	return p.fn(ctx, r)
}

type mockTools struct {
	calls []llm.ToolCall
	fn    func(context.Context, llm.ToolCall) (string, error)
}

func (t *mockTools) Schemas() []llm.Tool { return []llm.Tool{{Name: "read"}} }
func (t *mockTools) Execute(ctx context.Context, c llm.ToolCall) (string, error) {
	t.calls = append(t.calls, c)
	if t.fn != nil {
		return t.fn(ctx, c)
	}
	return "reference", nil
}
func TestRoundTripReasoningUsage(t *testing.T) {
	p := &mockProvider{}
	tools := &mockTools{}
	p.fn = func(_ context.Context, r llm.Request) (llm.Response, error) {
		if len(p.requests) == 1 {
			return llm.Response{Message: llm.Message{Thinking: "private", ReasoningDetails: json.RawMessage(`[{"type":"reasoning","encrypted_content":"opaque"}]`), ToolCalls: []llm.ToolCall{{Name: "read", Arguments: json.RawMessage(`{"path":"/etc/os-release"}`)}, {Name: "list", Arguments: json.RawMessage(`{}`)}}}, Usage: &llm.Usage{InputTokens: 10, OutputTokens: 3}}, nil
		}
		return llm.Response{Message: llm.Message{Content: "done"}}, nil
	}
	result, err := Run(context.Background(), p, tools, nil, "inspect", Options{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || result.ModelSteps != 2 || result.ToolCalls != 2 || result.Usage.TotalTokens != 13 || result.Usage.Missing != 1 {
		t.Fatalf("%+v", result)
	}
	messages := p.requests[1].Messages
	if len(messages) != 5 || messages[2].Thinking != "private" || len(messages[2].ReasoningDetails) == 0 {
		t.Fatalf("reasoning not replayed: %+v", messages)
	}
	for _, m := range messages[3:] {
		if m.ToolCallID == "" || !m.Untrusted || !strings.Contains(m.Content, `"trust":"untrusted"`) {
			t.Fatalf("invalid tool replay: %+v", m)
		}
	}
}
func TestRepeatedCallsCanonical(t *testing.T) {
	p := &mockProvider{}
	tools := &mockTools{}
	p.fn = func(context.Context, llm.Request) (llm.Response, error) {
		args := `{"path":"/x","extra":1}`
		if len(p.requests) == 2 {
			args = `{ "extra": 1, "path": "/x" }`
		}
		return llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{Name: "read", Arguments: json.RawMessage(args)}}}}, nil
	}
	result, err := Run(context.Background(), p, tools, nil, "loop", Options{})
	if err == nil || !strings.Contains(err.Error(), "unchanged 3 times") || len(tools.calls) != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	last := p.requests[2].Messages
	if !strings.Contains(last[len(last)-1].Content, "Recovery notice") {
		t.Fatal("missing recovery notice")
	}
}
func TestLimitsErrorsAndCancellation(t *testing.T) {
	for _, kind := range []string{"provider", "cancel", "tool-limit", "step-limit"} {
		t.Run(kind, func(t *testing.T) {
			p := &mockProvider{}
			tools := &mockTools{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.fn = func(context.Context, llm.Request) (llm.Response, error) {
				switch kind {
				case "provider":
					return llm.Response{}, fmt.Errorf("provider broke")
				case "cancel":
					cancel()
					return llm.Response{}, nil
				}
				return llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{Name: "read", Arguments: json.RawMessage(`{}`)}, {Name: "list", Arguments: json.RawMessage(`{}`)}}}}, nil
			}
			o := Options{MaxSteps: 1, MaxToolCalls: 1}
			if kind == "step-limit" {
				o.MaxToolCalls = 5
			}
			_, err := Run(ctx, p, tools, nil, "go", o)
			if err == nil {
				t.Fatal("expected error")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if len(tools.calls) > o.MaxToolCalls {
				t.Fatal("tool limit exceeded")
			}
		})
	}
}
func TestRedaction(t *testing.T) {
	secret := "unique-provider-secret"
	policy, _ := redaction.New(redaction.Config{}, []string{secret})
	p := &mockProvider{fn: func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.Message{Content: secret}}, nil
	}}
	r, err := Run(context.Background(), p, &mockTools{}, nil, "go", Options{Redaction: policy})
	if err != nil || r.Answer != redaction.Marker {
		t.Fatalf("%+v %v", r, err)
	}
}

type fatalError struct{}

func (fatalError) Error() string { return "namespace setup failed" }
func (fatalError) Fatal() bool   { return true }
func TestFatalToolErrorStopsRun(t *testing.T) {
	p := &mockProvider{fn: func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.Message{ToolCalls: []llm.ToolCall{{Name: "shell", Arguments: json.RawMessage(`{}`)}, {Name: "write", Arguments: json.RawMessage(`{}`)}}}}, nil
	}}
	tools := &mockTools{fn: func(context.Context, llm.ToolCall) (string, error) { return "", fatalError{} }}
	_, err := Run(context.Background(), p, tools, nil, "go", Options{})
	if err == nil || len(p.requests) != 1 || len(tools.calls) != 1 {
		t.Fatalf("fatal error did not stop execution: %v", err)
	}
}
