package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// #24: no prior models list must be Unknown, not unsupported.
func TestThinkingUnknownWithoutCache(t *testing.T) {
	clearCodexCatalogCache()
	p, err := newChatGPT(Config{ChatGPTOAuthPath: writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour)), HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("no network expected for ThinkingCapability")
		return nil, io.ErrUnexpectedEOF
	})})
	if err != nil {
		t.Fatal(err)
	}
	cap := p.(ThinkingProvider).ThinkingCapability("gpt-6.1-sol")
	if cap.Supported || !cap.Unknown {
		t.Fatalf("want Unknown, got %+v", cap)
	}
	if cap.RequestFormat != ThinkingRequestCodexReasoning {
		t.Fatalf("Unknown must carry codex format, got %q", cap.RequestFormat)
	}
	// Unknown must not be reported as "cannot think" via Supported.
	if cap.Supported {
		t.Fatalf("Unknown must not set Supported")
	}
}

// #24: legacy fallback (no reasoning metadata) is Unknown, not unsupported.
func TestLegacyFallbackIsUnknown(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"data":[{"id":"legacy-model"}]}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	cap := p.(ThinkingProvider).ThinkingCapability("legacy-model")
	if cap.Supported || !cap.Unknown {
		t.Fatalf("legacy must be Unknown, got %+v", cap)
	}
}

// #24: validation rejects unsupported, allows plausible when Unknown, rejects client-only.
func TestValidateThinkingShared(t *testing.T) {
	clearCodexCatalogCache()
	known := ThinkingCapability{
		Supported: true, Adjustable: true,
		Levels:        []string{"low", "medium", "high"},
		Default:       "medium",
		RequestFormat: ThinkingRequestCodexReasoning,
		ReplayFormat:  ThinkingReplayReasoningDetails,
	}
	if err := ValidateThinkingLevel(known, "high"); err != nil {
		t.Fatalf("known high should pass: %v", err)
	}
	if err := ValidateThinkingLevel(known, "ultra"); err == nil {
		t.Fatal("ultra must be rejected even when known lists it (client-only)")
	}
	if err := ValidateThinkingLevel(known, "minimal"); err == nil {
		t.Fatal("unsupported level must be rejected for known model")
	}
	if err := ValidateThinkingLevel(known, ""); err != nil {
		t.Fatalf("auto must pass: %v", err)
	}
	unknown := unknownCodexCapability()
	for _, lv := range []string{"low", "medium", "high", "xhigh", "max", "minimal", "none"} {
		if err := ValidateThinkingLevel(unknown, lv); err != nil {
			t.Fatalf("Unknown plausible %q should pass: %v", lv, err)
		}
	}
	for _, lv := range []string{"ultra", "persistent", "custom"} {
		if err := ValidateThinkingLevel(unknown, lv); err == nil {
			t.Fatalf("Unknown client-only %q must be rejected", lv)
		}
	}
	if err := ValidateThinkingLevel(unknown, "typo-level"); err == nil {
		t.Fatal("Unknown typo must be rejected")
	}
	// Config and /think share the same set: KnownThinkingLevels is the fallback.
	found := map[string]bool{}
	for _, lv := range KnownThinkingLevels {
		found[lv] = true
	}
	for _, want := range []string{"off", "on", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !found[want] {
			t.Fatalf("KnownThinkingLevels missing %q", want)
		}
	}
	// Generic provider without per-model metadata uses the same validator.
	generic := ThinkingCapability{Supported: true, Adjustable: true, Levels: []string{"low", "high"}, RequestFormat: ThinkingRequestReasoningEffort}
	if err := ValidateThinkingLevel(generic, "high"); err != nil {
		t.Fatalf("generic high should pass: %v", err)
	}
	if err := ValidateThinkingLevel(generic, "medium"); err == nil {
		t.Fatal("generic unsupported must be rejected (no silent drop)")
	}
}

// #24: Unknown wire sends plausible optimistically, rejects client-only/typo.
func TestUnknownWireOptimistic(t *testing.T) {
	clearCodexCatalogCache()
	unknown := unknownCodexCapability()
	got := thinkingFields(unknown, "high")
	if got == nil {
		t.Fatal("Unknown high should send optimistically")
	}
	if got["reasoning"] == nil {
		t.Fatalf("want reasoning, got %v", got)
	}
	for _, lv := range []string{"ultra", "persistent", "custom", "typo", ""} {
		if out := thinkingFields(unknown, lv); out != nil {
			t.Fatalf("Unknown %q must omit, got %v", lv, out)
		}
	}
}

// #24: fresh fast path skips the refresh lock (no network, no block).
func TestRefreshFastPathSkipsLockedFile(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	calls := 0
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"models":[{"slug":"m","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("setup calls=%d", calls)
	}
	// Hold the refresh lock; fresh hit must not block.
	cacheKey := codexCatalogCacheKey(p.(*chatGPTProvider).baseURL, "acct-1", oauthPath, codexClientVersion)
	mu := codexRefreshLockFor(cacheKey)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	snap, err := p.(*chatGPTProvider).RefreshCatalog(ctx)
	if err != nil {
		t.Fatalf("fresh fast path must skip lock, got %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("fresh blocked on lock")
	}
	if len(snap.DisplaySlugs) != 1 {
		t.Fatalf("snap=%v", snap)
	}
	if calls != 1 {
		t.Fatalf("fresh must not hit network, calls=%d", calls)
	}
}

// #24: stale refresh respects context while lock is held.
func TestRefreshStaleRespectsContextOnLockedFile(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"models":[{"slug":"m","visibility":"list","supported_reasoning_levels":[{"effort":"low"}]}]}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Expire.
	codexCatalogMu.Lock()
	for k, e := range codexCatalogByKey {
		e.expiresAt = time.Now().Add(-time.Minute)
		codexCatalogByKey[k] = e
	}
	codexCatalogMu.Unlock()
	cacheKey := codexCatalogCacheKey(p.(*chatGPTProvider).baseURL, "acct-1", oauthPath, codexClientVersion)
	mu := codexRefreshLockFor(cacheKey)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = p.(*chatGPTProvider).RefreshCatalog(ctx)
	elapsed := time.Since(start)
	if err == nil || (!strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "canceled") && err != context.DeadlineExceeded && err != context.Canceled) {
		// Accept any context error wrapping.
		if ctx.Err() == nil {
			t.Fatalf("expected context error, got %v", err)
		}
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("did not respect context, blocked %v", elapsed)
	}
}

// #24: concurrent refresh shares one flight.
func TestRefreshCompetitionSingleFlight(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	var calls atomic.Int32
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/models") {
			calls.Add(1)
			time.Sleep(80 * time.Millisecond)
			body := `{"models":[{"slug":"m","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		t.Fatalf("unexpected path %s", r.URL.Path)
		return nil, io.ErrUnexpectedEOF
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = p.(*chatGPTProvider).RefreshCatalog(context.Background())
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatalf("refresh err: %v", e)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent refresh calls=%d, want 1", got)
	}
}

// #24: cross-account ThinkingCapability isolation (different files, same endpoint).
func TestThinkingCrossAccountIsolation(t *testing.T) {
	clearCodexCatalogCache()
	mk := func(account, slug string) *chatGPTProvider {
		t.Helper()
		oauthPath := writeChatGPTOAuth(t, "a", "r", account, time.Now().Add(time.Hour))
		client := doerFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"models":[{"slug":"` + slug + `","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		pp, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pp.(ModelLister).Models(context.Background()); err != nil {
			t.Fatal(err)
		}
		return pp.(*chatGPTProvider)
	}
	pa := mk("acct-a", "model-a")
	pb := mk("acct-b", "model-b")
	// Each file scope sees its own model as Known; the other's model is Unknown.
	if cap := pa.ThinkingCapability("model-a"); !cap.Supported || cap.Unknown {
		t.Fatalf("pa model-a should be Known supported, got %+v", cap)
	}
	if cap := pb.ThinkingCapability("model-b"); !cap.Supported || cap.Unknown {
		t.Fatalf("pb model-b should be Known supported, got %+v", cap)
	}
	if cap := pa.ThinkingCapability("model-b"); cap.Supported || !cap.Unknown {
		t.Fatalf("pa model-b should be Unknown (isolated), got %+v", cap)
	}
	if cap := pb.ThinkingCapability("model-a"); cap.Supported || !cap.Unknown {
		t.Fatalf("pb model-a should be Unknown (isolated), got %+v", cap)
	}
}

// #24: Complete best-effort warms catalog without failing when catalog errors.
func TestCompleteWarmsCatalogBestEffort(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	mode := "fail-catalog"
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/models") {
			if mode == "fail-catalog" {
				return nil, io.ErrUnexpectedEOF
			}
			body := `{"models":[{"slug":"m","visibility":"list","supported_reasoning_levels":[{"effort":"low"}]}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	// Catalog fails but completion must still succeed (auto omits reasoning).
	if _, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Complete should survive catalog failure: %v", err)
	}
	if cap := p.(ThinkingProvider).ThinkingCapability("m"); !cap.Unknown {
		t.Fatalf("catalog failed, should stay Unknown, got %+v", cap)
	}
	// Next Complete warms successfully.
	mode = "good"
	if _, err := p.Complete(context.Background(), Request{Model: "m", Thinking: "low", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if cap := p.(ThinkingProvider).ThinkingCapability("m"); !cap.Supported || cap.Unknown {
		t.Fatalf("after warm should be Known, got %+v", cap)
	}
}
