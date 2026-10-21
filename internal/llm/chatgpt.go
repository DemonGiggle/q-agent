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
func (p *chatGPTProvider) Models(ctx context.Context) ([]string, error) {
	creds, err := chatgptoauth.GetValidTokens(ctx, p.client, p.tokensPath)
	if err != nil {
		return nil, err
	}
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
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Models []struct {
			Slug        string `json:"slug"`
			Visibility  string `json:"visibility"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	if len(decoded.Models) > 0 {
		models := make([]string, 0, len(decoded.Models))
		for _, m := range decoded.Models {
			if m.Slug == "" {
				continue
			}
			// Only keep entries intended for display. Empty visibility is
			// treated as listable for forward-compat with catalogs that
			// omit it.
			if m.Visibility != "" && m.Visibility != "list" {
				continue
			}
			models = append(models, m.Slug)
		}
		return models, nil
	}
	models := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
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
