// Package mcpclient discovery adapts MCP tools/list results into
// provider-ready tool schemas (issue #49).
//
// One Task owns one connection lifetime (#46); discovery runs against the
// Task's live sessions and publishes an immutable Catalog snapshot per
// model-request boundary. A later task re-discovers from scratch; snapshots
// are never mutated in place and never shared mutably between chats.
//
// Responsibilities of this file only:
//   - Page through every tools/list result with bounded page/tool counts and
//     loop protection (repeated or non-advancing cursors).
//   - Map (server identity, original tool name) to stable,
//     provider-compatible names, resolving collisions across servers, after
//     normalization/truncation, and with caller-supplied reserved
//     (built-in/process/schedule) names.
//   - Preserve input JSON Schema semantics into llm.Tool.Parameters, retain
//     output schemas and annotations locally, and never silently weaken a
//     schema to satisfy a provider.
//   - Validate tool metadata/schemas before provider exposure; quarantine
//     invalid or unrepresentable tools with sanitized diagnostics so one
//     tool cannot poison all model requests.
//   - Refresh on tools/list-changed notifications via the SDK's
//     version-appropriate mechanism (ToolListChangedHandler, which the SDK
//     backs with subscriptions/listen on modern servers and legacy
//     notifications/tools/list_changed elsewhere).
//
// This file performs no execution (#50) and no runtime/agent wiring (#51):
// dispatch here only resolves a call against a snapshot and rejects
// stale/unknown names instead of remapping them.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/llm"
)

// Bounds for tools/list discovery. Pagination is always bounded so a
// misbehaving server cannot stall or flood the model request.
const (
	// DefaultMaxDiscoveryPages caps tools/list pages fetched per server.
	DefaultMaxDiscoveryPages = 32
	// DefaultMaxDiscoveryToolsPerServer caps tools kept per server.
	DefaultMaxDiscoveryToolsPerServer = 256
	// DefaultMaxDiscoveryToolsTotal caps tools kept across all servers in
	// one snapshot. Overflow tools are quarantined, never silently merged.
	DefaultMaxDiscoveryToolsTotal = 1024
	// MaxAdaptedNameLen is the provider-compatible name limit
	// (OpenAI/Anthropic/Ollama-compatible: ^[A-Za-z0-9_-]{1,64}$).
	MaxAdaptedNameLen = 64
	// MaxInputSchemaBytes bounds the JSON-encoded input schema exposed to
	// providers. Oversized schemas are quarantined so one tool cannot blow
	// up every model request.
	MaxInputSchemaBytes = 64 * 1024
	// MaxDescriptionChars bounds the provider-facing description. The full
	// text is retained locally; the exposed copy is truncated explicitly.
	MaxDescriptionChars = 4096
)

// adaptedNamePattern is the provider-compatible tool-name alphabet.
var adaptedNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DiscoveryLimits bounds one tools/list pagination per server.
type DiscoveryLimits struct {
	// MaxPages caps pages fetched; <=0 selects DefaultMaxDiscoveryPages.
	MaxPages int
	// MaxToolsPerServer caps tools kept; <=0 selects
	// DefaultMaxDiscoveryToolsPerServer.
	MaxToolsPerServer int
}

func (l DiscoveryLimits) withDefaults() DiscoveryLimits {
	if l.MaxPages <= 0 {
		l.MaxPages = DefaultMaxDiscoveryPages
	}
	if l.MaxToolsPerServer <= 0 {
		l.MaxToolsPerServer = DefaultMaxDiscoveryToolsPerServer
	}
	return l
}

// ToolLister is the tools/list surface discovery pages through. It matches
// (*SDKClientSession).ListTools and exists so tests can substitute fakes;
// production always passes live sessions.
type ToolLister interface {
	ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
}

// ServerListing is one server's bounded tools/list outcome.
type ServerListing struct {
	// Tools holds the collected tools in server order, truncated to the
	// per-server limit.
	Tools []*mcp.Tool
	// Truncated is true when the per-server tool cap dropped trailing tools.
	Truncated bool
	// Seen counts tools observed before truncation (for diagnostics).
	Seen int
}

// ListServerTools retrieves every tools/list page for one session.
//
// It enforces ctx cancellation, a page cap, a per-server tool cap, and loop
// protection: a repeated nextCursor or a cursor that does not advance ends
// pagination with an error instead of looping forever. A nil result, a nil
// session, or a failed page is an error; nil tool entries inside a page are
// skipped defensively. Tool-count overflow truncates (Truncated=true) rather
// than failing, so a chatty server cannot deny service to healthy ones;
// integrity failures (loops, page-cap breach, RPC errors) are errors and the
// caller must treat that server as undiscovered.
func ListServerTools(ctx context.Context, session ToolLister, limits DiscoveryLimits) (ServerListing, error) {
	limits = limits.withDefaults()
	if session == nil {
		return ServerListing{}, fmt.Errorf("mcp: nil tool session")
	}
	var out ServerListing
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if page >= limits.MaxPages {
			return out, fmt.Errorf("mcp: tool pagination exceeded %d pages; possible nonterminating listing", limits.MaxPages)
		}
		if seen[cursor] && page > 0 {
			return out, fmt.Errorf("mcp: repeated tools/list cursor %q; possible nonterminating listing", cursor)
		}
		seen[cursor] = true
		params := &mcp.ListToolsParams{}
		if cursor != "" {
			params.Cursor = cursor
		}
		result, err := session.ListTools(ctx, params)
		if err != nil {
			return out, err
		}
		if result == nil {
			return out, fmt.Errorf("mcp: nil tools/list result")
		}
		next := result.NextCursor
		if next != "" {
			if next == cursor {
				return out, fmt.Errorf("mcp: tools/list cursor did not advance; possible nonterminating listing")
			}
			if seen[next] {
				return out, fmt.Errorf("mcp: repeated tools/list cursor %q; possible nonterminating listing", next)
			}
		}
		for _, tool := range result.Tools {
			if tool == nil {
				continue
			}
			out.Seen++
			if len(out.Tools) >= limits.MaxToolsPerServer {
				out.Truncated = true
				continue
			}
			out.Tools = append(out.Tools, tool)
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// sanitizeSegment maps one name segment into the provider alphabet. Every
// rune outside [A-Za-z0-9_-] becomes '_'. Empty input yields "_".
func sanitizeSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// BaseAdaptedName maps (server identity, original tool name) to the stable
// base provider name before collision resolution. The server identity is
// always included so identical tool names on different servers stay
// distinct. The result matches the provider alphabet and length limit.
func BaseAdaptedName(server, original string) string {
	server = sanitizeSegment(strings.TrimSpace(server))
	original = sanitizeSegment(strings.TrimSpace(original))
	if server == "" || server == "_" {
		server = "mcp"
	}
	if original == "" || original == "_" {
		original = "tool"
	}
	base := server + "__" + original
	if len(base) > MaxAdaptedNameLen {
		base = base[:MaxAdaptedNameLen]
	}
	return base
}

// NameCandidate is one tool awaiting collision resolution.
type NameCandidate struct {
	Server   string
	Original string
}

// ResolveAdaptedNames assigns stable provider-compatible names to every
// candidate. Candidates are ordered by (server, original) so suffixes are
// deterministic across refreshes with the same input set. A name already in
// reserved (built-in/process/schedule names supplied by the caller) or
// claimed by an earlier candidate gains a "_2", "_3", ... suffix, shrinking
// the base to keep the 64-character limit. Every returned name matches the
// provider pattern; the returned map keys are candidate indexes.
func ResolveAdaptedNames(candidates []NameCandidate, reserved map[string]bool) map[int]string {
	assigned := make(map[int]string, len(candidates))
	used := map[string]bool{}
	for name := range reserved {
		if name != "" {
			used[name] = true
		}
	}
	order := make([]int, len(candidates))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		ca, cb := candidates[order[a]], candidates[order[b]]
		if ca.Server != cb.Server {
			return ca.Server < cb.Server
		}
		if ca.Original != cb.Original {
			return ca.Original < cb.Original
		}
		return order[a] < order[b]
	})
	for _, idx := range order {
		base := BaseAdaptedName(candidates[idx].Server, candidates[idx].Original)
		name := base
		for n := 2; ; n++ {
			if !used[name] {
				break
			}
			suffix := fmt.Sprintf("_%d", n)
			trim := MaxAdaptedNameLen - len(suffix)
			if trim < 1 {
				trim = 1
			}
			short := base
			if len(short) > trim {
				short = short[:trim]
			}
			name = short + suffix
		}
		used[name] = true
		assigned[idx] = name
	}
	return assigned
}

// ReservedSet builds the reserved-name set from built-in/process/schedule
// tool names supplied by the caller (for example localtools.Schemas names).
// Discovery never imports those packages itself, avoiding import cycles; the
// runtime wrapper (#51) supplies the set.
func ReservedSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			set[name] = true
		}
	}
	return set
}

// ToolNames extracts tool names from provider-ready schemas for ReservedSet.
func ToolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Name != "" {
			names = append(names, tool.Name)
		}
	}
	return names
}

// AdaptInputSchema validates raw MCP inputSchema and returns a deep copy
// safe to expose as llm.Tool.Parameters. Semantics are preserved verbatim:
// every key survives the copy, required entries are kept, and no
// additionalProperties value is injected. A nil schema becomes the empty
// object schema.
//
// Errors (quarantine, never silent weakening) cover: unmarshalable values,
// non-object schemas, a non-"object" type, non-object properties,
// non-string required entries, and schemas whose JSON exceeds
// MaxInputSchemaBytes.
func AdaptInputSchema(raw any) (map[string]any, error) {
	if raw == nil {
		return map[string]any{"type": "object"}, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp: tool input schema is not JSON-representable: %v", err)
	}
	if len(data) > MaxInputSchemaBytes {
		return nil, fmt.Errorf("mcp: tool input schema too large (%d bytes)", len(data))
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("mcp: tool input schema is not a JSON object")
	}
	if schema == nil {
		return map[string]any{"type": "object"}, nil
	}
	if typ, ok := schema["type"]; ok && typ != nil {
		s, ok := typ.(string)
		if !ok || s != "object" {
			return nil, fmt.Errorf("mcp: tool input schema type %q is not an object schema", fmt.Sprint(typ))
		}
	}
	if props, ok := schema["properties"]; ok && props != nil {
		propMap, ok := props.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp: tool input schema properties is not an object")
		}
		for name, prop := range propMap {
			if prop == nil {
				continue
			}
			if _, ok := prop.(map[string]any); !ok {
				return nil, fmt.Errorf("mcp: tool input schema property %q is not an object", name)
			}
		}
	}
	if req, ok := schema["required"]; ok && req != nil {
		list, ok := req.([]any)
		if !ok {
			return nil, fmt.Errorf("mcp: tool input schema required is not an array")
		}
		for _, entry := range list {
			s, ok := entry.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("mcp: tool input schema required entry is not a nonempty string")
			}
		}
	}
	return schema, nil
}

// copyJSONValue deep-copies a JSON-representable value via round-trip. It
// reports false for values that cannot survive provider exposure.
func copyJSONValue(raw any) (map[string]any, bool) {
	if raw == nil {
		return nil, true
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false
	}
	return out, true
}

// sanitizeDescription bounds provider-facing text without touching the
// locally retained full description.
func sanitizeDescription(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	if len(s) > MaxDescriptionChars {
		s = strings.TrimSpace(s[:MaxDescriptionChars]) + "…"
	}
	return s
}

// DiscoveredTool is one validated MCP tool. Tool is the provider-ready
// schema; OutputSchema, Annotations, Title, and Description retain the full
// local metadata (output schemas and annotations are never sent to
// providers by discovery).
type DiscoveredTool struct {
	Server       string
	Original     string
	Adapted      string
	Tool         llm.Tool
	OutputSchema map[string]any
	Annotations  *mcp.ToolAnnotations
	Title        string
	Description  string
}

// QuarantineEntry records one tool (or server listing) withheld from
// providers. Reason is sanitized and bounded; it never carries host details.
type QuarantineEntry struct {
	Server   string
	Original string
	Reason   string
}

func quarantineReason(server, original, format string, args ...any) QuarantineEntry {
	return QuarantineEntry{
		Server:   sanitizeName(server),
		Original: sanitizeName(original),
		Reason:   sanitizeError(fmt.Errorf(format, args...)),
	}
}

// AdaptTool validates one MCP tool and adapts it for provider exposure. The
// adapted name is the collision-free resolved name supplied by the caller
// (see ResolveAdaptedNames). Input schema semantics are preserved; output
// schema and annotations are retained locally. Any validation failure is an
// error for quarantine: the tool must not reach providers.
func AdaptTool(server string, tool *mcp.Tool, adapted string) (DiscoveredTool, error) {
	if tool == nil {
		return DiscoveredTool{}, fmt.Errorf("mcp: nil tool")
	}
	original := strings.TrimSpace(tool.Name)
	if original == "" {
		return DiscoveredTool{}, fmt.Errorf("mcp: tool with empty name")
	}
	if !utf8.ValidString(original) {
		return DiscoveredTool{}, fmt.Errorf("mcp: tool name is not valid UTF-8")
	}
	if adapted == "" || !adaptedNamePattern.MatchString(adapted) {
		return DiscoveredTool{}, fmt.Errorf("mcp: adapted tool name %q is not provider-compatible", adapted)
	}
	params, err := AdaptInputSchema(tool.InputSchema)
	if err != nil {
		return DiscoveredTool{}, err
	}
	output, ok := copyJSONValue(tool.OutputSchema)
	if !ok {
		return DiscoveredTool{}, fmt.Errorf("mcp: tool output schema is not JSON-representable")
	}
	var annotations *mcp.ToolAnnotations
	if tool.Annotations != nil {
		cp := *tool.Annotations
		annotations = &cp
	}
	description := strings.TrimSpace(strings.ToValidUTF8(tool.Description, "�"))
	return DiscoveredTool{
		Server:       server,
		Original:     original,
		Adapted:      adapted,
		Tool:         llm.Tool{Name: adapted, Description: sanitizeDescription(description), Parameters: params},
		OutputSchema: output,
		Annotations:  annotations,
		Title:        tool.Title,
		Description:  description,
	}, nil
}

// Catalog is an immutable snapshot of one discovery pass. It is published at
// model-request boundaries: the runtime wrapper (#51) discovers once per
// turn and exposes exactly this set for that turn. There are no exported
// fields; every accessor returns deep copies, so callers cannot mutate a
// snapshot and dispatch never observes a half-refreshed tool set.
type Catalog struct {
	tools   []DiscoveredTool
	byName  map[string]int
	at      time.Time
	version int64
}

// CatalogVersion identifies snapshot order for diagnostics. It is process
// local and monotonic.
var catalogVersion int64

// Len reports the number of provider-ready tools in the snapshot.
func (c Catalog) Len() int { return len(c.tools) }

// Empty reports whether the snapshot exposes no tools.
func (c Catalog) Empty() bool { return len(c.tools) == 0 }

// Tools returns a deep copy of the provider-ready schemas for this
// snapshot. Mutating the result cannot affect the catalog.
func (c Catalog) Tools() []llm.Tool {
	out := make([]llm.Tool, 0, len(c.tools))
	for _, entry := range c.tools {
		out = append(out, llm.Tool{
			Name:        entry.Tool.Name,
			Description: entry.Tool.Description,
			Parameters:  deepCopySchema(entry.Tool.Parameters),
		})
	}
	return out
}

// Lookup returns the catalog entry for an adapted provider name. The result
// is a deep copy; the second result is false for unknown names.
func (c Catalog) Lookup(adapted string) (DiscoveredTool, bool) {
	idx, ok := c.byName[adapted]
	if !ok || idx < 0 || idx >= len(c.tools) {
		return DiscoveredTool{}, false
	}
	return deepCopyEntry(c.tools[idx]), true
}

// Resolve maps a model tool call to the catalog entry for this snapshot. It
// matches adapted names only and rejects stale or unknown names with an
// error instead of remapping them to a different server or tool.
func (c Catalog) Resolve(call llm.ToolCall) (DiscoveredTool, error) {
	entry, ok := c.Lookup(call.Name)
	if !ok {
		return DiscoveredTool{}, fmt.Errorf("mcp: unknown or stale tool %q; refresh discovery instead of remapping", call.Name)
	}
	return entry, nil
}

func deepCopySchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out, ok := copyJSONValue(schema)
	if !ok {
		return nil
	}
	return out
}

func deepCopyEntry(entry DiscoveredTool) DiscoveredTool {
	cp := entry
	cp.Tool.Parameters = deepCopySchema(entry.Tool.Parameters)
	cp.OutputSchema = deepCopySchema(entry.OutputSchema)
	if entry.Annotations != nil {
		annotations := *entry.Annotations
		cp.Annotations = &annotations
	}
	return cp
}

// DiscoverOptions configures one discovery pass.
type DiscoverOptions struct {
	// Limits bounds per-server pagination; zero selects defaults.
	Limits DiscoveryLimits
	// Reserved holds built-in/process/schedule tool names that adapted
	// names must not collide with. Nil exposes MCP tools without builtin
	// collision checks; the runtime wrapper (#51) always supplies the
	// live set.
	Reserved map[string]bool
	// MaxTotal caps tools across all servers; <=0 selects
	// DefaultMaxDiscoveryToolsTotal.
	MaxTotal int
}

func (o DiscoverOptions) maxTotal() int {
	if o.MaxTotal <= 0 {
		return DefaultMaxDiscoveryToolsTotal
	}
	return o.MaxTotal
}

// DiscoverSessions runs a full discovery pass over the given sessions in
// order and publishes one immutable Catalog plus quarantine entries.
// Sessions are [ToolLister] so tests use fakes; Discover wraps live Task
// sessions. One server's listing or adaptation failure never denies service
// to healthy servers: failures become quarantine entries.
func DiscoverSessions(ctx context.Context, sessions map[string]ToolLister, order []string, opts DiscoverOptions) (Catalog, []QuarantineEntry) {
	limits := opts.Limits.withDefaults()
	maxTotal := opts.maxTotal()
	type raw struct {
		server string
		tool   *mcp.Tool
	}
	var raws []raw
	var quarantined []QuarantineEntry
	for _, server := range order {
		if err := ctx.Err(); err != nil {
			quarantined = append(quarantined, quarantineReason(server, "", "mcp: discovery cancelled: %v", err))
			break
		}
		lister, ok := sessions[server]
		if !ok || lister == nil {
			quarantined = append(quarantined, quarantineReason(server, "", "mcp: no live session for server %s", sanitizeName(server)))
			continue
		}
		listing, err := ListServerTools(ctx, lister, limits)
		if err != nil {
			quarantined = append(quarantined, quarantineReason(server, "", "mcp: tools/list failed: %v", err))
			continue
		}
		if listing.Truncated {
			quarantined = append(quarantined, quarantineReason(server, "", "mcp: kept %d of %d tools; per-server limit %d", len(listing.Tools), listing.Seen, limits.MaxToolsPerServer))
		}
		for _, tool := range listing.Tools {
			raws = append(raws, raw{server: server, tool: tool})
		}
	}
	candidates := make([]NameCandidate, len(raws))
	for i, r := range raws {
		name := ""
		if r.tool != nil {
			name = r.tool.Name
		}
		candidates[i] = NameCandidate{Server: r.server, Original: name}
	}
	assigned := ResolveAdaptedNames(candidates, opts.Reserved)
	var entries []DiscoveredTool
	for i, r := range raws {
		if len(entries) >= maxTotal {
			original := ""
			if r.tool != nil {
				original = r.tool.Name
			}
			quarantined = append(quarantined, quarantineReason(r.server, original, "mcp: dropped by global tool budget %d", maxTotal))
			continue
		}
		adapted, err := AdaptTool(r.server, r.tool, assigned[i])
		if err != nil {
			original := ""
			if r.tool != nil {
				original = r.tool.Name
			}
			quarantined = append(quarantined, quarantineReason(r.server, original, "mcp: quarantined tool: %v", err))
			continue
		}
		entries = append(entries, adapted)
	}
	sort.Slice(entries, func(a, b int) bool {
		if entries[a].Adapted != entries[b].Adapted {
			return entries[a].Adapted < entries[b].Adapted
		}
		if entries[a].Server != entries[b].Server {
			return entries[a].Server < entries[b].Server
		}
		return entries[a].Original < entries[b].Original
	})
	byName := make(map[string]int, len(entries))
	for i, entry := range entries {
		byName[entry.Adapted] = i
	}
	catalogVersion++
	return Catalog{tools: entries, byName: byName, at: time.Now(), version: catalogVersion}, quarantined
}

// Discover runs a full discovery pass over the Task's live sessions and
// publishes one immutable Catalog plus quarantine entries. It holds the Task
// mutex only to snapshot sessions, never across network calls, so listing
// RPCs run concurrently with other Task readers.
func Discover(ctx context.Context, t *Task, opts DiscoverOptions) (Catalog, []QuarantineEntry) {
	if t == nil {
		return Catalog{}, []QuarantineEntry{{Server: "<unknown>", Reason: sanitizeError(fmt.Errorf("mcp: nil task"))}}
	}
	t.mu.Lock()
	order := append([]string(nil), t.order...)
	sessions := make(map[string]ToolLister, len(t.sessions))
	for name, session := range t.sessions {
		if session != nil && !t.closed {
			sessions[name] = session
		}
	}
	t.mu.Unlock()
	if len(order) == 0 {
		return Catalog{}, nil
	}
	return DiscoverSessions(ctx, sessions, order, opts)
}

// Refresh re-runs discovery over the Task's current live sessions and
// returns a fresh immutable snapshot. It is the supported refresh path for
// tools/list-changed notifications: notification handlers (see
// NewClientWithToolListChanged) call Refresh instead of mutating any prior
// snapshot, so in-flight model turns keep their own catalog.
func (t *Task) Refresh(ctx context.Context, opts DiscoverOptions) (Catalog, []QuarantineEntry) {
	return Discover(ctx, t, opts)
}

// NewClientWithToolListChanged returns a task-scoped SDK client identical to
// NewClient except that server tools/list-changed notifications invoke
// handler. The SDK uses this single handler for both protocol eras: legacy
// servers deliver notifications/tools/list_changed, while modern servers
// (2026-07-28 with Tools.ListChanged) are subscribed via the SDK's
// subscriptions/listen handshake during Connect. Capabilities stay empty so
// no other client-initiated features are advertised.
//
// A nil handler behaves exactly like NewClient.
func NewClientWithToolListChanged(handler func(context.Context, *mcp.ToolListChangedRequest)) *SDKClient {
	if handler == nil {
		return NewClient()
	}
	return mcp.NewClient(
		&mcp.Implementation{Name: ClientName, Version: ClientVersion},
		&mcp.ClientOptions{
			Capabilities:           &mcp.ClientCapabilities{},
			ToolListChangedHandler: handler,
		},
	)
}
