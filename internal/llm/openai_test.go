package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestOpenAIStreamsTextAndToolCall(t *testing.T) {
	var gotAuthorization string
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		gotAuthorization = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true {
			t.Errorf("stream = %v", body["stream"])
		}
		streamBody := strings.Join([]string{
			`data: {"choices":[{"delta":{"reasoning_content":"checking "}}]}`,
			`data: {"choices":[{"delta":{"content":"hello "}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"abc","function":{"name":"re","arguments":"{\"pa"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"ad","arguments":"th\":\"x\"}"}}]}}]}`,
			`data: [DONE]`, "",
		}, "\n")
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(streamBody))}, nil
	})

	provider, err := newOpenAI(Config{BaseURL: "http://provider.test/v1", APIKey: "secret", HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	response, err := provider.Complete(context.Background(), Request{Model: "test", Messages: []Message{{Role: "user", Content: "hi"}}}, func(event StreamEvent) {
		fmt.Fprintf(&streamed, "%d:%s", event.Kind, event.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuthorization != "Bearer secret" {
		t.Errorf("authorization = %q", gotAuthorization)
	}
	if streamed.String() != "1:checking 0:hello " || response.Message.Content != "hello " {
		t.Errorf("content = %q / %q", streamed.String(), response.Message.Content)
	}
	if len(response.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(response.Message.ToolCalls))
	}
	call := response.Message.ToolCalls[0]
	if call.ID != "abc" || call.Name != "read" || string(call.Arguments) != `{"path":"x"}` {
		t.Errorf("tool call = %#v", call)
	}
}

func TestOpenAISendsImageContentParts(t *testing.T) {
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 1 {
			t.Fatalf("messages = %#v", payload.Messages)
		}
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(payload.Messages[0].Content, &parts); err != nil {
			t.Fatal(err)
		}
		if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "describe" || parts[1].Type != "image_url" || parts[1].ImageURL.URL != "data:image/png;base64,AQID" {
			t.Fatalf("content parts = %#v", parts)
		}
		streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"seen\"}}]}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(streamBody))}, nil
	})
	provider, err := newOpenAI(Config{BaseURL: "http://provider.test/v1", HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Model: "vision", Messages: []Message{{
		Role: "user", Content: "describe", Images: []Image{{MediaType: "image/png", Data: []byte{1, 2, 3}}},
	}}}
	if _, err := provider.Complete(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProviderNamesAreStable(t *testing.T) {
	got := strings.Join(Names(), ",")
	if got != "ollama,openai,opencode-go" {
		t.Fatalf("names = %q", got)
	}
}

func TestOpenAIListsModels(t *testing.T) {
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"model-b"},{"id":"model-a"}]}`))}, nil
	})
	provider, err := newOpenAI(Config{BaseURL: "http://provider.test/v1", APIKey: "secret", HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	models, err := provider.(ModelLister).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "model-b,model-a" {
		t.Fatalf("models = %v", models)
	}
}
