package llm

import (
	"context"
	"fmt"
	"strings"

	"q-agent/internal/chatgptoauth"
)

// chatGPTBaseURL is the subscription (Codex) endpoint, not the platform
// api.openai.com API. The Responses wire format is reused.
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

// Models lists models when the endpoint supports it.
func (p *chatGPTProvider) Models(ctx context.Context) ([]string, error) {
	token, err := chatgptoauth.GetValidToken(ctx, p.client, p.tokensPath)
	if err != nil {
		return nil, err
	}
	inner := &openAIProvider{baseURL: p.baseURL, apiKey: token, client: p.client, name: "chatgpt"}
	return inner.Models(ctx)
}

// Complete sends a Responses request with the OAuth access token, refreshing
// once on 401 before giving up.
func (p *chatGPTProvider) Complete(ctx context.Context, input Request, onText StreamCallback) (Response, error) {
	token, err := chatgptoauth.GetValidToken(ctx, p.client, p.tokensPath)
	if err != nil {
		return Response{}, err
	}
	inner := &openAIProvider{baseURL: p.baseURL, apiKey: token, client: p.client, name: "chatgpt"}
	resp, err := inner.completeResponses(ctx, input, onText)
	if err == nil {
		return resp, nil
	}
	if !isUnauthorized(err) {
		return Response{}, err
	}
	token, err = chatgptoauth.RefreshAndSave(ctx, p.client, p.tokensPath)
	if err != nil {
		return Response{}, err
	}
	inner = &openAIProvider{baseURL: p.baseURL, apiKey: token, client: p.client, name: "chatgpt"}
	return inner.completeResponses(ctx, input, onText)
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
