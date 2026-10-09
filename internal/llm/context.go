package llm

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Context limits from https://models.dev/api.json, retrieved 2026-09-06.
// Exact IDs only: unknown models must not inherit a possibly incorrect limit.
//
//go:embed context_windows.json
var contextCatalog []byte

func (p *openAIProvider) ContextWindow(_ context.Context, model string) (int, error) {
	// A custom endpoint may impose different limits even for familiar model IDs.
	if (p.name == "openai" && p.baseURL != "https://api.openai.com/v1") ||
		(p.name == "opencode-go" && p.baseURL != openCodeGoBaseURL) {
		return 0, nil
	}
	var catalog map[string]map[string]int
	if err := json.Unmarshal(contextCatalog, &catalog); err != nil {
		return 0, err
	}
	return catalog[p.name][model], nil
}

func (p *ollamaProvider) ContextWindow(ctx context.Context, model string) (int, error) {
	// /api/ps reports the allocated window, unlike /api/show's training maximum.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/ps", nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("context discovery: %s", resp.Status)
	}
	var payload struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	normalize := func(s string) string {
		if !strings.Contains(s, ":") {
			return s + ":latest"
		}
		return s
	}
	for _, item := range payload.Models {
		if normalize(item.Name) == normalize(model) || normalize(item.Model) == normalize(model) {
			return item.ContextLength, nil
		}
	}
	return 0, nil
}
