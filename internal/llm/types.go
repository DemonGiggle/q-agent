package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Image is provider-neutral binary image input attached to a message.
type Image struct {
	MediaType string
	Data      []byte
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Origin and Untrusted are local provenance metadata. Providers without a
	// native trust field receive the same information in the content envelope.
	Origin    string `json:"origin,omitempty"`
	Untrusted bool   `json:"untrusted,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	// ReasoningDetails preserves provider-specific structured reasoning when a
	// model requires it on a later tool-call turn. Thinking remains the
	// normalized, displayable stream; this field is intentionally opaque.
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	Images           []Image         `json:"-"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Request struct {
	Model string
	// Thinking is an optional user-selected level. Providers must omit thinking
	// request fields when it is empty or unsupported for the selected model.
	Thinking    string
	Messages    []Message
	Tools       []Tool
	Temperature float64
}

type Response struct {
	Message Message
	Usage   *Usage
}

// ToolResult is the normalized output returned by a tool implementation.
type ToolResult struct {
	Output string
	// UserAnswer is direct input collected by an interactive tool. The agent
	// records it as a user message after satisfying the tool-call protocol.
	UserAnswer   string
	Diff         string
	Images       []Image
	ChangedFiles []string
	// EndTurn tells the agent loop that the tool completed the user's request
	// for this model turn. It is used by fire-and-forget orchestration tools so
	// the model cannot immediately start polling the background task.
	EndTurn bool
}

type StreamKind uint8

const (
	StreamOutput StreamKind = iota
	StreamThinking
)

type StreamEvent struct {
	Kind StreamKind
	Text string
}

type StreamCallback func(StreamEvent)

type Provider interface {
	Name() string
	Complete(ctx context.Context, request Request, onText StreamCallback) (Response, error)
}

// ModelLister is an optional provider capability used by interactive clients.
type ModelLister interface {
	Models(context.Context) ([]string, error)
}

// ThinkingCapability describes model-specific thinking behavior without
// exposing provider wire details to the UI or agent loop.
type ThinkingCapability struct {
	Supported     bool
	Adjustable    bool
	AlwaysOn      bool
	Levels        []string
	Default       string
	RequestFormat string
	ReplayFormat  string
}

// ThinkingProvider is optional. A provider that does not implement it has no
// user-selectable thinking policy.
type ThinkingProvider interface {
	ThinkingCapability(model string) ThinkingCapability
}

// ModelValidator is optional. It lets a provider reject a known model that is
// advertised by a shared catalog but requires an endpoint qcode does not yet
// implement.
type ModelValidator interface {
	ValidateModel(model string) error
}

type Factory func(Config) (Provider, error)

type Config struct {
	BaseURL            string
	APIKey             string
	HTTP               HTTPDoer
	InsecureSkipVerify bool
	// ModelMeta supplies exact-model reasoning choices from Models.dev. Request
	// and replay formats remain in qcode's provider adapters.
	ModelMeta *ProviderModelMeta
}

type HTTPDoer interface {
	Do(*httpRequest) (*httpResponse, error)
}

// Aliases keep the public provider seam small while allowing tests to inject clients.
// They are implemented by *http.Client through the adapter in http.go.
type httpRequest = requestAlias
type httpResponse = responseAlias

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

func New(name string, config Config) (Provider, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (available: %v)", name, Names())
	}
	return factory(config)
}

func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SessionUsage contains reported totals; Missing counts requests without usage.
type SessionUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	Missing      int `json:"missing_completions"`
}

// Usage describes one completion, not cumulative session billing.
type Usage struct {
	InputTokens  int `json:"prompt_tokens"`
	OutputTokens int `json:"completion_tokens"`
}

// ContextWindowProvider reports the active model's context capacity, or zero
// when unknown. Discovery must not prevent a completion from succeeding.
type ContextWindowProvider interface {
	ContextWindow(context.Context, string) (int, error)
}
