package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const ollamaThinkingCapability = "thinking"

type ollamaProvider struct {
	baseURL      string
	client       HTTPDoer
	capabilityMu sync.RWMutex
	capabilities map[string]ThinkingCapability
}

func init() { Register("ollama", newOllama) }

func newOllama(config Config) (Provider, error) {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	return &ollamaProvider{baseURL: baseURL, client: httpClient(config), capabilities: make(map[string]ThinkingCapability)}, nil
}

func (p *ollamaProvider) Name() string { return "ollama" }

func (p *ollamaProvider) ThinkingCapability(model string) ThinkingCapability {
	p.capabilityMu.RLock()
	capability, ok := p.capabilities[model]
	p.capabilityMu.RUnlock()
	if ok {
		capability.Levels = append([]string(nil), capability.Levels...)
		return capability
	}
	return ollamaThinkingCapabilityForModel(model, nil)
}

func (p *ollamaProvider) rememberThinkingCapability(model string, capability ThinkingCapability) {
	p.capabilityMu.Lock()
	p.capabilities[model] = capability
	p.capabilityMu.Unlock()
}

func (p *ollamaProvider) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
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
	var payload struct {
		Models []struct {
			Name         string    `json:"name"`
			Capabilities *[]string `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode ollama models: %w", err)
	}
	models := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if model.Name != "" {
			models = append(models, model.Name)
			capabilities := model.Capabilities
			if capabilities == nil {
				// Older Ollama versions omit capabilities from /api/tags.
				// Keep model listing compatible while enriching the selector when
				// /api/show is available.
				capabilities = p.showModelCapabilities(ctx, model.Name)
			}
			p.rememberThinkingCapability(model.Name, ollamaThinkingCapabilityForModel(model.Name, capabilities))
		}
	}
	return models, nil
}

func (p *ollamaProvider) showModelCapabilities(ctx context.Context, model string) *[]string {
	if err := ctx.Err(); err != nil {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/show", bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var result struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	if len(result.Capabilities) == 0 {
		return nil
	}
	return &result.Capabilities
}

func ollamaThinkingCapabilityForModel(model string, capabilities *[]string) ThinkingCapability {
	if capabilities != nil {
		found := false
		for _, capability := range *capabilities {
			if strings.EqualFold(capability, ollamaThinkingCapability) {
				found = true
				break
			}
		}
		if !found {
			return ThinkingCapability{RequestFormat: ThinkingRequestNone, ReplayFormat: ThinkingReplayNone}
		}
	} else if !ollamaLikelyThinkingModel(model) {
		return ThinkingCapability{RequestFormat: ThinkingRequestNone, ReplayFormat: ThinkingReplayNone}
	}

	levels := []string{"off", "low", "medium", "high", "max"}
	if strings.Contains(strings.ToLower(model), "gpt-oss") {
		levels = []string{"low", "medium", "high"}
	}
	return ThinkingCapability{
		Supported:     true,
		Adjustable:    true,
		Levels:        levels,
		Default:       "high",
		RequestFormat: ThinkingRequestOllamaThink,
		ReplayFormat:  ThinkingReplayNone,
	}
}

func ollamaLikelyThinkingModel(model string) bool {
	model = strings.ToLower(model)
	for _, family := range []string{"qwen3", "deepseek-r1", "deepseek-v3.1", "gpt-oss"} {
		if strings.Contains(model, family) {
			return true
		}
	}
	return false
}

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

func (p *ollamaProvider) Complete(ctx context.Context, input Request, onText StreamCallback) (Response, error) {
	messages := make([]map[string]any, 0, len(input.Messages))
	for _, message := range input.Messages {
		item := map[string]any{"role": message.Role, "content": message.Content}
		if message.Role == "tool" {
			if message.Name != "" {
				item["tool_name"] = message.Name
			}
		}
		if message.Thinking != "" {
			item["thinking"] = message.Thinking
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				var arguments any = map[string]any{}
				_ = json.Unmarshal(call.Arguments, &arguments)
				calls = append(calls, map[string]any{"function": map[string]any{"name": call.Name, "arguments": arguments}})
			}
			item["tool_calls"] = calls
		}
		if len(message.Images) > 0 {
			images := make([]string, 0, len(message.Images))
			for _, image := range message.Images {
				images = append(images, base64.StdEncoding.EncodeToString(image.Data))
			}
			item["images"] = images
		}
		messages = append(messages, item)
	}
	tools := make([]map[string]any, 0, len(input.Tools))
	for _, tool := range input.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": tool})
	}
	body := map[string]any{"model": input.Model, "messages": messages, "stream": true, "options": map[string]any{"temperature": input.Temperature}}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	for field, value := range thinkingFields(p.ThinkingCapability(input.Model), input.Thinking) {
		body[field] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Response{}, fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	result := Message{Role: "assistant"}
	var usage *Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event struct {
			InputTokens  *int          `json:"prompt_eval_count"`
			OutputTokens int           `json:"eval_count"`
			Message      ollamaMessage `json:"message"`
			Error        string        `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return Response{}, fmt.Errorf("decode provider stream: %w", err)
		}
		if event.InputTokens != nil {
			usage = &Usage{InputTokens: *event.InputTokens, OutputTokens: event.OutputTokens}
		}
		if event.Error != "" {
			return Response{}, fmt.Errorf("ollama: %s", event.Error)
		}
		if event.Message.Thinking != "" {
			result.Thinking += event.Message.Thinking
			if onText != nil {
				onText(StreamEvent{Kind: StreamThinking, Text: event.Message.Thinking})
			}
		}
		if event.Message.Content != "" {
			result.Content += event.Message.Content
			if onText != nil {
				onText(StreamEvent{Kind: StreamOutput, Text: event.Message.Content})
			}
		}
		for _, call := range event.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
	}
	if err := scanner.Err(); err != nil {
		return Response{}, err
	}
	return Response{Message: result, Usage: usage}, nil
}
