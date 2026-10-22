package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"q-agent/internal/chatgptoauth"
)

// P2-1: same OAuth file re-saved for a different account must not serve the
// previous account's snapshot. RefreshCatalog resolves credentials first and
// uses the exact account scope; ThinkingCapability is account-aware.
func TestAccountSwitchSamePathRefreshes(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-A", time.Now().Add(time.Hour))
	var modelsCalls int
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/models") {
			modelsCalls++
			acct := r.Header.Get("ChatGPT-Account-Id")
			var body string
			switch acct {
			case "acct-A":
				body = `{"models":[{"slug":"model-a","visibility":"list","supported_reasoning_levels":[{"effort":"low"}]}]}`
			case "acct-B":
				body = `{"models":[{"slug":"model-b","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]}]}`
			default:
				body = `{"models":[]}`
			}
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	p1, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	gotA, err := p1.(ModelLister).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 1 || gotA[0] != "model-a" {
		t.Fatalf("first models = %v, want [model-a]", gotA)
	}
	if modelsCalls != 1 {
		t.Fatalf("models calls = %d, want 1", modelsCalls)
	}
	// Same path now stores account B.
	if err := chatgptoauth.Save(oauthPath, chatgptoauth.Tokens{
		AccessToken: "b", RefreshToken: "r",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct-B",
	}); err != nil {
		t.Fatal(err)
	}
	p2, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := p2.(ModelLister).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 || gotB[0] != "model-b" {
		t.Fatalf("after account switch models = %v, want [model-b] (stale A must not be served)", gotB)
	}
	if modelsCalls != 2 {
		t.Fatalf("account switch must refetch, models calls = %d, want 2", modelsCalls)
	}
	// ThinkingCapability must use B's snapshot, not A's.
	capB := p2.(ThinkingProvider).ThinkingCapability("model-b")
	if !capB.Supported {
		t.Fatalf("model-b should be supported in B scope, got %+v", capB)
	}
	capA := p2.(ThinkingProvider).ThinkingCapability("model-a")
	if capA.Supported {
		t.Fatalf("model-a must not resolve from B scope, got %+v", capA)
	}
	if !capA.Unknown {
		t.Fatalf("model-a in B scope must be Unknown, got %+v", capA)
	}
}

// P2-2: known-unsupported explicit effort must error before POST, not silently
// omit reasoning. Covers config-time typo/client-only via shared validator
// (see config package) and request-time strict check.
func TestCompleteRejectsUnsupportedExplicit(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	var responsesCalls int
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/models") {
			body := `{"models":[{"slug":"m","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}]}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		responsesCalls++
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Supported level passes and POSTs.
	responsesCalls = 0
	_, err = p.Complete(context.Background(), Request{Model: "m", Thinking: "low", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("supported low must succeed: %v", err)
	}
	if responsesCalls != 1 {
		t.Fatalf("supported must POST once, got %d", responsesCalls)
	}
	// Unsupported explicit must error without POST.
	responsesCalls = 0
	_, err = p.Complete(context.Background(), Request{Model: "m", Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err == nil {
		t.Fatal("unsupported high must error, not silently omit")
	}
	if responsesCalls != 0 {
		t.Fatalf("unsupported must not POST, got %d calls", responsesCalls)
	}
	// Client-only never sends, even when known.
	responsesCalls = 0
	_, err = p.Complete(context.Background(), Request{Model: "m", Thinking: "ultra", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err == nil {
		t.Fatal("ultra must be rejected")
	}
	if responsesCalls != 0 {
		t.Fatalf("client-only must not POST, got %d", responsesCalls)
	}
}

// P2-3: single advertised level is not always-on and explicit single effort
// must be sent. ["none"] must validate/send without claiming always-on.
func TestSingleLevelExplicitWire(t *testing.T) {
	clearCodexCatalogCache()
	singleHigh := CodexModelEntry{
		Slug:               "solo-high",
		SupportedReasoning: []CodexReasoningLevelOption{{Effort: "high"}},
	}
	cap := codexCapabilityForEntry(singleHigh)
	if cap.AlwaysOn {
		t.Fatalf("single high must not be AlwaysOn, got %+v", cap)
	}
	if cap.Default != "" {
		t.Fatalf("single without advertised default must not synthesize, got %+v", cap)
	}
	if err := ValidateCodexThinkingLevel(cap, "high"); err != nil {
		t.Fatalf("single high validate: %v", err)
	}
	if err := ValidateCodexThinkingLevel(cap, "low"); err == nil {
		t.Fatal("single high must reject low")
	}
	got := codexRequestBodyWithCapability(cap, Request{Model: "solo-high", Thinking: "high", Messages: []Message{{Role: "user", Content: "hi"}}})
	raw, ok := got["reasoning"]
	if !ok {
		t.Fatalf("single explicit high must send reasoning, got %v", got)
	}
	m := raw.(map[string]any)
	if m["effort"] != "high" {
		t.Fatalf("effort = %v, want high", m)
	}
	noneOnly := CodexModelEntry{
		Slug:               "solo-none",
		SupportedReasoning: []CodexReasoningLevelOption{{Effort: "none"}},
	}
	noneCap := codexCapabilityForEntry(noneOnly)
	if noneCap.AlwaysOn {
		t.Fatalf("none-only must not be AlwaysOn, got %+v", noneCap)
	}
	if err := ValidateCodexThinkingLevel(noneCap, "none"); err != nil {
		t.Fatalf("none-only validate none: %v", err)
	}
	gotNone := codexRequestBodyWithCapability(noneCap, Request{Model: "solo-none", Thinking: "none", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := gotNone["reasoning"]; !ok {
		t.Fatalf("none-only explicit none must send reasoning")
	}
}
