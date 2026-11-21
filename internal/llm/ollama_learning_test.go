package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"q-agent/internal/learning"
)

func TestLearningOllamaRefusesRemoteEndpoint(t *testing.T) {
	for _, endpoint := range []string{"https://example.org", "http://localhost:11434", "http://127.0.0.1:11434/other"} {
		if _, err := NewLearningOllama(endpoint, "qwen3"); err == nil {
			t.Fatalf("unexpectedly accepted %s", endpoint)
		}
	}
	if _, err := NewLearningOllama("", "example:cloud"); err == nil {
		t.Fatal("accepted cloud model")
	}
}

func TestLearningOllamaRejectsMissingAndExcessUsage(t *testing.T) {
	for _, tc := range []struct {
		counts string
		usage  int64
	}{
		{`"eval_count":10`, 0},
		{`"prompt_eval_count":10,"eval_count":385`, 2433},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"done":true,%s,"message":{"content":"{}"}}`, tc.counts)
		}))
		model, err := NewLearningOllama(server.URL, "fixture")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		raw, usage, err := model.Generate(context.Background(), learning.StructuredRequest{
			Operation: "summarize_excerpt", SchemaJSON: []byte(`{}`), TokenLimit: 2432,
			References: []learning.DocumentSpan{{Text: strings.Repeat("x", 24)}},
		})
		server.Close()
		if err == nil || len(raw) != 0 || usage.Requests != 1 || usage.Tokens != tc.usage {
			t.Fatalf("raw=%q usage=%+v err=%v", raw, usage, err)
		}
	}
}
