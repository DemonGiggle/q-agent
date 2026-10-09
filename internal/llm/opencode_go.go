package llm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const openCodeGoBaseURL = "https://opencode.ai/zen/go/v1"

const (
	openCodeGoChatRoute      = "chat/completions"
	openCodeGoMessagesRoute  = "messages"
	openCodeGoResponsesRoute = "responses"
)

// OpenCode Go's /models endpoint includes models on three wire protocols.
// Keep routing separate from thinking capabilities: the same model catalog can
// say both how a request is encoded and whether qcode has an adapter for it.
// Source: OpenCode Go model table, checked 2026-09-12.
var openCodeGoModelRoutes = map[string]string{
	"gpt-5.6-luna":               openCodeGoResponsesRoute,
	"grok-4.6":                   openCodeGoResponsesRoute,
	"minimax-m2.5":               openCodeGoMessagesRoute,
	"minimax-m2.7":               openCodeGoMessagesRoute,
	"minimax-m3":                 openCodeGoMessagesRoute,
	"muse-spark-1.2-contributor": openCodeGoResponsesRoute,
	"muse-spark-1.3-contributor": openCodeGoResponsesRoute,
	"qwen3.5-plus":               openCodeGoMessagesRoute,
	"qwen3.6-plus":               openCodeGoMessagesRoute,
	"qwen3.7-max":                openCodeGoMessagesRoute,
	"qwen3.7-plus":               openCodeGoMessagesRoute,
	"qwen3.8-flash":              openCodeGoMessagesRoute,
	"qwen3.8-max":                openCodeGoMessagesRoute,
}

func init() { Register("opencode-go", newOpenCodeGo) }

// newOpenCodeGo configures OpenCode Go's OpenAI-compatible Chat Completions
// endpoint. OpenCode asks third-party agents to identify themselves, so these
// requests use a narrow qcode-specific User-Agent instead of a browser value.
func newOpenCodeGo(config Config) (Provider, error) {
	sessionID, err := newOpenCodeSessionID()
	if err != nil {
		return nil, fmt.Errorf("create OpenCode Go session ID: %w", err)
	}
	provider, err := newOpenAICompatible(config, openCodeGoBaseURL, "opencode-go", "q-agent", sessionID)
	if err != nil {
		return nil, err
	}
	provider.(*openAIProvider).modelMeta = config.ModelMeta
	return provider, nil
}

// IsOfficialOpenCodeGoEndpoint reports whether baseURL selects OpenCode Go's
// built-in endpoint. An empty URL means the built-in endpoint will be used.
func IsOfficialOpenCodeGoEndpoint(baseURL string) bool {
	baseURL = strings.TrimRight(baseURL, "/")
	return baseURL == "" || baseURL == openCodeGoBaseURL
}

func (p *openAIProvider) ValidateModel(model string) error {
	if p.name != "opencode-go" {
		return nil
	}
	return nil
}

// newOpenCodeSessionID returns a UUIDv4 which is stable for the lifetime of a
// provider instance. OpenCode Go uses it to keep related requests on the same
// backend and requires the x-opencode-session header to be present.
func newOpenCodeSessionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	hexID := hex.EncodeToString(id[:])
	return hexID[0:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:32], nil
}
