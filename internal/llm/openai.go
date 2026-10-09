package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const openAIBaseURL = "https://api.openai.com/v1"

type openAIProvider struct {
	baseURL   string
	apiKey    string
	client    HTTPDoer
	name      string
	userAgent string
	sessionID string
	modelMeta *ProviderModelMeta
}

// SessionIdentity preserves the non-secret routing identity used by OpenCode Go.
func (p *openAIProvider) SessionIdentity() string { return p.sessionID }
func (p *openAIProvider) RestoreSessionIdentity(id string) error {
	if id == "" {
		return nil
	}
	data, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if p.name != "opencode-go" || err != nil || len(data) != 16 || len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return fmt.Errorf("invalid provider session identity")
	}
	p.sessionID = id
	return nil
}

func init() {
	Register("openai", newOpenAI)
}

func newOpenAI(config Config) (Provider, error) {
	provider, err := newOpenAICompatible(config, openAIBaseURL, "openai", "", "")
	if err != nil {
		return nil, err
	}
	provider.(*openAIProvider).modelMeta = config.ModelMeta
	return provider, nil
}

// IsOfficialOpenAIEndpoint reports whether baseURL selects OpenAI's default
// endpoint. An empty URL means the built-in endpoint will be used.
func IsOfficialOpenAIEndpoint(baseURL string) bool {
	baseURL = strings.TrimRight(baseURL, "/")
	return baseURL == "" || baseURL == openAIBaseURL
}

func newOpenAICompatible(config Config, defaultBaseURL, name, userAgent, sessionID string) (Provider, error) {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &openAIProvider{baseURL: baseURL, apiKey: config.APIKey, client: httpClient(config), name: name, userAgent: userAgent, sessionID: sessionID}, nil
}

func (p *openAIProvider) Name() string { return p.name }

func (p *openAIProvider) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}
	if p.sessionID != "" {
		req.Header.Set("x-opencode-session", p.sessionID)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
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
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode provider models: %w", err)
	}
	models := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		if model.ID != "" {
			models = append(models, model.ID)
		}
	}
	return models, nil
}

type openAITool struct {
	Type     string `json:"type"`
	Function Tool   `json:"function"`
}

type openAIToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage  `json:"reasoning_details,omitempty"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

func (p *openAIProvider) Complete(ctx context.Context, input Request, onText StreamCallback) (Response, error) {
	if p.name == "opencode-go" && openCodeGoModelRoutes[input.Model] == openCodeGoResponsesRoute {
		return p.completeResponses(ctx, input, onText)
	}
	if p.name == "opencode-go" && openCodeGoModelRoutes[input.Model] == openCodeGoMessagesRoute {
		return p.completeAnthropicMessages(ctx, input, onText)
	}
	capability := p.ThinkingCapability(input.Model)
	messages := make([]openAIMessage, 0, len(input.Messages))
	for _, message := range input.Messages {
		converted := openAIMessage{Role: message.Role, Content: openAIContent(message), Name: message.Name, ToolCallID: message.ToolCallID}
		if message.Role == "assistant" {
			switch capability.ReplayFormat {
			case ThinkingReplayReasoningContent:
				// DeepSeek-compatible models require this field on every
				// replayed assistant turn while thinking is active, including an
				// empty value for messages that did not emit visible reasoning.
				reasoning := message.Thinking
				converted.ReasoningContent = &reasoning
			case ThinkingReplayReasoningDetails:
				converted.ReasoningDetails = message.ReasoningDetails
			}
		}
		for i, call := range message.ToolCalls {
			item := openAIToolCall{Index: i, ID: call.ID, Type: "function"}
			item.Function.Name = call.Name
			item.Function.Arguments = string(call.Arguments)
			converted.ToolCalls = append(converted.ToolCalls, item)
		}
		messages = append(messages, converted)
	}
	tools := make([]openAITool, 0, len(input.Tools))
	for _, tool := range input.Tools {
		tools = append(tools, openAITool{Type: "function", Function: tool})
	}
	body := map[string]any{"stream_options": map[string]any{"include_usage": true}, "model": input.Model, "messages": messages, "stream": true}
	if p.name != "openai" || !capability.Supported {
		body["temperature"] = input.Temperature
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	for field, value := range thinkingFields(capability, input.Thinking) {
		body[field] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}
	if p.sessionID != "" {
		req.Header.Set("x-opencode-session", p.sessionID)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
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
	return parseOpenAIStream(resp.Body, onText)
}

func openAIContent(message Message) any {
	if len(message.Images) == 0 {
		return message.Content
	}
	parts := make([]map[string]any, 0, len(message.Images)+1)
	if message.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": message.Content})
	}
	for _, image := range message.Images {
		dataURL := "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL},
		})
	}
	return parts
}

func parseOpenAIStream(reader io.Reader, onText StreamCallback) (Response, error) {
	result := Message{Role: "assistant"}
	var usage *Usage
	type partialCall struct{ id, name, arguments string }
	partials := map[int]*partialCall{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event struct {
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Choices []struct {
				Delta struct {
					Content          string           `json:"content"`
					ReasoningContent string           `json:"reasoning_content"`
					Reasoning        string           `json:"reasoning"`
					ReasoningDetails json.RawMessage  `json:"reasoning_details"`
					ToolCalls        []openAIToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return Response{}, fmt.Errorf("decode provider stream: %w", err)
		}
		if event.Usage != nil {
			usage = event.Usage
		}
		if event.Error != nil {
			return Response{}, errors.New(event.Error.Message)
		}
		for _, choice := range event.Choices {
			reasoning := choice.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = choice.Delta.Reasoning
			}
			if reasoning != "" && onText != nil {
				onText(StreamEvent{Kind: StreamThinking, Text: reasoning})
			}
			result.Thinking += reasoning
			if len(choice.Delta.ReasoningDetails) > 0 {
				result.ReasoningDetails = append(json.RawMessage(nil), choice.Delta.ReasoningDetails...)
			}
			if choice.Delta.Content != "" {
				result.Content += choice.Delta.Content
				if onText != nil {
					onText(StreamEvent{Kind: StreamOutput, Text: choice.Delta.Content})
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				part := partials[delta.Index]
				if part == nil {
					part = &partialCall{}
					partials[delta.Index] = part
				}
				if delta.ID != "" {
					part.id = delta.ID
				}
				part.name += delta.Function.Name
				part.arguments += delta.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Response{}, err
	}
	for i := 0; i < len(partials); i++ {
		part := partials[i]
		if part == nil {
			continue
		}
		arguments := json.RawMessage(part.arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{ID: part.id, Name: part.name, Arguments: arguments})
	}
	return Response{Message: result, Usage: usage}, nil
}
