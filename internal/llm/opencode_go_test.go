package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestOpenCodeGoUsesDedicatedEndpointAndIdentity(t *testing.T) {
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.URL.String(); got != openCodeGoBaseURL+"/chat/completions" {
			t.Errorf("URL = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer go-secret" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("User-Agent"); got != "q-agent" {
			t.Errorf("user agent = %q", got)
		}
		if got := request.Header.Get("x-opencode-session"); !uuidV4Pattern.MatchString(got) {
			t.Errorf("x-opencode-session = %q, want UUIDv4", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	provider, err := newOpenCodeGo(Config{APIKey: "go-secret", HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "opencode-go" {
		t.Fatalf("name = %q", provider.Name())
	}
	if _, err := provider.Complete(context.Background(), Request{Model: "kimi-k3"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeGoThinkingUsesOnlyModelValidEncoding(t *testing.T) {
	tests := []struct {
		name, model, level, effort, thinking string
	}{
		{"deepseek effort", "deepseek-v4-flash", "max", "max", ""},
		{"deepseek v4.1 effort", "deepseek-v4.1-flash", "low", "low", ""},
		{"hy no-think", "hy4-preview", "none", "none", ""},
		{"deepseek off", "deepseek-v4-flash", "off", "", "disabled"},
		{"glm toggle", "glm-5", "on", "", "enabled"},
		{"unknown omits", "future-model", "max", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := doerFunc(func(request *http.Request) (*http.Response, error) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				var effort string
				_ = json.Unmarshal(body["reasoning_effort"], &effort)
				if effort != test.effort {
					t.Errorf("reasoning_effort = %q, want %q", effort, test.effort)
				}
				var thinking struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(body["thinking"], &thinking)
				if thinking.Type != test.thinking {
					t.Errorf("thinking.type = %q, want %q", thinking.Type, test.thinking)
				}
				if test.effort != "" && test.thinking != "" {
					t.Fatal("test setup permits mutually exclusive fields")
				}
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
			})
			provider, err := newOpenCodeGo(Config{HTTP: client})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Complete(context.Background(), Request{Model: test.model, Thinking: test.level}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenCodeGoReplaysReasoningContentOnToolContinuation(t *testing.T) {
	requests := 0
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			var effort string
			_ = json.Unmarshal(body["reasoning_effort"], &effort)
			if effort != "high" {
				t.Errorf("reasoning_effort = %q, want high", effort)
			}
		}
		if requests == 2 {
			var body struct {
				Messages []struct {
					Role             string          `json:"role"`
					Content          json.RawMessage `json:"content"`
					ReasoningContent *string         `json:"reasoning_content"`
					ToolCalls        []any           `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Messages) != 1 || body.Messages[0].Role != "assistant" || body.Messages[0].ReasoningContent == nil || *body.Messages[0].ReasoningContent != "check first" {
				t.Fatalf("replayed messages = %#v", body.Messages)
			}
			if string(body.Messages[0].Content) != `""` {
				t.Fatalf("replayed empty content = %s, want an explicit empty string", body.Messages[0].Content)
			}
		}
		stream := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"check first\"}}]}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	provider, err := newOpenCodeGo(Config{
		HTTP: client,
		ModelMeta: &ProviderModelMeta{
			Version:  ProviderModelMetaVersion,
			Provider: "opencode-go",
			Models:   map[string]ProviderModelOptions{"deepseek-v4-flash": {Effort: []string{"low", "high"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Complete(context.Background(), Request{Model: "deepseek-v4-flash", Thinking: "high"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Message.Thinking != "check first" {
		t.Fatalf("thinking = %q", first.Message.Thinking)
	}
	if _, err := provider.Complete(context.Background(), Request{Model: "deepseek-v4-flash", Thinking: "high", Messages: []Message{first.Message}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeGoReusesSessionIDForAllRequests(t *testing.T) {
	var sessionID string
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		got := request.Header.Get("x-opencode-session")
		if !uuidV4Pattern.MatchString(got) {
			t.Errorf("x-opencode-session = %q, want UUIDv4", got)
		}
		if sessionID == "" {
			sessionID = got
		} else if got != sessionID {
			t.Errorf("x-opencode-session = %q, want %q", got, sessionID)
		}
		body := "data: [DONE]\n\n"
		if strings.HasSuffix(request.URL.Path, "/models") {
			body = `{"data":[{"id":"kimi-k3"}]}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	provider, err := newOpenCodeGo(Config{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete(context.Background(), Request{Model: "kimi-k3"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.(ModelLister).Models(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeGoRestoresSessionIdentity(t *testing.T) {
	first, err := newOpenCodeGo(Config{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := newOpenCodeGo(Config{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.(*openAIProvider), second.(*openAIProvider)
	if err := b.RestoreSessionIdentity(a.SessionIdentity()); err != nil {
		t.Fatal(err)
	}
	if b.SessionIdentity() != a.SessionIdentity() {
		t.Fatal("routing identity changed")
	}
	if err := b.RestoreSessionIdentity("invalid\r\nheader"); err == nil {
		t.Fatal("invalid identity accepted")
	}
}

func TestOpenCodeGoAllowsBaseURLOverride(t *testing.T) {
	provider, err := newOpenCodeGo(Config{BaseURL: "http://go.test/custom/"})
	if err != nil {
		t.Fatal(err)
	}
	got := provider.(*openAIProvider).baseURL
	if got != "http://go.test/custom" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestOpenCodeGoListsAndAcceptsResponsesModels(t *testing.T) {
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"glm-5.2"},{"id":"gpt-5.6-luna"},{"id":"qwen3.8-max"}]}`)),
		}, nil
	})
	provider, err := newOpenCodeGo(Config{HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	models, err := provider.(ModelLister).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "glm-5.2,gpt-5.6-luna,qwen3.8-max" {
		t.Fatalf("models = %v", models)
	}
	for _, model := range []string{"gpt-5.6-luna", "grok-4.6", "muse-spark-1.2-contributor", "muse-spark-1.3-contributor"} {
		if openCodeGoModelRoutes[model] != openCodeGoResponsesRoute {
			t.Errorf("route for %q = %q", model, openCodeGoModelRoutes[model])
		}
		if err := provider.(ModelValidator).ValidateModel(model); err != nil {
			t.Errorf("%s validation error = %v", model, err)
		}
	}
	if err := provider.(ModelValidator).ValidateModel("glm-5.2"); err != nil {
		t.Fatalf("chat model validation error = %v", err)
	}
}

func TestOpenCodeGoUsesAnthropicMessagesForQwen(t *testing.T) {
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "go-secret" || request.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("headers = %#v", request.Header)
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    string `json:"system"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "qwen3.8-flash" || body.MaxTokens != 8192 || body.System != "system" || len(body.Messages) != 1 || body.Messages[0].Role != "user" || len(body.Tools) != 1 || body.Tools[0].Name != "read" {
			t.Fatalf("body = %#v", body)
		}
		stream := strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"read","input":{}}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"."}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"}"}}`,
			`event: message_delta`,
			`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
		}, "\n") + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	provider, err := newOpenCodeGo(Config{BaseURL: "http://go.test/v1", APIKey: "go-secret", HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Complete(context.Background(), Request{Model: "qwen3.8-flash", Messages: []Message{{Role: "system", Content: "system"}, {Role: "user", Content: "inspect"}}, Tools: []Tool{{Name: "read", Parameters: map[string]any{"type": "object"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage == nil || response.Usage.InputTokens != 11 || response.Usage.OutputTokens != 7 || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("response = %#v", response)
	}
	call := response.Message.ToolCalls[0]
	if call.ID != "tool_1" || call.Name != "read" || string(call.Arguments) != `{"path":"."}` {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestOpenCodeGoResponsesThinkingCatalog(t *testing.T) {
	provider, err := newOpenCodeGo(Config{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		model  string
		levels []string
	}{
		{"gpt-5.6-luna", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"grok-4.6", []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"muse-spark-1.2-contributor", []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"muse-spark-1.3-contributor", []string{"minimal", "low", "medium", "high", "xhigh"}},
	}
	for _, test := range tests {
		capability := provider.(ThinkingProvider).ThinkingCapability(test.model)
		if !capability.Supported || !capability.Adjustable || capability.AlwaysOn {
			t.Errorf("%s capability = %#v", test.model, capability)
		}
		if capability.RequestFormat != ThinkingRequestResponsesReasoning || capability.ReplayFormat != ThinkingReplayReasoningDetails {
			t.Errorf("%s formats = %q/%q", test.model, capability.RequestFormat, capability.ReplayFormat)
		}
		if strings.Join(capability.Levels, ",") != strings.Join(test.levels, ",") {
			t.Errorf("%s levels = %v, want %v", test.model, capability.Levels, test.levels)
		}
	}
}
