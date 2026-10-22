package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pure helper: auto omits, explicit sends effort only.
func TestCodexWireAutoOmitsEffort(t *testing.T) {
	clearCodexCatalogCache()
	cap := ThinkingCapability{
		Supported: true, Adjustable: true,
		Levels:        []string{"low", "medium", "high", "xhigh", "max"},
		Default:       "low",
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
	}
	got := codexRequestBodyWithCapability(cap, Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := got["reasoning"]; ok {
		t.Fatalf("auto must omit reasoning, got %v", got["reasoning"])
	}
	// Thinking empty string is auto.
	got2 := codexRequestBodyWithCapability(cap, Request{Model: "m", Thinking: "", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := got2["reasoning"]; ok {
		t.Fatalf("empty thinking must omit reasoning")
	}
}

func TestCodexWireExplicitSendsEffortWithoutSummary(t *testing.T) {
	clearCodexCatalogCache()
	cap := ThinkingCapability{
		Supported: true, Adjustable: true,
		Levels:        []string{"low", "medium", "high", "xhigh", "max"},
		Default:       "low",
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
	}
	got := codexRequestBodyWithCapability(cap, Request{Model: "m", Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}})
	raw, ok := got["reasoning"]
	if !ok {
		t.Fatalf("explicit high must send reasoning, got keys %v", keysOf(got))
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("reasoning not object: %T", raw)
	}
	if m["effort"] != "high" {
		t.Fatalf("effort = %v, want high", m["effort"])
	}
	if _, hasSummary := m["summary"]; hasSummary {
		t.Fatalf("codex must not send summary by default, got %v", m)
	}
	// Case-insensitive + trim.
	got2 := codexRequestBodyWithCapability(cap, Request{Model: "m", Thinking: " High ", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := got2["reasoning"]; !ok {
		t.Fatalf("level should be normalized")
	}
	// Unknown level omits (validation rejects elsewhere, wire never sends).
	got3 := codexRequestBodyWithCapability(cap, Request{Model: "m", Thinking: "ultra-high", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := got3["reasoning"]; ok {
		t.Fatalf("unknown level must omit, got %v", got3["reasoning"])
	}
}

func TestCodexWireRejectsUltraPersistentCustom(t *testing.T) {
	clearCodexCatalogCache()
	// Even if a custom catalog advertises them, wire refuses.
	cap := ThinkingCapability{
		Supported: true, Adjustable: true,
		Levels:        []string{"low", "medium", "high", "ultra", "persistent", "custom"},
		Default:       "low",
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
	}
	for _, lv := range []string{"ultra", "ULTRA", "persistent", "custom", " Ultra "} {
		got := codexRequestBodyWithCapability(cap, Request{Model: "m", Thinking: lv, Messages: []Message{{Role: "user", Content: "hi"}}})
		if _, ok := got["reasoning"]; ok {
			t.Fatalf("level %q must not go on wire, got %v", lv, got["reasoning"])
		}
	}
	// thinkingFields defensive check directly.
	for _, lv := range []string{"ultra", "persistent", "custom"} {
		if out := thinkingFields(cap, lv); out != nil {
			t.Fatalf("thinkingFields(%q) = %v, want nil", lv, out)
		}
	}
}

func TestCodexCapabilityFiltersClientOnly(t *testing.T) {
	clearCodexCatalogCache()
	entry := CodexModelEntry{
		Slug:               "gpt-6.1-sol",
		Visibility:         "list",
		DefaultReasoning:   "low",
		SupportedReasoning: []CodexReasoningLevelOption{{Effort: "low"}, {Effort: "medium"}, {Effort: "high"}, {Effort: "xhigh"}, {Effort: "max"}, {Effort: "ultra"}},
	}
	cap := codexCapabilityForEntry(entry)
	if !cap.Supported || !cap.Adjustable {
		t.Fatalf("cap = %+v, want supported+adjustable", cap)
	}
	for _, lv := range cap.Levels {
		if lv == "ultra" || lv == "persistent" || lv == "custom" {
			t.Fatalf("Levels must exclude %q, got %v", lv, cap.Levels)
		}
	}
	if len(cap.Levels) != 5 {
		t.Fatalf("Levels = %v, want 5 without ultra", cap.Levels)
	}
	if cap.Default != "low" {
		t.Fatalf("Default = %q, want low", cap.Default)
	}
	if cap.RequestFormat != ThinkingRequestCodexReasoning {
		t.Fatalf("RequestFormat = %q", cap.RequestFormat)
	}
	// Default that is client-only must not survive.
	entry2 := CodexModelEntry{Slug: "x", SupportedReasoning: []CodexReasoningLevelOption{{Effort: "low"}, {Effort: "ultra"}}, DefaultReasoning: "ultra"}
	cap2 := codexCapabilityForEntry(entry2)
	if cap2.Default == "ultra" {
		t.Fatalf("client-only default must be dropped, got %+v", cap2)
	}
	// No levels => unsupported (pre-#23 behavior preserved).
	empty := codexCapabilityForEntry(CodexModelEntry{Slug: "legacy"})
	if empty.Supported {
		t.Fatalf("empty entry must stay unsupported")
	}
}

func TestCodexEmptyObjectBugFixed(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	catalogJSON := `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","display_name":"Sol","default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]}]}`
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "/models") {
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(catalogJSON))}, nil
		}
		// /responses: echo ok.
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	// Before Models(): no cache => unsupported (old behavior preserved).
	if got := p.(ThinkingProvider).ThinkingCapability("gpt-6.1-sol"); got.Supported {
		t.Fatalf("pre-cache should stay unsupported, got %+v", got)
	}
	// Old buggy path always unsupported even with cache available.
	buggy := (&chatGPTProvider{}).ThinkingCapability("gpt-6.1-sol")
	if buggy.Supported {
		t.Fatalf("empty provider must stay unsupported")
	}
	// Load catalog.
	if _, err := p.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	cap := p.(ThinkingProvider).ThinkingCapability("gpt-6.1-sol")
	if !cap.Supported || !cap.Adjustable {
		t.Fatalf("loaded cap = %+v, want supported+adjustable", cap)
	}
	if len(cap.Levels) != 5 {
		t.Fatalf("loaded Levels = %v, want 5 without ultra", cap.Levels)
	}
	// Instance builder carries effort; empty-provider path would drop it.
	body := p.(*chatGPTProvider).codexRequestBody(Request{Model: "gpt-6.1-sol", Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}})
	raw, ok := body["reasoning"]
	if !ok {
		t.Fatalf("instance body must carry reasoning, keys=%v", keysOf(body))
	}
	m := raw.(map[string]any)
	if m["effort"] != "high" {
		t.Fatalf("effort = %v", m["effort"])
	}
	if _, has := m["summary"]; has {
		t.Fatalf("summary must be omitted, got %v", m)
	}
	// Buggy empty-provider body drops it.
	buggyBody := codexRequestBodyWithCapability(buggy, Request{Model: "gpt-6.1-sol", Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := buggyBody["reasoning"]; ok {
		t.Fatalf("empty capability must not send reasoning")
	}
	// Unknown model stays unsupported (unknown vs unsupported split is #24).
	if got := p.(ThinkingProvider).ThinkingCapability("nope-xyz"); got.Supported {
		t.Fatalf("unknown must stay unsupported in #23, got %+v", got)
	}
}

func TestCodexWireEndToEndNoSummary(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	var gotReasoning json.RawMessage
	var gotStore *bool
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "/models") {
			body := `{"models":[{"slug":"gpt-6-luna","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		data, _ := io.ReadAll(request.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		gotReasoning = payload["reasoning"]
		var s bool
		if err := json.Unmarshal(payload["store"], &s); err == nil {
			gotStore = &s
		}
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), Request{Model: "gpt-6-luna", Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotReasoning) == 0 {
		t.Fatalf("reasoning missing on wire")
	}
	var decoded map[string]any
	if err := json.Unmarshal(gotReasoning, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["effort"] != "high" {
		t.Fatalf("effort = %v", decoded)
	}
	if _, has := decoded["summary"]; has {
		t.Fatalf("summary must be omitted by default, got %v", decoded)
	}
	if gotStore == nil || *gotStore {
		t.Fatalf("store must stay false")
	}
	// auto omits.
	gotReasoning = nil
	_, err = p.Complete(context.Background(), Request{Model: "gpt-6-luna", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotReasoning) != 0 {
		t.Fatalf("auto must omit reasoning, got %s", gotReasoning)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
