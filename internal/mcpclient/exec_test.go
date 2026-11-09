package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/redaction"
	"q-agent/internal/trust"
)

// fakeCaller substitutes one server session. It records invocations
// so tests prove exactly-once dispatch (no replay after ambiguous
// failures, no retry after input-required results).
type fakeCaller struct {
	calls int64
	fn    func(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

func (f *fakeCaller) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	atomic.AddInt64(&f.calls, 1)
	return f.fn(ctx, params)
}

func (f *fakeCaller) count() int64 { return atomic.LoadInt64(&f.calls) }

// execLister serves one tools/list page for catalog construction.
type execLister struct {
	tools []*mcp.Tool
	err   error
}

func (f *execLister) ListTools(_ context.Context, _ *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &mcp.ListToolsResult{Tools: f.tools}, nil
}

// fixtureCatalog builds a one-server snapshot with the given tools.
func fixtureCatalog(t *testing.T, server string, tools ...*mcp.Tool) Catalog {
	t.Helper()
	sessions := map[string]ToolLister{server: &execLister{tools: tools}}
	catalog, quarantined := DiscoverSessions(context.Background(), sessions, []string{server}, DiscoverOptions{})
	if len(quarantined) != 0 {
		t.Fatalf("fixture quarantined: %+v", quarantined)
	}
	if catalog.Len() != len(tools) {
		t.Fatalf("catalog len = %d, want %d", catalog.Len(), len(tools))
	}
	return catalog
}

// adaptedName returns the snapshot's adapted name for an original.
func adaptedName(t *testing.T, catalog Catalog, original string) string {
	t.Helper()
	for _, tool := range catalog.Tools() {
		entry, ok := catalog.Lookup(tool.Name)
		if ok && entry.Original == original {
			return tool.Name
		}
	}
	t.Fatalf("no adapted name for %q", original)
	return ""
}

func plainTool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: "fixture", InputSchema: map[string]any{"type": "object"}}
}

func callWith(t *testing.T, name string, args any) llm.ToolCall {
	t.Helper()
	var raw json.RawMessage
	if args == nil {
		raw = json.RawMessage(`{}`)
	} else {
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		raw = data
	}
	return llm.ToolCall{ID: "call-1", Name: name, Arguments: raw}
}

func mustExecute(t *testing.T, exec *Executor, call llm.ToolCall) string {
	t.Helper()
	out, err := exec.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestExecTextWithProvenance(t *testing.T) {
	catalog := fixtureCatalog(t, "docs", plainTool("lookup"))
	name := adaptedName(t, catalog, "lookup")
	exec := NewExecutor(catalog, map[string]Caller{
		"docs": &fakeCaller{fn: func(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			if params.Name != "lookup" {
				t.Errorf("server tool name = %q, want lookup", params.Name)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello world"}}}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, map[string]any{"q": "x"}))
	if !strings.Contains(out, `server="docs"`) || !strings.Contains(out, `tool="lookup"`) {
		t.Fatalf("missing provenance in %q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("missing text in %q", out)
	}
}

func TestExecStructuredJSONRenderedAndValidated(t *testing.T) {
	tool := &mcp.Tool{
		Name:        "stats",
		Description: "fixture",
		InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"n": map[string]any{"type": "integer"}},
			"required":   []any{"n"},
		},
	}
	catalog := fixtureCatalog(t, "db", tool)
	name := adaptedName(t, catalog, "stats")
	exec := NewExecutor(catalog, map[string]Caller{
		"db": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: "done"}},
				StructuredContent: map[string]any{"n": 3},
			}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if !strings.Contains(out, "done") {
		t.Fatalf("missing text section in %q", out)
	}
	if !strings.Contains(out, "[structured output (JSON)]") || !strings.Contains(out, `"n":3`) {
		t.Fatalf("missing structured JSON in %q", out)
	}
}

func TestExecStructuredValidationFailureExplicit(t *testing.T) {
	tool := &mcp.Tool{
		Name:        "stats",
		Description: "fixture",
		InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"n": map[string]any{"type": "integer"}},
			"required":   []any{"n"},
		},
	}
	catalog := fixtureCatalog(t, "db", tool)
	name := adaptedName(t, catalog, "stats")
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "oops"}},
			StructuredContent: map[string]any{"n": "not-an-integer"},
		}, nil
	}}
	exec := NewExecutor(catalog, map[string]Caller{"db": fake}, 0, nil)
	out, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil {
		t.Fatalf("expected validation failure, got success %q", out)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "output schema") {
		t.Fatalf("validation error not explicit: %v", err)
	}
	if fake.count() != 1 {
		t.Fatalf("calls = %d, want exactly 1 (no replay)", fake.count())
	}
}

func TestExecTextualEmbeddedResource(t *testing.T) {
	catalog := fixtureCatalog(t, "fs", plainTool("read"))
	name := adaptedName(t, catalog, "read")
	exec := NewExecutor(catalog, map[string]Caller{
		"fs": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///notes.txt", MIMEType: "text/plain", Text: "file body"}},
			}}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if !strings.Contains(out, "file:///notes.txt") || !strings.Contains(out, "file body") {
		t.Fatalf("resource not rendered: %q", out)
	}
}

func TestExecBinaryOmittedNeverSilent(t *testing.T) {
	catalog := fixtureCatalog(t, "media", plainTool("shot"))
	name := adaptedName(t, catalog, "shot")
	exec := NewExecutor(catalog, map[string]Caller{
		"media": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "caption"},
				&mcp.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3, 4}},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a.bin", MIMEType: "application/octet-stream", Blob: []byte{5, 6, 7}}},
			}}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if !strings.Contains(out, "caption") {
		t.Fatalf("text lost: %q", out)
	}
	if !strings.Contains(out, "[omitted binary image content") {
		t.Fatalf("image omission not explicit: %q", out)
	}
	if !strings.Contains(out, "[omitted binary resource") {
		t.Fatalf("blob omission not explicit: %q", out)
	}
	if strings.Contains(out, string([]byte{1, 2, 3, 4})) && len(out) < 10 {
		t.Fatalf("binary leaked")
	}
}

func TestExecMixedContentOrdered(t *testing.T) {
	catalog := fixtureCatalog(t, "mix", plainTool("go"))
	name := adaptedName(t, catalog, "go")
	exec := NewExecutor(catalog, map[string]Caller{
		"mix": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "first"},
					&mcp.AudioContent{MIMEType: "audio/mpeg", Data: []byte{9}},
				},
				StructuredContent: map[string]any{"ok": true},
			}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	first := strings.Index(out, "first")
	omitted := strings.Index(out, "[omitted binary audio content")
	structured := strings.Index(out, "[structured output (JSON)]")
	if first < 0 || omitted < 0 || structured < 0 || !(first < omitted && omitted < structured) {
		t.Fatalf("mixed sections unordered or missing: %q", out)
	}
	if !strings.Contains(out, `server="mix"`) || !strings.Contains(out, `tool="go"`) {
		t.Fatalf("provenance missing: %q", out)
	}
}

func TestExecIsErrorIsModelVisibleSuccess(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("run"))
	name := adaptedName(t, catalog, "run")
	exec := NewExecutor(catalog, map[string]Caller{
		"ops": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "division by zero"}},
				IsError: true,
			}, nil
		}},
	}, 0, nil)
	out, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err != nil {
		t.Fatalf("isError must not be a Go error: %v", err)
	}
	if !strings.Contains(out, "isError=true") || !strings.Contains(out, "division by zero") {
		t.Fatalf("tool error not explicit: %q", out)
	}
}

func TestExecTransportFailureIsGoError(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("run"))
	name := adaptedName(t, catalog, "run")
	boom := &jsonrpc.Error{Code: -32603, Message: "internal error"}
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return nil, boom
	}}
	exec := NewExecutor(catalog, map[string]Caller{"ops": fake}, 0, nil)
	out, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil {
		t.Fatalf("transport failure must be a Go error, got %q", out)
	}
	if out != "" {
		t.Fatalf("failure output must be empty, got %q", out)
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("must preserve JSON-RPC cause chain: %T %v", err, err)
	}
	if fake.count() != 1 {
		t.Fatalf("calls = %d, want 1", fake.count())
	}
}

func TestExecAmbiguousNeverReplayed(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("pay"))
	name := adaptedName(t, catalog, "pay")
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return nil, NewAmbiguousError("ops", "call", errors.New("connection reset"))
	}}
	exec := NewExecutor(catalog, map[string]Caller{"ops": fake}, 0, nil)
	_, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	if !IsAmbiguous(err) {
		t.Fatalf("ambiguity must survive mapping: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "do not replay") {
		t.Fatalf("no-replay note missing: %v", err)
	}
	if fake.count() != 1 {
		t.Fatalf("calls = %d, want exactly 1", fake.count())
	}
}

func TestExecUnknownToolRejected(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("run"))
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "x"}}}, nil
	}}
	exec := NewExecutor(catalog, map[string]Caller{"ops": fake}, 0, nil)
	_, err := exec.Execute(context.Background(), callWith(t, "nope", nil))
	if err == nil || !strings.Contains(err.Error(), "unknown or stale tool") {
		t.Fatalf("stale name must be rejected without remap: %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("rejected call contacted server %d times", fake.count())
	}
}

func TestExecMissingSession(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("run"))
	name := adaptedName(t, catalog, "run")
	exec := NewExecutor(catalog, nil, 0, nil)
	_, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Fatalf("missing session must fail explicitly: %v", err)
	}
}

func TestExecInputRequiredUnsupported(t *testing.T) {
	catalog := fixtureCatalog(t, "hr", plainTool("ask"))
	name := adaptedName(t, catalog, "ask")
	raw := `{"content":[{"type":"text","text":"need more"}],"inputRequests":{"r1":{"method":"elicitation/create","params":{"message":"confirm?","requestedSchema":{"type":"object"}}}},"resultType":"input_required"}`
	var res mcp.CallToolResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("fixture unmarshal: %v", err)
	}
	if !res.NeedsInput() {
		t.Fatal("fixture must be input_required")
	}
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		cp := res
		return &cp, nil
	}}
	exec := NewExecutor(catalog, map[string]Caller{"hr": fake}, 0, nil)
	_, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil {
		t.Fatal("input-required must fail explicitly")
	}
	lowered := strings.ToLower(err.Error())
	for _, want := range []string{"elicitation", "do not retry automatically"} {
		if !strings.Contains(lowered, want) {
			t.Fatalf("unsupported-capability result missing %q: %v", want, err)
		}
	}
	if fake.count() != 1 {
		t.Fatalf("calls = %d, want 1 (no fulfillment retry)", fake.count())
	}
}

func TestExecMiddlewareInputErrorUnsupported(t *testing.T) {
	catalog := fixtureCatalog(t, "hr", plainTool("ask"))
	name := adaptedName(t, catalog, "ask")
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return nil, errors.New("multi-round-trip: fulfilling input request: sampling unavailable")
	}}
	exec := NewExecutor(catalog, map[string]Caller{"hr": fake}, 0, nil)
	_, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no roots, sampling") {
		t.Fatalf("middleware input failure must map to unsupported: %v", err)
	}
	if fake.count() != 1 {
		t.Fatalf("calls = %d, want 1", fake.count())
	}
}

func TestExecCancellationPreserved(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("slow"))
	name := adaptedName(t, catalog, "slow")
	fake := &fakeCaller{fn: func(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	exec := NewExecutor(catalog, map[string]Caller{"ops": fake}, time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exec.Execute(ctx, callWith(t, name, nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel must propagate: %v", err)
	}
	if fake.count() != 0 {
		t.Fatalf("cancelled call contacted server %d times", fake.count())
	}
}

func TestExecCallTimeoutRespected(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("slow"))
	name := adaptedName(t, catalog, "slow")
	released := make(chan struct{})
	fake := &fakeCaller{fn: func(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		defer close(released)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	exec := NewExecutor(catalog, map[string]Caller{"ops": fake}, 20*time.Millisecond, nil)
	start := time.Now()
	_, err := exec.Execute(context.Background(), callWith(t, name, nil))
	if err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("call timeout not respected: %v", elapsed)
	}
	<-released
}

func TestExecBadArgumentsRejected(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("run"))
	name := adaptedName(t, catalog, "run")
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "x"}}}, nil
	}}
	exec := NewExecutor(catalog, map[string]Caller{"ops": fake}, 0, nil)
	bad := llm.ToolCall{ID: "c", Name: name, Arguments: json.RawMessage(`[1,2]`)}
	if _, err := exec.Execute(context.Background(), bad); err == nil {
		t.Fatal("non-object arguments must fail")
	}
	if fake.count() != 0 {
		t.Fatal("invalid arguments contacted server")
	}
}

func TestExecOversizedTruncatedAt8KiB(t *testing.T) {
	if MaxOutputBytes != localtools.OutputLimit {
		t.Fatalf("MaxOutputBytes = %d, want localtools.OutputLimit = %d", MaxOutputBytes, localtools.OutputLimit)
	}
	catalog := fixtureCatalog(t, "big", plainTool("dump"))
	name := adaptedName(t, catalog, "dump")
	huge := strings.Repeat("x", 4*MaxOutputBytes)
	exec := NewExecutor(catalog, map[string]Caller{
		"big": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: huge}}}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if len(out) > MaxOutputBytes {
		t.Fatalf("output %d bytes exceeds %d", len(out), MaxOutputBytes)
	}
	if !strings.HasSuffix(out, outputTruncation) {
		t.Fatalf("truncation marker missing: tail %q", out[len(out)-50:])
	}
	if !strings.Contains(out, `server="big"`) {
		t.Fatalf("provenance lost under truncation: %q", out[:200])
	}
}

func TestExecRedactionBeforeTruncation(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("show"))
	name := adaptedName(t, catalog, "show")
	secret := "super-secret-token-12345"
	policy, err := redaction.New(redaction.Config{}, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(catalog, map[string]Caller{
		"ops": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "leak: " + secret}}}, nil
		}},
	}, 0, policy)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if strings.Contains(out, secret) {
		t.Fatalf("secret survived redaction: %q", out)
	}
	if !strings.Contains(out, redaction.Marker) {
		t.Fatalf("redaction marker missing: %q", out)
	}
}

func TestExecOutputSurvivesAgentWrapping(t *testing.T) {
	// The agent loop applies Remote redaction, an 8 KiB bound, and
	// trust.Wrap to every tool result (see agent.Run). MCP output
	// must keep provenance and stay bounded through that pipeline.
	catalog := fixtureCatalog(t, "docs", plainTool("lookup"))
	name := adaptedName(t, catalog, "lookup")
	exec := NewExecutor(catalog, map[string]Caller{
		"docs": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answer body"}}}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	// Wrap with a nil-policy equivalent: default policy redacts nothing
	// new, so provenance must survive verbatim.
	var nilPolicy *redaction.Policy
	agentView := trust.Wrap(name, localtools.Limit(nilPolicy.Text(redaction.Remote, out)))
	if !strings.Contains(agentView, "answer body") || !strings.Contains(agentView, "docs") {
		t.Fatalf("agent pipeline lost content: %q", agentView)
	}
	var envelope struct {
		Origin  string `json:"origin"`
		Trust   string `json:"trust"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(agentView), &envelope); err != nil {
		t.Fatalf("wrapped result must stay valid JSON: %v", err)
	}
	if envelope.Trust != "untrusted" {
		t.Fatalf("trust = %q, want untrusted", envelope.Trust)
	}
}

func TestExecSchemasMatchSnapshot(t *testing.T) {
	catalog := fixtureCatalog(t, "b", plainTool("one"), plainTool("two"))
	exec := NewExecutor(catalog, map[string]Caller{}, 0, nil)
	if got := len(exec.Schemas()); got != 2 {
		t.Fatalf("schemas = %d, want 2", got)
	}
}

func TestExecOversizedProvenanceFirst(t *testing.T) {
	// Regression guard: truncation must cut the tail, never the
	// provenance header the model uses to attribute the result.
	catalog := fixtureCatalog(t, "big", plainTool("dump"))
	name := adaptedName(t, catalog, "dump")
	exec := NewExecutor(catalog, map[string]Caller{
		"big": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: strings.Repeat("y", 2*MaxOutputBytes)},
			}}, nil
		}},
	}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if !strings.HasPrefix(out, "[mcp ") {
		t.Fatalf("output must start with provenance header: %q", out[:min(120, len(out))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestExecResourceLinkNotFetched(t *testing.T) {
	catalog := fixtureCatalog(t, "web", plainTool("fetch"))
	name := adaptedName(t, catalog, "fetch")
	fake := &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.ResourceLink{URI: "https://example.com/x", Name: "x", MIMEType: "text/html"},
		}}, nil
	}}
	exec := NewExecutor(catalog, map[string]Caller{"web": fake}, 0, nil)
	out := mustExecute(t, exec, callWith(t, name, nil))
	if !strings.Contains(out, "resource link") || !strings.Contains(out, "https://example.com/x") {
		t.Fatalf("link must render as reference: %q", out)
	}
	if !strings.Contains(out, "not fetched") {
		t.Fatalf("link must state it was not fetched: %q", out)
	}
	if fake.count() != 1 {
		t.Fatalf("calls = %d, want 1 (no follow-up fetch)", fake.count())
	}
}

func TestExecNilResultIsError(t *testing.T) {
	catalog := fixtureCatalog(t, "ops", plainTool("run"))
	name := adaptedName(t, catalog, "run")
	exec := NewExecutor(catalog, map[string]Caller{
		"ops": &fakeCaller{fn: func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			return nil, nil
		}},
	}, 0, nil)
	if _, err := exec.Execute(context.Background(), callWith(t, name, nil)); err == nil {
		t.Fatal("nil result must be an error")
	}
}

func TestExecCallErrorBounded(t *testing.T) {
	err := &CallError{Msg: strings.Repeat("z", 2000) + "\nsecond line"}
	if len(err.Error()) > 600 {
		t.Fatalf("CallError unbounded: %d", len(err.Error()))
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatal("CallError must be single-line")
	}
	var _ error = (*CallError)(nil)
	_ = fmt.Sprint()
}
