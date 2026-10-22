package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"q-agent/internal/chatgptoauth"
)

// chatGPTBaseURL is the subscription (Codex) endpoint, not the platform
// api.openai.com API. The Responses wire format is reused with a Codex
// request contract: top-level instructions, store:false, and the
// ChatGPT-Account-Id header.
const chatGPTBaseURL = "https://chatgpt.com/backend-api/codex"

type chatGPTProvider struct {
	baseURL    string
	tokensPath string
	client     HTTPDoer
}

func init() {
	Register("chatgpt", newChatGPT)
}

func newChatGPT(config Config) (Provider, error) {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = chatGPTBaseURL
	}
	return &chatGPTProvider{baseURL: baseURL, tokensPath: config.ChatGPTOAuthPath, client: httpClient(config)}, nil
}

// Name returns the registry name.
func (p *chatGPTProvider) Name() string { return "chatgpt" }

// codexClientVersion is sent as ?client_version=... on the Codex catalog
// request, mirroring the Codex reference client (codex-rs/codex-api
// endpoint/models.rs). The server uses it for compatibility gating
// (minimal_client_version).
const codexClientVersion = "1.0.0"

// Models lists models from the Codex catalog (models[].slug). It preserves
// server ordering and only returns entries intended for display
// (visibility == "list"). A legacy Platform shape (data[].id) is accepted as
// a fallback for custom baseURLs.
//
// Catalog metadata (reasoning levels, defaults, summary support) is decoded
// by decodeCodexCatalog and retained in a scoped last-known-good cache keyed
// by endpoint + account + client_version + token file. Visibility filtering
// happens at display time so decode never discards metadata. Auth failures
// (401/403) never serve stale entries; retriable failures do.
//
// Refresh is serialized per scope with a context-aware lock so concurrent
// callers share one flight and task cancellation stays effective.
func (p *chatGPTProvider) Models(ctx context.Context) ([]string, error) {
	snap, err := p.RefreshCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), snap.DisplaySlugs...), nil
}

// RefreshCatalog ensures a fresh scoped snapshot, returning fresh or stale
// last-known-good on retriable failures. It never returns an empty list on
// retriable errors when stale exists; auth failures (401/403) always error.
//
// The lookup is account-scoped: credentials are resolved first and the exact
// cache key (endpoint + account + version + token file) is used. A pre-creds
// scan across accounts must not serve another account's fresh snapshot when
// the same token file was re-saved for a different account.
func (p *chatGPTProvider) RefreshCatalog(ctx context.Context) (CodexCatalogSnapshot, error) {
	creds, err := chatgptoauth.GetValidTokens(ctx, p.client, p.tokensPath)
	if err != nil {
		return CodexCatalogSnapshot{}, err
	}
	cacheKey := codexCatalogCacheKey(p.baseURL, creds.AccountID, p.tokensPath, codexClientVersion)
	if cached, ok := getCodexCatalogCache(cacheKey, time.Now()); ok && !cached.Stale {
		return cached, nil
	}
	cached, hasCached := getCodexCatalogCache(cacheKey, time.Now())
	mu := codexRefreshLockFor(cacheKey)
	if err := lockCodexRefreshContext(ctx, mu); err != nil {
		if hasCached {
			return cached, err
		}
		return CodexCatalogSnapshot{}, err
	}
	defer mu.Unlock()
	// Re-check after acquiring: a concurrent refresher may have won.
	if fresh, ok := getCodexCatalogCache(cacheKey, time.Now()); ok && !fresh.Stale {
		return fresh, nil
	}
	cached, hasCached = getCodexCatalogCache(cacheKey, time.Now())
	snap, fetchErr := p.fetchCatalogWithCreds(ctx, creds, cacheKey)
	if fetchErr != nil {
		if authErr, ok := fetchErr.(*codexAuthError); ok && authErr != nil {
			return CodexCatalogSnapshot{}, fetchErr
		}
		if hasCached {
			return cached, nil
		}
		return CodexCatalogSnapshot{}, fetchErr
	}
	return snap, nil
}

// codexAuthError marks 401/403 so callers never hide it behind stale.
type codexAuthError struct{ msg string }

func (e *codexAuthError) Error() string { return e.msg }

// fetchCatalogWithCreds performs one GET /models with the given credentials
// and stores the result. Retriable failures return an error; auth failures
// return *codexAuthError.
func (p *chatGPTProvider) fetchCatalogWithCreds(ctx context.Context, creds chatgptoauth.Tokens, cacheKey string) (CodexCatalogSnapshot, error) {
	catalogURL := p.baseURL + "/models"
	if u, err := url.Parse(catalogURL); err == nil {
		q := u.Query()
		q.Set("client_version", codexClientVersion)
		u.RawQuery = q.Encode()
		catalogURL = u.String()
	} else {
		catalogURL += "?client_version=" + codexClientVersion
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return CodexCatalogSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return CodexCatalogSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return CodexCatalogSnapshot{}, &codexAuthError{msg: msg.Error()}
		}
		return CodexCatalogSnapshot{}, msg
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CodexCatalogSnapshot{}, err
	}
	entries, legacyIDs, err := decodeCodexCatalog(data)
	if err != nil {
		return CodexCatalogSnapshot{}, fmt.Errorf("decode models: %w", err)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(p.baseURL), "/")
	fetchedAt := time.Now()
	if len(entries) > 0 {
		display := codexDisplaySlugs(entries)
		snap := CodexCatalogSnapshot{
			Entries:       append([]CodexModelEntry(nil), entries...),
			DisplaySlugs:  append([]string(nil), display...),
			FetchedAt:     fetchedAt,
			Source:        "live",
			Stale:         false,
			Endpoint:      endpoint,
			ClientVersion: codexClientVersion,
			Count:         len(entries),
		}
		putCodexCatalogCache(cacheKey, snap)
		return snap, nil
	}
	// Legacy fallback: synthesize minimal entries so cache shape stays uniform.
	// These carry no reasoning metadata and resolve to Unknown, not unsupported.
	synth := make([]CodexModelEntry, 0, len(legacyIDs))
	for _, id := range legacyIDs {
		synth = append(synth, CodexModelEntry{Slug: id, Visibility: "list"})
	}
	display := append([]string(nil), legacyIDs...)
	snap := CodexCatalogSnapshot{
		Entries:       synth,
		DisplaySlugs:  append([]string(nil), display...),
		FetchedAt:     fetchedAt,
		Source:        "live-legacy",
		Stale:         false,
		Endpoint:      endpoint,
		ClientVersion: codexClientVersion,
		Count:         len(synth),
	}
	putCodexCatalogCache(cacheKey, snap)
	return snap, nil
}

// codexCatalogWarmMaxWait caps optional catalog warm-up during Complete so a
// hung /models cannot exhaust the whole completion deadline. Warm-up runs on
// a derived context with a short independent budget; on timeout Complete
// proceeds with stale/Unknown using the original context (still valid) for
// /responses. This keeps "catalog errors never fail completion" true even
// for hangs, not just immediate errors.
const codexCatalogWarmMaxWait = 2 * time.Second

// codexCatalogWarmReserve is kept for the actual completion POST when the
// parent context carries a deadline.
const codexCatalogWarmReserve = 50 * time.Millisecond

// codexWarmContext derives a bounded context for optional warm-up (lock wait
// + GET /models). It returns ok=false when there is too little budget left
// to attempt warm-up; the caller should skip and proceed with stale/Unknown.
func codexWarmContext(parent context.Context) (context.Context, context.CancelFunc, bool) {
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= codexCatalogWarmReserve+10*time.Millisecond {
			return nil, nil, false
		}
		warm := remaining / 10
		if warm > codexCatalogWarmMaxWait {
			warm = codexCatalogWarmMaxWait
		}
		if maxAllowed := remaining - codexCatalogWarmReserve; warm > maxAllowed {
			warm = maxAllowed
		}
		if warm <= 0 {
			return nil, nil, false
		}
		ctx, cancel := context.WithTimeout(parent, warm)
		return ctx, cancel, true
	}
	ctx, cancel := context.WithTimeout(parent, codexCatalogWarmMaxWait)
	return ctx, cancel, true
}

// ensureCatalogWithCreds warms the cache with already-held credentials.
// Catalog errors never fail completion; callers proceed with cached/unknown.
//
// Warm-up (lock wait + GET /models) runs on a short independent budget
// derived from the parent context so a hung /models cannot consume the whole
// completion deadline. The actual /responses POST always uses the original
// context. On warm-up timeout the caller proceeds with stale/Unknown.
func (p *chatGPTProvider) ensureCatalogWithCreds(ctx context.Context, creds chatgptoauth.Tokens) {
	cacheKey := codexCatalogCacheKey(p.baseURL, creds.AccountID, p.tokensPath, codexClientVersion)
	if cached, ok := getCodexCatalogCache(cacheKey, time.Now()); ok && !cached.Stale {
		return
	}
	warmCtx, cancel, ok := codexWarmContext(ctx)
	if !ok {
		return
	}
	defer cancel()
	mu := codexRefreshLockFor(cacheKey)
	if err := lockCodexRefreshContext(warmCtx, mu); err != nil {
		return
	}
	defer mu.Unlock()
	if fresh, ok := getCodexCatalogCache(cacheKey, time.Now()); ok && !fresh.Stale {
		return
	}
	_, _ = p.fetchCatalogWithCreds(warmCtx, creds, cacheKey)
}

// CachedCatalog returns the last-known-good catalog snapshot for this
// provider scope without network access. The snapshot carries no credential
// material. The boolean reports cache presence; Stale reports TTL expiry.
//
// The lookup is account-aware without network: the current account is read
// via file-only Load (no refresh, no HTTP) and only the exact cache key
// (endpoint + account + version + token file) is served. When the account is
// known but has no cached snapshot, Unknown is returned instead of another
// account's data (same token file re-saved for a different account). Only
// when the account cannot be determined (missing/unreadable file) does it
// fall back to the legacy endpoint-scoped scan.
func (p *chatGPTProvider) CachedCatalog() (CodexCatalogSnapshot, bool) {
	if toks, err := chatgptoauth.Load(p.tokensPath); err == nil {
		if snap, ok := p.cachedCatalogExact(toks.AccountID); ok {
			return snap, true
		}
		return CodexCatalogSnapshot{}, false
	}
	// Unreadable file: best-effort legacy scan so offline diagnostics
	// can still show something; callers must treat it as best-effort.
	return p.cachedCatalogScan()
}

// cachedCatalogExact serves only the exact account scope. No cross-account
// fallback.
func (p *chatGPTProvider) cachedCatalogExact(accountID string) (CodexCatalogSnapshot, bool) {
	key := codexCatalogCacheKey(p.baseURL, accountID, p.tokensPath, codexClientVersion)
	if snap, ok := getCodexCatalogCache(key, time.Now()); ok {
		return snap, true
	}
	return CodexCatalogSnapshot{}, false
}

// cachedCatalogScan is the legacy endpoint + version + token-path scan used
// only when the current account identity is unavailable. It must never be
// used when the account is known: same-path account switches would otherwise
// serve the previous account's snapshot.
func (p *chatGPTProvider) cachedCatalogScan() (CodexCatalogSnapshot, bool) {
	codexCatalogMu.RLock()
	defer codexCatalogMu.RUnlock()
	now := time.Now()
	prefixEndpoint := strings.TrimRight(strings.TrimSpace(p.baseURL), "/") + "\x00"
	for key, entry := range codexCatalogByKey {
		if len(key) < len(prefixEndpoint) || key[:len(prefixEndpoint)] != prefixEndpoint {
			continue
		}
		// key = endpoint + \x00 + account + \x00 + clientVersion + \x00 + tokensPath
		rest := key[len(prefixEndpoint):]
		parts := strings.Split(rest, "\x00")
		if len(parts) != 3 {
			continue
		}
		if parts[1] != codexClientVersion || parts[2] != p.tokensPath {
			continue
		}
		snap := entry.snapshot
		snap.Stale = now.After(entry.expiresAt)
		return snap, true
	}
	return CodexCatalogSnapshot{}, false
}

// splitCodexMessages lifts system messages into top-level instructions. The
// Codex backend rejects system roles inside input, so they must not be sent
// there. Non-system turns keep their order and are encoded with the shared
// Responses input helper.
func splitCodexMessages(messages []Message) (string, []Message) {
	var systems []string
	rest := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if strings.TrimSpace(m.Content) != "" {
				systems = append(systems, m.Content)
			}
			continue
		}
		rest = append(rest, m)
	}
	return strings.Join(systems, "\n\n"), rest
}

// codexRequestBodyWithCapability builds the Codex Responses payload from an
// already-resolved capability. It mirrors completeResponses for
// tools/thinking/streaming but keeps the Codex contract separate so OpenCode
// Go behavior stays untouched:
//   - effort omitted means auto (server default), never a wire default
//   - summary omitted by default; summary=none is an omission, not a value
//   - ultra/persistent/custom never go on the wire (filtered from Levels and
//     refused defensively by thinkingFields)
func codexRequestBodyWithCapability(capability ThinkingCapability, input Request) map[string]any {
	instructions, rest := splitCodexMessages(input.Messages)
	body := map[string]any{
		"model":  input.Model,
		"input":  responsesInput(rest),
		"stream": true,
		"store":  false,
	}
	if strings.TrimSpace(instructions) != "" {
		body["instructions"] = instructions
	}
	for field, value := range thinkingFields(capability, input.Thinking) {
		body[field] = value
	}
	if len(input.Tools) > 0 {
		tools := make([]map[string]any, 0, len(input.Tools))
		for _, tool := range input.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.normalizedParameters()})
		}
		body["tools"] = tools
	}
	return body
}

// codexRequestBody builds the payload using this instance's cached catalog
// capability. It must not construct an empty provider: that always reports
// unsupported and silently drops explicit levels.
func (p *chatGPTProvider) codexRequestBody(input Request) map[string]any {
	return codexRequestBodyWithCapability(p.ThinkingCapability(input.Model), input)
}

// capabilityForSnapshot resolves one model against an already-loaded
// snapshot. Model absent returns Unknown with the snapshot's source/stale so
// UI can distinguish "catalog loaded but model unknown" from "no catalog".
func capabilityForSnapshot(snap CodexCatalogSnapshot, model string) ThinkingCapability {
	if entry := findCodexEntry(snap, model); entry != nil {
		cap := codexCapabilityForEntry(*entry)
		cap.Source = snap.Source
		cap.Stale = snap.Stale
		return cap
	}
	cap := unknownCodexCapability()
	cap.Source = snap.Source
	cap.Stale = snap.Stale
	return cap
}

// capabilityForCreds resolves one model against the exact account scope for
// the given credentials. No cross-account fallback: same-path account
// switches must not reuse the previous account's levels. No snapshot returns
// Unknown without source.
func (p *chatGPTProvider) capabilityForCreds(creds chatgptoauth.Tokens, model string) ThinkingCapability {
	key := codexCatalogCacheKey(p.baseURL, creds.AccountID, p.tokensPath, codexClientVersion)
	if snap, ok := getCodexCatalogCache(key, time.Now()); ok {
		return capabilityForSnapshot(snap, model)
	}
	return unknownCodexCapability()
}

func (p *chatGPTProvider) doCodexWithCapability(ctx context.Context, input Request, creds chatgptoauth.Tokens, cap ThinkingCapability, onText StreamCallback) (Response, error) {
	payload, err := json.Marshal(codexRequestBodyWithCapability(cap, input))
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Response{}, fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return parseResponsesStream(resp.Body, onText)
}

// doCodex keeps the old signature for callers without a resolved capability.
// It resolves via the account-aware cache-only lookup, so explicit levels are
// never silently built from another account's snapshot.
func (p *chatGPTProvider) doCodex(ctx context.Context, input Request, creds chatgptoauth.Tokens, onText StreamCallback) (Response, error) {
	return p.doCodexWithCapability(ctx, input, creds, p.ThinkingCapability(input.Model), onText)
}

// Complete sends a Codex Responses request with the OAuth access token,
// refreshing once on 401 before giving up. The 401 retry reuses the
// serialized RefreshAndSaveWithStale path so a concurrent refresh is not
// spent twice on the same rotating refresh token.
//
// It best-effort warms the catalog with the same credentials so request-time
// capability is not Unknown when another instance recently listed models.
// Catalog errors never fail completion.
//
// Explicit thinking is validated against the exact account snapshot before
// sending: known-unsupported levels return an error instead of being silently
// omitted. Auto (empty) always omits effort.
func (p *chatGPTProvider) Complete(ctx context.Context, input Request, onText StreamCallback) (Response, error) {
	creds, err := chatgptoauth.GetValidTokens(ctx, p.client, p.tokensPath)
	if err != nil {
		return Response{}, err
	}
	p.ensureCatalogWithCreds(ctx, creds)
	cap := p.capabilityForCreds(creds, input.Model)
	if strings.TrimSpace(input.Thinking) != "" {
		if err := ValidateCodexThinkingLevel(cap, input.Thinking); err != nil {
			return Response{}, err
		}
	}
	resp, err := p.doCodexWithCapability(ctx, input, creds, cap, onText)
	if err == nil {
		return resp, nil
	}
	if !isUnauthorized(err) {
		return Response{}, err
	}
	fresh, err := chatgptoauth.RefreshAndSaveWithStale(ctx, p.client, p.tokensPath, creds.AccessToken)
	if err != nil {
		return Response{}, err
	}
	freshCap := p.capabilityForCreds(fresh, input.Model)
	if strings.TrimSpace(input.Thinking) != "" {
		if err := ValidateCodexThinkingLevel(freshCap, input.Thinking); err != nil {
			return Response{}, err
		}
	}
	return p.doCodexWithCapability(ctx, input, fresh, freshCap, onText)
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, " 401") || strings.Contains(msg, "401 Unauthorized")
}

// ThinkingCapability resolves per-model levels from the scoped
// last-known-good catalog cache without network access.
//
// It never does network I/O so /think can call it while holding the runtime
// lock (only a file-only Load to discover the current account, no refresh).
// No cache (or model absent) returns Unknown, not unsupported; legacy
// fallback entries without reasoning metadata also return Unknown. Callers
// must not report "does not support thinking" for Unknown.
func (p *chatGPTProvider) ThinkingCapability(model string) ThinkingCapability {
	// Empty provider (zero value, only used to simulate the pre-#23 bug)
	// stays unsupported so the regression test keeps its meaning. Real
	// providers from newChatGPT always have baseURL set.
	if strings.TrimSpace(p.baseURL) == "" {
		return ThinkingCapability{Supported: false, Unknown: false}
	}
	if snap, ok := p.CachedCatalog(); ok {
		return capabilityForSnapshot(snap, model)
	}
	return unknownCodexCapability()
}

// ValidateModel accepts any non-empty model; the Codex catalog moves fast.
func (p *chatGPTProvider) ValidateModel(model string) error {
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	return nil
}
