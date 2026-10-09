package llm

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const modelsDevFixture = `{
  "openai": {"models": {"unrelated": {"reasoning_options": [{"type": "effort", "values": ["wrong"]}]}}},
  "opencode-go": {"models": {
    "gpt-5.6-luna": {"reasoning_options": [
      {"type": "effort", "values": ["low", "high", "max"]},
      {"type": "budget", "values": [1024]}
    ]},
    "glm-5": {"reasoning_options": [{"type": "toggle"}]},
    "unknown-format": {"reasoning_options": [{"type": "budget", "values": [2048]}]}
  }}
}`

func TestParseProviderModelMeta(t *testing.T) {
	meta, err := ParseProviderModelMeta([]byte(modelsDevFixture), "opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != ProviderModelMetaVersion || meta.Source != modelsDevCatalogURL {
		t.Fatalf("metadata header = %+v", meta)
	}
	if got, want := meta.Models["gpt-5.6-luna"].Effort, []string{"low", "high", "max"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("effort options = %v, want %v", got, want)
	}
	if !meta.Models["glm-5"].Toggle {
		t.Fatal("toggle option was not parsed")
	}
	if got := meta.Models["unknown-format"]; got.Toggle || len(got.Effort) != 0 {
		t.Fatalf("unsupported option was retained: %+v", got)
	}
	if _, ok := meta.Models["unrelated"]; ok {
		t.Fatal("unrelated provider model was parsed")
	}
}

func TestLoadProviderModelMetaFetchesOnlyOnCacheMiss(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache", "metadata.json")
	calls := 0
	client := metadataDoer(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.String() != modelsDevCatalogURL {
			t.Errorf("request = %s %s", request.Method, request.URL)
		}
		return metadataResponse(http.StatusOK, modelsDevFixture), nil
	})
	first, err := LoadProviderModelMeta(context.Background(), cachePath, "opencode-go", client)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(first.Models) != 3 {
		t.Fatalf("calls = %d, models = %d", calls, len(first.Models))
	}
	if info, err := os.Stat(cachePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache stat = %v, err = %v; want mode 0600", info, err)
	}

	second, err := LoadProviderModelMeta(context.Background(), cachePath, "opencode-go", metadataDoer(func(*http.Request) (*http.Response, error) {
		t.Fatal("valid cache triggered a fetch")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(first.Models, second.Models) {
		t.Fatalf("cache load fetched again or changed data: calls=%d models=%v", calls, second.Models)
	}
}

func TestLoadProviderModelMetaContinuesWithoutChoicesOnFirstFetchFailure(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "missing", "metadata.json")
	meta, err := LoadProviderModelMeta(context.Background(), cachePath, "opencode-go", metadataDoer(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	}))
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if meta == nil || len(meta.Models) != 0 {
		t.Fatalf("fallback metadata = %#v", meta)
	}
	if _, statErr := os.Stat(cachePath); !os.IsNotExist(statErr) {
		t.Fatalf("failed first fetch created cache: %v", statErr)
	}
}

func TestRefreshProviderModelMetaPreservesGoodCacheOnFailure(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "metadata.json")
	good, err := ParseProviderModelMeta([]byte(modelsDevFixture), "opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeProviderModelMeta(cachePath, good); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"invalid json", http.StatusOK, `{not-json`},
		{"missing provider", http.StatusOK, `{"openai":{"models":{}}}`},
		{"http error", http.StatusServiceUnavailable, `unavailable`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, refreshErr := RefreshProviderModelMeta(context.Background(), cachePath, "opencode-go", metadataDoer(func(*http.Request) (*http.Response, error) {
				return metadataResponse(tc.status, tc.body), nil
			}))
			if refreshErr == nil {
				t.Fatal("expected refresh failure")
			}
			current, readErr := os.ReadFile(cachePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(current, original) {
				t.Fatalf("failed refresh changed cache:\n%s", current)
			}
		})
	}
}

func TestOpenCodeGoSyncedOptionsRequireLocalAdapterAndKeepEncoding(t *testing.T) {
	meta, err := ParseProviderModelMeta([]byte(modelsDevFixture), "opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	var requestBody string
	client := metadataDoer(func(request *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		requestBody = string(body)
		stream := "data: [DONE]\n\n"
		if strings.HasSuffix(request.URL.Path, "/responses") {
			stream = `data: {"type":"response.completed","response":{"output":[]}}` + "\n\n"
		}
		return metadataResponse(http.StatusOK, stream), nil
	})
	provider, err := newOpenCodeGo(Config{ModelMeta: meta, HTTP: client})
	if err != nil {
		t.Fatal(err)
	}
	thinking := provider.(ThinkingProvider)
	capability := thinking.ThinkingCapability("gpt-5.6-luna")
	if !capability.Adjustable || !reflect.DeepEqual(capability.Levels, []string{"low", "high", "max"}) {
		t.Fatalf("synced capability = %+v", capability)
	}
	toggle := thinking.ThinkingCapability("glm-5")
	if !toggle.Adjustable || !reflect.DeepEqual(toggle.Levels, []string{"off", "on"}) {
		t.Fatalf("synced toggle = %+v", toggle)
	}
	if unknown := thinking.ThinkingCapability("unknown-format"); unknown.Supported || unknown.Adjustable {
		t.Fatalf("model without a local adapter got thinking choices: %+v", unknown)
	}
	if _, err := provider.Complete(context.Background(), Request{Model: "gpt-5.6-luna", Thinking: "high"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestBody, `"reasoning":{"effort":"high","summary":"auto"}`) {
		t.Fatalf("synced effort changed Responses encoding: %s", requestBody)
	}
	if _, err := provider.Complete(context.Background(), Request{Model: "glm-5", Thinking: "on"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestBody, `"thinking":{"type":"enabled"}`) {
		t.Fatalf("synced toggle changed Chat Completions encoding: %s", requestBody)
	}
}

type metadataDoer func(*http.Request) (*http.Response, error)

func (do metadataDoer) Do(request *http.Request) (*http.Response, error) { return do(request) }

func metadataResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
