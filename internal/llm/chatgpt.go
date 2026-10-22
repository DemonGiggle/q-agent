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
func (p *chatGPTProvider) Models(ctx context.Context) ([]string, error) {
	creds, err := chatgptoauth.GetValidTokens(ctx, p.client, p.tokensPath)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cacheKey := codexCatalogCacheKey(p.baseURL, creds.AccountID, p.tokensPath, codexClientVersion)
	if cached, ok := getCodexCatalogCache(cacheKey, now); ok && !cached.Stale {
		return append([]string(nil), cached.DisplaySlugs...), nil
	}
	cached, hasCached := getCodexCatalogCache(cacheKey, now)
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
		if hasCached {
			return append([]string(nil), cached.DisplaySlugs...), nil
		}
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if hasCached {
			return append([]string(nil), cached.DisplaySlugs...), nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		// Auth failures must surface so callers re-login; never hide them
		// behind a stale list and never let one account's list leak into
		// another scope.
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
		}
		if hasCached {
			return append([]string(nil), cached.DisplaySlugs...), nil
		}
		return nil, fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if hasCached {
			return append([]string(nil), cached.DisplaySlugs...), nil
		}
		return nil, err
	}
	entries, legacyIDs, err := decodeCodexCatalog(data)
	if err != nil {
		if hasCached {
			return append([]string(nil), cached.DisplaySlugs...), nil
		}
		return nil, fmt.Errorf("decode models: %w", err)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(p.baseURL), "/")
	fetchedAt := time.Now()
	if len(entries) > 0 {
		display := codexDisplaySlugs(entries)
		putCodexCatalogCache(cacheKey, CodexCatalogSnapshot{
			Entries:       append([]CodexModelEntry(nil), entries...),
			DisplaySlugs:  append([]string(nil), display...),
			FetchedAt:     fetchedAt,
			Source:        "live",
			Stale:         false,
			Endpoint:      endpoint,
			ClientVersion: codexClientVersion,
			Count:         len(entries),
		})
		return display, nil
	}
	// Legacy fallback: synthesize minimal entries so cache shape stays uniform.
	synth := make([]CodexModelEntry, 0, len(legacyIDs))
	for _, id := range legacyIDs {
		synth = append(synth, CodexModelEntry{Slug: id, Visibility: "list"})
	}
	display := append([]string(nil), legacyIDs...)
	putCodexCatalogCache(cacheKey, CodexCatalogSnapshot{
		Entries:       synth,
		DisplaySlugs:  append([]string(nil), display...),
		FetchedAt:     fetchedAt,
		Source:        "live-legacy",
		Stale:         false,
		Endpoint:      endpoint,
		ClientVersion: codexClientVersion,
		Count:         len(synth),
	})
	return display, nil
}

// CachedCatalog returns the last-known-good catalog snapshot for this
// provider scope without network access. The snapshot carries no credential
// material. The boolean reports cache presence; Stale reports TTL expiry.
func (p *chatGPTProvider) CachedCatalog() (CodexCatalogSnapshot, bool) {
	// Account identity is unknown without tokens; best-effort lookup scans
	// this provider's token-file scope across cached accounts. Endpoint +
	// client_version + token file still isolate custom baseURLs.
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

// codexRequestBody builds the Codex Responses payload. It mirrors
// completeResponses for tools/thinking/streaming but keeps the Codex
// contract separate so OpenCode Go behavior stays untouched.
func codexRequestBody(input Request) map[string]any {
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
	for field, value := range thinkingFields((&chatGPTProvider{}).ThinkingCapability(input.Model), input.Thinking) {
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

func (p *chatGPTProvider) doCodex(ctx context.Context, input Request, creds chatgptoauth.Tokens, onText StreamCallback) (Response, error) {
	payload, err := json.Marshal(codexRequestBody(input))
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

// Complete sends a Codex Responses request with the OAuth access token,
// refreshing once on 401 before giving up. The 401 retry reuses the
// serialized RefreshAndSaveWithStale path so a concurrent refresh is not
// spent twice on the same rotating refresh token.
func (p *chatGPTProvider) Complete(ctx context.Context, input Request, onText StreamCallback) (Response, error) {
	creds, err := chatgptoauth.GetValidTokens(ctx, p.client, p.tokensPath)
	if err != nil {
		return Response{}, err
	}
	resp, err := p.doCodex(ctx, input, creds, onText)
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
	return p.doCodex(ctx, input, fresh, onText)
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, " 401") || strings.Contains(msg, "401 Unauthorized")
}

// ThinkingCapability reports no adjustable thinking for Codex models yet.
func (p *chatGPTProvider) ThinkingCapability(model string) ThinkingCapability {
	return ThinkingCapability{Supported: false}
}

// ValidateModel accepts any non-empty model; the Codex catalog moves fast.
func (p *chatGPTProvider) ValidateModel(model string) error {
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	return nil
}
