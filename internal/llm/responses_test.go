package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenCodeGoResponsesUsesDedicatedRouteAndStreamsToolCall(t *testing.T) {
	client := doerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer go-secret" || request.Header.Get("x-opencode-session") == "" {
			t.Errorf("headers = %#v", request.Header)
		}
		var body struct {
			Model string            `json:"model"`
			Input []json.RawMessage `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "muse-spark-1.3-contributor" || len(body.Input) != 2 || len(body.Tools) != 1 || body.Tools[0].Name != "read" {
			t.Fatalf("body = %#v", body)
		}
		stream := strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"Checking"}`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":""}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":\"README.md\"}"}`,
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":4},"output":[{"type":"message","content":[{"type":"output_text","text":"Checking"}]},{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"README.md\"}"}]}}`,
		}, "\n") + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	provider, err := newOpenCodeGo(Config{BaseURL: "http://go.test/v1", APIKey: "go-secret", HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	var streamed string
	response, err := provider.Complete(context.Background(), Request{Model: "muse-spark-1.3-contributor", Messages: []Message{{Role: "system", Content: "be helpful"}, {Role: "user", Content: "inspect"}}, Tools: []Tool{{Name: "read", Parameters: map[string]any{"type": "object"}}}}, func(event StreamEvent) { streamed += event.Text })
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "Checking" || streamed != "Checking" || response.Usage == nil || response.Usage.InputTokens != 10 || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("response = %#v, streamed = %q", response, streamed)
	}
	call := response.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "read" || string(call.Arguments) != `{"path":"README.md"}` || !json.Valid(response.Message.ReasoningDetails) {
		t.Fatalf("call = %#v, details = %s", call, response.Message.ReasoningDetails)
	}
}

func TestResponsesInputReplaysOutputAndToolResult(t *testing.T) {
	raw := json.RawMessage(`[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"}]`)
	items := responsesInput([]Message{{Role: "assistant", ReasoningDetails: raw}, {Role: "tool", ToolCallID: "call_1", Content: "file contents"}})
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"encrypted_content":"opaque"`) || !strings.Contains(string(encoded), `"type":"function_call_output"`) || !strings.Contains(string(encoded), `"call_id":"call_1"`) {
		t.Fatalf("input = %s", encoded)
	}
}

func TestOpenCodeGoResponsesSendsModelValidReasoningEffort(t *testing.T) {
	tests := []struct {
		name, model, level, effort string
	}{
		{"luna selects max", "gpt-5.6-luna", "max", "max"},
		{"luna disables reasoning", "gpt-5.6-luna", "none", "none"},
		{"grok selects xhigh", "grok-4.6", "xhigh", "xhigh"},
		{"muse accepts minimal", "muse-spark-1.2-contributor", "minimal", "minimal"},
		{"model-invalid level omits object", "grok-4.6", "max", ""},
		{"empty level omits object", "muse-spark-1.3-contributor", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := doerFunc(func(request *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(request.URL.Path, "/responses") {
					t.Errorf("path = %q", request.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				var reasoning struct {
					Effort  string `json:"effort"`
					Summary string `json:"summary"`
				}
				_ = json.Unmarshal(body["reasoning"], &reasoning)
				if reasoning.Effort != test.effort {
					t.Errorf("reasoning.effort = %q, want %q", reasoning.Effort, test.effort)
				}
				wantSummary := ""
				if test.effort != "" && test.effort != "none" {
					wantSummary = "auto"
				}
				if reasoning.Summary != wantSummary {
					t.Errorf("reasoning.summary = %q, want %q", reasoning.Summary, wantSummary)
				}
				if test.effort == "" && body["reasoning"] != nil {
					t.Errorf("reasoning = %s, want the field omitted", body["reasoning"])
				}
				if _, ok := body["thinking"]; ok {
					t.Errorf("Responses request contains the Chat Completions thinking field")
				}
				if _, ok := body["reasoning_effort"]; ok {
					t.Errorf("Responses request contains the Chat Completions reasoning_effort field")
				}
				if _, ok := body["temperature"]; ok {
					t.Errorf("Responses request contains temperature")
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

func TestResponsesStreamCapturesReasoningSummary(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.reasoning_summary_text.delta","delta":"weigh "}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"options"}`,
		`data: {"type":"response.output_text.delta","delta":"done"}`,
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`,
	}, "\n") + "\n\n"
	var thinking, output strings.Builder
	response, err := parseResponsesStream(strings.NewReader(stream), func(event StreamEvent) {
		switch event.Kind {
		case StreamThinking:
			thinking.WriteString(event.Text)
		case StreamOutput:
			output.WriteString(event.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Thinking != "weigh options" || thinking.String() != "weigh options" {
		t.Fatalf("thinking = %q, streamed = %q", response.Message.Thinking, thinking.String())
	}
	if response.Message.Content != "done" || output.String() != "done" {
		t.Fatalf("content = %q, streamed = %q", response.Message.Content, output.String())
	}
}

func TestResponsesStreamReturnsErrors(t *testing.T) {
	tests := []struct {
		name, stream, want string
	}{
		{
			name:   "top-level error event",
			stream: `data: {"type":"error","message":"provider failed"}` + "\n\n",
			want:   "provider failed",
		},
		{
			name:   "failed response event",
			stream: `data: {"type":"response.failed","response":{"error":{"message":"model failed"}}}` + "\n\n",
			want:   "model failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseResponsesStream(strings.NewReader(test.stream), nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}
