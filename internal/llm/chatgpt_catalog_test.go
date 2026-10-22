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

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Survey fixture: 7 listable slugs with ultra / non-ultra groups plus hidden
// entries to verify visibility only affects display, not decode.
func surveyFixtureCatalog() string {
	trueVal := true
	falseVal := false
	_ = trueVal
	_ = falseVal
	ultra := []any{
		map[string]any{"effort": "low", "description": "Fast"},
		map[string]any{"effort": "medium", "description": "Balanced"},
		map[string]any{"effort": "high", "description": "Deep"},
		map[string]any{"effort": "xhigh", "description": "Extra deep"},
		map[string]any{"effort": "max", "description": "Maximum"},
		map[string]any{"effort": "ultra", "description": "Experimental ultra"},
	}
	noUltra := []any{
		map[string]any{"effort": "low", "description": "Fast"},
		map[string]any{"effort": "medium", "description": "Balanced"},
		map[string]any{"effort": "high", "description": "Deep"},
		map[string]any{"effort": "xhigh", "description": "Extra deep"},
		map[string]any{"effort": "max", "description": "Maximum"},
	}
	models := []any{
		map[string]any{"slug": "gpt-6.1-sol", "visibility": "list", "display_name": "Sol", "default_reasoning_level": "low", "supported_reasoning_levels": ultra, "default_reasoning_summary": "none", "multi_agent_reasoning_effort": "xhigh"},
		map[string]any{"slug": "gpt-6-astra", "visibility": "list", "display_name": "Astra", "default_reasoning_level": "low", "supported_reasoning_levels": ultra, "default_reasoning_summary": "none"},
		map[string]any{"slug": "gpt-6-sol", "visibility": "list", "display_name": "Sol 6", "default_reasoning_level": "medium", "supported_reasoning_levels": ultra},
		map[string]any{"slug": "gpt-6-luna", "visibility": "list", "default_reasoning_level": "medium", "supported_reasoning_levels": noUltra},
		map[string]any{"slug": "gpt-5.6-sol", "visibility": "list", "default_reasoning_level": "low", "supported_reasoning_levels": ultra},
		map[string]any{"slug": "gpt-5.6-terra", "visibility": "list", "default_reasoning_level": "medium", "supported_reasoning_levels": ultra},
		map[string]any{"slug": "gpt-5.6-luna", "visibility": "list", "default_reasoning_level": "medium", "supported_reasoning_levels": noUltra},
		map[string]any{"slug": "hidden-1", "visibility": "hidden", "default_reasoning_level": "low", "supported_reasoning_levels": ultra},
		map[string]any{"slug": "", "visibility": "list", "default_reasoning_level": "low", "supported_reasoning_levels": ultra},
		map[string]any{"slug": "no-visibility", "default_reasoning_level": "low", "supported_reasoning_levels": []any{"low", "medium"}},
	}
	return mustMarshalForCatalog(models)
}

func mustMarshalForCatalog(models []any) string {
	data, _ := json.Marshal(map[string]any{"models": models})
	return string(data)
}

func TestDecodeCodexCatalogPreservesMetadata(t *testing.T) {
	clearCodexCatalogCache()
	entries, legacy, err := decodeCodexCatalog([]byte(surveyFixtureCatalog()))
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 {
		t.Fatalf("legacy = %v, want empty for models shape", legacy)
	}
	if len(entries) != 10 {
		t.Fatalf("entries = %d, want 10 (decode keeps hidden/empty)", len(entries))
	}
	bySlug := map[string]CodexModelEntry{}
	for _, e := range entries {
		if e.Slug != "" {
			bySlug[e.Slug] = e
		}
	}
	sol, ok := bySlug["gpt-6.1-sol"]
	if !ok {
		t.Fatal("missing gpt-6.1-sol")
	}
	if sol.DefaultReasoning != "low" {
		t.Fatalf("default = %q, want low", sol.DefaultReasoning)
	}
	if got := sol.Levels(); len(got) != 6 || got[5] != "ultra" {
		t.Fatalf("levels = %v, want 6 ending ultra", got)
	}
	if sol.SupportedReasoning[0].Description == "" {
		t.Fatalf("description lost: %+v", sol.SupportedReasoning[0])
	}
	if sol.MultiAgentEffort != "xhigh" {
		t.Fatalf("multi_agent = %q", sol.MultiAgentEffort)
	}
	// Missing supports_reasoning_summary_parameter must be unknown, not false.
	if _, known := sol.SupportsSummaryKnown(); known {
		t.Fatalf("summary should be unknown when missing")
	}
	// Bare-string levels form.
	entries2, _, err := decodeCodexCatalog([]byte(`{"models":[{"slug":"m","supported_reasoning_levels":["low","medium"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries2[0].Levels(); len(got) != 2 || got[0] != "low" {
		t.Fatalf("bare-string levels = %v", got)
	}
	// Explicit bool preserved.
	entries3, _, err := decodeCodexCatalog([]byte(`{"models":[{"slug":"m","supports_reasoning_summary_parameter":true},{"slug":"n","supports_reasoning_summary_parameter":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if sup, known := entries3[0].SupportsSummaryKnown(); !known || !sup {
		t.Fatalf("explicit true lost: %v %v", sup, known)
	}
	if sup, known := entries3[1].SupportsSummaryKnown(); !known || sup {
		t.Fatalf("explicit false lost: %v %v", sup, known)
	}
	// Display filtering preserves order, keeps empty visibility, drops hidden/empty.
	display := codexDisplaySlugs(entries)
	want := []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "no-visibility"}
	if len(display) != len(want) {
		t.Fatalf("display = %v, want %v", display, want)
	}
	for i := range want {
		if display[i] != want[i] {
			t.Fatalf("display = %v, want %v", display, want)
		}
	}
}

func TestModelsUsesCacheSameScope(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	calls := 0
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}]}]}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	ml := p.(ModelLister)
	first, err := ml.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := ml.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (second served from cache)", calls)
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("first=%v second=%v", first, second)
	}
	snap, ok := p.(*chatGPTProvider).CachedCatalog()
	if !ok {
		t.Fatal("expected cached snapshot")
	}
	if snap.Stale {
		t.Fatalf("fresh snapshot marked stale")
	}
	if snap.Count != 1 || snap.ClientVersion != codexClientVersion {
		t.Fatalf("snapshot = %+v", snap)
	}
	if strings.Contains(snap.Describe(), "acct-1") || strings.Contains(snap.Describe(), "Bearer") {
		t.Fatalf("diagnostics leak secrets: %q", snap.Describe())
	}
}

func TestModelsCacheIsolation(t *testing.T) {
	clearCodexCatalogCache()
	mkProvider := func(t *testing.T, account, baseURL string) (ModelLister, *int) {
		t.Helper()
		oauthPath := writeChatGPTOAuth(t, "a", "r", account, time.Now().Add(time.Hour))
		calls := new(int)
		client := doerFunc(func(request *http.Request) (*http.Response, error) {
			*calls++
			body := `{"models":[{"slug":"` + account + `-model","visibility":"list"}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		cfg := Config{ChatGPTOAuthPath: oauthPath, HTTP: client}
		if baseURL != "" {
			cfg.BaseURL = baseURL
		}
		p, err := newChatGPT(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return p.(ModelLister), calls
	}
	a, callsA := mkProvider(t, "acct-a", "")
	gotA, err := a.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, callsB := mkProvider(t, "acct-b", "")
	gotB, err := b.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *callsA != 1 || *callsB != 1 {
		t.Fatalf("cross-account must not share: %d %d", *callsA, *callsB)
	}
	if gotA[0] == gotB[0] {
		t.Fatalf("isolated accounts returned same %v %v", gotA, gotB)
	}
	c, callsC := mkProvider(t, "acct-a", "https://custom.example.test/codex")
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *callsC != 1 {
		t.Fatalf("custom endpoint must fetch, calls=%d", *callsC)
	}
	// Same account+endpoint+file hits cache tested elsewhere; different file
	// with same account still fetches (superset isolation keeps tests hermetic).
}

func TestModelsErrorDoesNotPolluteAndServesStale(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	goodBody := `{"models":[{"slug":"gpt-6.1-sol","visibility":"list"}]}`
	mode := "good"
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if mode == "good" {
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(goodBody))}, nil
		}
		return nil, io.ErrUnexpectedEOF
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	ml := p.(ModelLister)
	if _, err := ml.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	mode = "bad-transport"
	got, err := ml.Models(context.Background())
	// Fresh cache serves without network, so force expiry to exercise stale path.
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "gpt-6.1-sol" {
		t.Fatalf("fresh hit = %v", got)
	}
	// Expire manually then fail: must serve last-known-good, not empty/error.
	codexCatalogMu.Lock()
	for k, e := range codexCatalogByKey {
		e.expiresAt = time.Now().Add(-time.Minute)
		snap := e.snapshot
		snap.Stale = true
		e.snapshot = snap
		codexCatalogByKey[k] = e
	}
	codexCatalogMu.Unlock()
	got, err = ml.Models(context.Background())
	if err != nil {
		t.Fatalf("stale should be served on transport error: %v", err)
	}
	if len(got) != 1 || got[0] != "gpt-6.1-sol" {
		t.Fatalf("stale = %v", got)
	}
	snap, ok := p.(*chatGPTProvider).CachedCatalog()
	if !ok || !snap.Stale {
		t.Fatalf("expected stale snapshot, got %+v %v", snap, ok)
	}
}

func TestModelsAuthErrorDoesNotServeStale(t *testing.T) {
	clearCodexCatalogCache()
	oauthPath := writeChatGPTOAuth(t, "a", "r", "acct-1", time.Now().Add(time.Hour))
	first := true
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if first {
			first = false
			body := `{"models":[{"slug":"gpt-6.1-sol","visibility":"list"}]}`
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		return &http.Response{StatusCode: 401, Status: "401 Unauthorized", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("expired"))}, nil
	})
	p, err := newChatGPT(Config{ChatGPTOAuthPath: oauthPath, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	ml := p.(ModelLister)
	if _, err := ml.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	codexCatalogMu.Lock()
	for k, e := range codexCatalogByKey {
		e.expiresAt = time.Now().Add(-time.Minute)
		codexCatalogByKey[k] = e
	}
	codexCatalogMu.Unlock()
	if _, err := ml.Models(context.Background()); err == nil {
		t.Fatal("401 must surface, not serve stale")
	}
}
