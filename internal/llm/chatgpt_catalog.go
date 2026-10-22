package llm

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// codexCatalogTTL mirrors the upstream Codex reference client
// (codex-rs models-manager/src/manager.rs: 300s). Fresh entries avoid a
// network round-trip; expired entries are refreshed on demand and the last
// known good snapshot is served only on retriable failures.
const codexCatalogTTL = 5 * time.Minute

// CodexReasoningLevelOption is one entry of
// supported_reasoning_levels: {effort, description}. The wire accepts both
// object and bare-string forms; bare strings carry no description.
type CodexReasoningLevelOption struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

// UnmarshalJSON tolerates {"effort","description"} objects and bare "effort"
// strings. Unknown shapes are rejected so decode stays strict on types but
// tolerant on unknown fields elsewhere.
func (o *CodexReasoningLevelOption) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*o = CodexReasoningLevelOption{}
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var effort string
		if err := json.Unmarshal(data, &effort); err != nil {
			return err
		}
		*o = CodexReasoningLevelOption{Effort: effort}
		return nil
	}
	type alias CodexReasoningLevelOption
	var obj alias
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*o = CodexReasoningLevelOption(obj)
	return nil
}

// CodexModelEntry is the decoded per-model metadata the catalog carries.
// Visibility only controls menu display; decode keeps every entry so later
// stages (ThinkingCapability, wire contract) can use per-model levels.
type CodexModelEntry struct {
	Slug               string                      `json:"slug"`
	DisplayName        string                      `json:"display_name,omitempty"`
	Visibility         string                      `json:"visibility,omitempty"`
	Aliases            []string                    `json:"aliases,omitempty"`
	DefaultReasoning   string                      `json:"default_reasoning_level,omitempty"`
	SupportedReasoning []CodexReasoningLevelOption `json:"supported_reasoning_levels,omitempty"`
	SupportsSummary    *bool                       `json:"supports_reasoning_summary_parameter,omitempty"`
	DefaultSummary     string                      `json:"default_reasoning_summary,omitempty"`
	MultiAgentEffort   string                      `json:"multi_agent_reasoning_effort,omitempty"`
}

// SupportsSummaryKnown reports whether the catalog explicitly states summary
// support. Missing field means unknown, not false.
func (e CodexModelEntry) SupportsSummaryKnown() (supported bool, known bool) {
	if e.SupportsSummary == nil {
		return false, false
	}
	return *e.SupportsSummary, true
}

// Levels returns the ordered effort names for this model.
func (e CodexModelEntry) Levels() []string {
	out := make([]string, 0, len(e.SupportedReasoning))
	for _, opt := range e.SupportedReasoning {
		if opt.Effort == "" {
			continue
		}
		out = append(out, opt.Effort)
	}
	return out
}

// codexCatalogPayload is the live catalog shape. Unknown fields are ignored
// by encoding/json. Legacy data[].id is kept as a fallback for custom
// baseURLs that do not serve the Codex shape.
type codexCatalogPayload struct {
	Models []CodexModelEntry `json:"models"`
	Data   []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// decodeCodexCatalog parses raw catalog JSON without discarding metadata.
// It returns model entries when the models array is present (len>0),
// otherwise legacy IDs. Visibility filtering is NOT applied here.
func decodeCodexCatalog(data []byte) ([]CodexModelEntry, []string, error) {
	var payload codexCatalogPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, err
	}
	if len(payload.Models) > 0 {
		entries := make([]CodexModelEntry, 0, len(payload.Models))
		for _, m := range payload.Models {
			// Keep entries even with empty slug here; display stage skips
			// them. Preserve aliases ordering as sent.
			entries = append(entries, m)
		}
		return entries, nil, nil
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if d.ID == "" {
			continue
		}
		ids = append(ids, d.ID)
	}
	return nil, ids, nil
}

// codexDisplaySlugs applies menu display filtering on decoded entries,
// preserving server order. Empty slug is skipped. Empty visibility is
// treated as listable for forward-compat.
func codexDisplaySlugs(entries []CodexModelEntry) []string {
	out := make([]string, 0, len(entries))
	for _, m := range entries {
		if m.Slug == "" {
			continue
		}
		if m.Visibility != "" && m.Visibility != "list" {
			continue
		}
		out = append(out, m.Slug)
	}
	return out
}

// CodexCatalogSnapshot is a scoped last-known-good view. It carries no
// credential material: no tokens, no account IDs. Diagnostics can expose
// Source/FetchedAt/Stale safely.
type CodexCatalogSnapshot struct {
	Entries       []CodexModelEntry `json:"-"`
	DisplaySlugs  []string          `json:"display_slugs"`
	FetchedAt     time.Time         `json:"fetched_at"`
	Source        string            `json:"source"`
	Stale         bool              `json:"stale"`
	Endpoint      string            `json:"endpoint"`
	ClientVersion string            `json:"client_version"`
	Count         int               `json:"count"`
}

// Describe returns a diagnostics-safe one-liner without secrets.
func (s CodexCatalogSnapshot) Describe() string {
	stale := "fresh"
	if s.Stale {
		stale = "stale"
	}
	return "codex-catalog source=" + s.Source +
		" endpoint=" + s.Endpoint +
		" client_version=" + s.ClientVersion +
		" count=" + itoa(s.Count) +
		" " + stale
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

type codexCatalogCacheEntry struct {
	snapshot  CodexCatalogSnapshot
	expiresAt time.Time
}

var (
	codexCatalogMu    sync.RWMutex
	codexCatalogByKey = map[string]codexCatalogCacheEntry{}
)

// codexCatalogCacheKey scopes entries by endpoint + auth identity + catalog
// compatibility version + token file. Tokens path is included (superset of
// the required scope) so distinct local files with the same account do not
// share, which also keeps unit tests isolated. Empty account falls back to
// path isolation.
func codexCatalogCacheKey(endpoint, accountID, tokensPath, clientVersion string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	return endpoint + "\x00" + accountID + "\x00" + clientVersion + "\x00" + tokensPath
}

func getCodexCatalogCache(key string, now time.Time) (CodexCatalogSnapshot, bool) {
	codexCatalogMu.RLock()
	entry, ok := codexCatalogByKey[key]
	codexCatalogMu.RUnlock()
	if !ok {
		return CodexCatalogSnapshot{}, false
	}
	snap := entry.snapshot
	snap.Stale = now.After(entry.expiresAt)
	return snap, true
}

func putCodexCatalogCache(key string, snap CodexCatalogSnapshot) {
	codexCatalogMu.Lock()
	codexCatalogByKey[key] = codexCatalogCacheEntry{
		snapshot:  snap,
		expiresAt: snap.FetchedAt.Add(codexCatalogTTL),
	}
	codexCatalogMu.Unlock()
}

// clearCodexCatalogCache resets the global cache. Tests only.
func clearCodexCatalogCache() {
	codexCatalogMu.Lock()
	codexCatalogByKey = map[string]codexCatalogCacheEntry{}
	codexCatalogMu.Unlock()
}
