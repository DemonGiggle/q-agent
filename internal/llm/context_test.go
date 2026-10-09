package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestProviderUsage(t *testing.T) {
	for _, name := range []string{"openai", "opencode-go", "ollama"} {
		t.Run(name, func(t *testing.T) {
			client := doerFunc(func(r *http.Request) (*http.Response, error) {
				body := ""
				if name == "ollama" {
					body = `{"message":{"content":"hi"},"done":false}` + "\n" + `{"done":true,"prompt_eval_count":750,"eval_count":50}` + "\n"
				} else {
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					options, ok := request["stream_options"].(map[string]any)
					if !ok || options["include_usage"] != true {
						t.Fatal("missing stream usage request")
					}
					body = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
						"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":750,\"completion_tokens\":50}}\n\ndata: [DONE]\n"
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			p, err := New(name, Config{HTTP: client})
			if err != nil {
				t.Fatal(err)
			}
			response, err := p.Complete(context.Background(), Request{Model: "test"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.Message.Content != "hi" || response.Usage == nil || *response.Usage != (Usage{750, 50}) {
				t.Fatalf("response = %+v", response)
			}
		})
	}
}

func TestMissingUsage(t *testing.T) {
	r, err := parseOpenAIStream(strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n"), nil)
	if err != nil || r.Usage != nil {
		t.Fatalf("response=%+v error=%v", r, err)
	}
}

func TestContextWindowDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, model, base string
		want              int
	}{
		{"openai", "gpt-4o", "", 128000}, {"opencode-go", "glm-5", "", 202752},
		{"openai", "unknown", "", 0}, {"openai", "gpt-4o", "https://custom.test", 0},
	} {
		p, _ := New(tc.name, Config{BaseURL: tc.base})
		got, err := p.(ContextWindowProvider).ContextWindow(context.Background(), tc.model)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %d, %v", tc, got, err)
		}
	}
	p, _ := newOllama(Config{HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("path=%s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"name":"other:latest","context_length":999},{"name":"test:latest","context_length":8192}]}`))}, nil
	})})
	got, err := p.(ContextWindowProvider).ContextWindow(context.Background(), "test")
	if err != nil || got != 8192 {
		t.Fatalf("%d, %v", got, err)
	}
}
