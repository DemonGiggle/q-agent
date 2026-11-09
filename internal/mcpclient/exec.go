// Execution dispatches registered MCP tools through the agent tool
// interface (issue #50, step 6/8 of epic #44).
//
// One Executor is bound to one immutable discovery snapshot ([Catalog])
// plus the live sessions for that agent task. It implements the
// agent.Tools execution shape — Execute(context.Context, llm.ToolCall)
// (string, error) — while retaining the existing text-returning
// interface, so the runtime wrapper (#51) can route adapted tool names
// to MCP servers and everything else to built-in tools.
//
// Exactly one tools/call attempt is made per Execute: calls are never
// replayed after an ambiguous failure (see [IsAmbiguous]), and parent
// cancellation plus the per-server call timeout are always respected.
// The client advertises no roots, sampling, elicitation, or login
// capabilities (see NewClient); a server that demands additional client
// input yields an explicit unsupported-capability error instead of a
// fulfilled round-trip.
//
// Result handling preserves the protocol distinction:
//   - A tool-level failure (CallToolResult.IsError) is model-visible
//     success: Execute returns the normalized text with an explicit
//     error marker and a nil Go error, so the model can self-correct.
//   - A protocol/transport failure (Go error from CallTool, unknown
//     tool, missing session, validation failure, unsupported input,
//     timeout/cancellation) returns ("", err); the agent loop surfaces
//     it as a failed tool call.
//
// Normalization renders text blocks, structured JSON output, and
// textual embedded resources with server + original tool provenance.
// Binary or otherwise undeliverable content (images, audio, resource
// blobs, unknown block types) is never silently dropped and never
// delivered as binary: an explicit omission marker names the type and
// size. Structured output is validated against the tool's declared
// output schema when both are present; a mismatch is an explicit
// error, never success.
//
// The returned text is redacted (Remote sink) before truncation and
// bounded to [MaxOutputBytes] with an explicit truncation marker,
// mirroring localtools.OutputLimit. The agent loop then applies its own
// Remote redaction and untrusted-result wrapping, exactly as for
// built-in tools; Executor output is plain text carrying provenance,
// not a trust envelope.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/llm"
	"q-agent/internal/redaction"
)

// MaxOutputBytes bounds normalized MCP tool output delivered to the
// model. It mirrors localtools.OutputLimit (8 KiB) so MCP and built-in
// tools share one budget; exec_test.go asserts the equality.
const MaxOutputBytes = 8 << 10

// outputTruncation marks a result cut to MaxOutputBytes. The marker is
// identical to localtools.Truncation.
const outputTruncation = "\n[output truncated]"

// omissionMarkerPrefix introduces a placeholder for content that is
// described but not delivered (binary payloads, unsupported blocks).
const omissionMarkerPrefix = "[omitted "

// Caller is the tools/call surface Execute dispatches through.
// *SDKClientSession satisfies it; tests substitute fakes. Listing,
// lifecycle, and transports are reused as-is; this interface keeps
// execution independent of connection management.
type Caller interface {
	CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// Executor dispatches one discovery snapshot's adapted tool names to
// their servers. The zero value is unusable; build with NewExecutor.
//
// Catalog is the immutable snapshot for this model turn (see
// DiscoverSessions). Sessions maps server names to live task sessions;
// it is read-only after construction and never mutated. CallTimeout
// bounds each tools/call (<=0 selects DefaultCallTimeout). Redaction
// filters model-visible text (nil selects the default policy).
type Executor struct {
	Catalog     Catalog
	Sessions    map[string]Caller
	CallTimeout time.Duration
	Redaction   *redaction.Policy
}

// NewExecutor snapshots the session map for one task turn. A nil
// session entry is dropped so dispatch treats that server as
// unavailable instead of panicking.
func NewExecutor(catalog Catalog, sessions map[string]Caller, callTimeout time.Duration, policy *redaction.Policy) *Executor {
	cp := make(map[string]Caller, len(sessions))
	for name, session := range sessions {
		if name != "" && session != nil {
			cp[name] = session
		}
	}
	return &Executor{Catalog: catalog, Sessions: cp, CallTimeout: callTimeout, Redaction: policy}
}

// Schemas exposes the snapshot's provider-ready schemas, matching the
// agent.Tools interface so an Executor can serve model requests for its
// turn. The result is a deep copy; mutating it cannot affect dispatch.
func (e *Executor) Schemas() []llm.Tool {
	if e == nil {
		return nil
	}
	return e.Catalog.Tools()
}

// policy selects the effective redaction policy.
func (e *Executor) policy() *redaction.Policy {
	if e == nil || e.Redaction == nil {
		return nil
	}
	return e.Redaction
}

// timeout selects the effective per-call bound.
func (e *Executor) timeout() time.Duration {
	if e != nil && e.CallTimeout > 0 {
		return e.CallTimeout
	}
	if d, err := time.ParseDuration(DefaultCallTimeout); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

// CallError is a bounded, provenance-carrying execution failure. Err
// preserves the underlying cause for errors.Is/As (cancellation,
// [ErrAmbiguous], connection closure); Error() stays single-line and
// truncated so a misbehaving server cannot flood diagnostics.
type CallError struct {
	Server  string
	Tool    string
	Adapted string
	Msg     string
	Err     error
}

func (e *CallError) Error() string {
	msg := strings.TrimSpace(e.Msg)
	if msg == "" {
		msg = "mcp: tool call failed"
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	const maxCallErrorLen = 500
	if len(msg) > maxCallErrorLen {
		msg = strings.TrimSpace(msg[:maxCallErrorLen]) + "…"
	}
	if msg == "" {
		msg = "mcp: tool call failed"
	}
	return msg
}

// Unwrap exposes the underlying failure.
func (e *CallError) Unwrap() error {
	if e == nil || e.Err == nil {
		return nil
	}
	return e.Err
}

// fail builds a provenance-carrying error that keeps the cause chain.
func fail(server, original, adapted, format string, args ...any) *CallError {
	return &CallError{
		Server:  sanitizeName(server),
		Tool:    sanitizeName(original),
		Adapted: sanitizeName(adapted),
		Msg:     fmt.Sprintf(format, args...),
		Err:     nil,
	}
}

// wrapFail attaches a cause while keeping the display message bounded.
func wrapFail(server, original, adapted, format string, err error, args ...any) *CallError {
	// Arguments precede the error in the display string; the raw cause
	// stays reachable via Unwrap for errors.Is (cancellation,
	// ambiguity, connection closure).
	prefix := ""
	if len(args) > 0 {
		prefix = fmt.Sprintf(format, args...)
	} else {
		prefix = format
	}
	if err != nil {
		prefix += ": " + sanitizeError(err)
	}
	return &CallError{
		Server:  sanitizeName(server),
		Tool:    sanitizeName(original),
		Adapted: sanitizeName(adapted),
		Msg:     prefix,
		Err:     err,
	}
}

// Execute runs one registered MCP tool and returns normalized,
// redacted, bounded plain text. It performs exactly one tools/call
// attempt: unknown or stale names are rejected without remapping (see
// Catalog.Resolve), and failures of unknown outcome are surfaced —
// never replayed — so a later task cannot duplicate a side effect.
//
// The text carries "[mcp server=... tool=...]" provenance on every
// path. Tool-level errors (isError) return (text, nil) with an
// explicit marker; every other failure returns ("", err).
func (e *Executor) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if e == nil {
		return "", fmt.Errorf("mcp: nil executor")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entry, err := e.Catalog.Resolve(call)
	if err != nil {
		return "", fail("", "", call.Name, "mcp: unknown or stale tool %q; refresh discovery instead of remapping", call.Name)
	}
	args, err := parseToolArguments(call.Arguments)
	if err != nil {
		return "", wrapFail(entry.Server, entry.Original, entry.Adapted, "mcp: invalid arguments for tool %q on server %q", err, entry.Adapted, entry.Server)
	}
	session, ok := e.Sessions[entry.Server]
	if !ok || session == nil {
		return "", fail(entry.Server, entry.Original, entry.Adapted, "mcp: no live session for server %q; tool %q unavailable", entry.Server, entry.Original)
	}
	callCtx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	// A single attempt: no retry loop exists anywhere in this
	// function, so ambiguous failures cannot be replayed.
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: entry.Original, Arguments: args})
	if err != nil {
		return "", mapCallError(ctx, entry, err)
	}
	if result == nil {
		return "", fail(entry.Server, entry.Original, entry.Adapted, "mcp: nil result from tool %q on server %q", entry.Original, entry.Server)
	}
	if result.NeedsInput() || len(result.InputRequests) > 0 {
		return "", unsupportedInputError(entry, result)
	}
	normalized, err := normalizeResult(entry, result)
	if err != nil {
		return "", err
	}
	return e.finalize(normalized), nil
}

// mapCallError preserves cancellation/ambiguity signals while giving
// every failure an explicit, provenance-carrying message. Server
// demands for additional client input that surface as middleware
// errors (automatic multi-round-trip fulfillment against a client
// that provides no roots/sampling/elicitation handlers) are reported
// as unsupported-capability errors, never fulfilled.
func mapCallError(parent context.Context, entry DiscoveredTool, err error) error {
	if err == nil {
		return nil
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isUnsupportedInputError(err) {
		return &CallError{
			Server:  sanitizeName(entry.Server),
			Tool:    sanitizeName(entry.Original),
			Adapted: sanitizeName(entry.Adapted),
			Msg:     fmt.Sprintf("mcp: server %q tool %q requires additional client input (roots, sampling, or elicitation); this client provides no roots, sampling, elicitation, or login flows: %s; do not retry automatically", entry.Server, entry.Original, sanitizeError(err)),
			Err:     err,
		}
	}
	note := ""
	if IsAmbiguous(err) || IsConnectionClosed(err) {
		note = "; outcome uncertain: do not replay"
	}
	return &CallError{
		Server:  sanitizeName(entry.Server),
		Tool:    sanitizeName(entry.Original),
		Adapted: sanitizeName(entry.Adapted),
		Msg:     fmt.Sprintf("mcp: tool %q on server %q failed%s: %s", entry.Original, entry.Server, note, sanitizeError(err)),
		Err:     err,
	}
}

// isUnsupportedInputError detects automatic multi-round-trip
// fulfillment failures: the server asked for roots, sampling, or
// elicitation input and this capability-free client could not
// provide it. Matching is substring-based because the SDK surfaces
// these as plain errors from its retry middleware.
func isUnsupportedInputError(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(err.Error())
	for _, marker := range []string{
		"multi-round-trip",
		"multi round trip",
		"input request",
		"inputrequest",
		"inputresponses",
		"requeststate",
		"elicit",
		"sampling",
		"createmessage",
		"listroots",
		"list roots",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// unsupportedInputError reports a modern input-required result that
// this client must not fulfill. Roots, sampling, elicitation, and
// login flows are intentionally unimplemented; the call ends here
// with an explicit error instead of a retried round-trip.
func unsupportedInputError(entry DiscoveredTool, result *mcp.CallToolResult) error {
	kinds := describeInputRequests(result.InputRequests)
	detail := "additional client input"
	if kinds != "" {
		detail = kinds
	}
	if result.NeedsInput() && kinds == "" {
		detail = "additional client input (load-shedding signal with no actionable requests)"
	}
	return fail(entry.Server, entry.Original, entry.Adapted,
		"mcp: server %q tool %q requires %s; this client provides no roots, sampling, elicitation, or login flows: call completed without fulfilling input requests; do not retry automatically",
		entry.Server, entry.Original, detail)
}

// describeInputRequests names the server's requested input kinds for
// diagnostics without echoing request payloads.
func describeInputRequests(reqs mcp.InputRequestMap) string {
	if len(reqs) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(reqs))
	seen := map[string]bool{}
	for _, req := range reqs {
		var kind string
		switch req.(type) {
		case *mcp.ElicitParams:
			kind = "elicitation"
		case *mcp.CreateMessageParams, *mcp.CreateMessageWithToolsParams:
			kind = "sampling"
		case *mcp.ListRootsParams:
			kind = "roots"
		default:
			kind = fmt.Sprintf("%T", req)
		}
		if !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}

// parseToolArguments requires a JSON object; an empty or missing value
// becomes {}. Non-object JSON and malformed input are client-side
// errors reported without contacting the server.
func parseToolArguments(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %v", err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	if obj == nil {
		return map[string]any{}, nil
	}
	return obj, nil
}

// normalizeResult renders one tools/call result as provenance-carrying
// plain text. Structured output is validated against the declared
// output schema first; validation failures are errors, never success.
// Tool-level errors (isError) keep their content and gain an explicit
// marker while returning success to the caller via Execute.
func normalizeResult(entry DiscoveredTool, result *mcp.CallToolResult) (string, error) {
	if err := validateStructuredOutput(entry, result.StructuredContent); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`[mcp server="`)
	b.WriteString(provenanceToken(entry.Server))
	b.WriteString(`" tool="`)
	b.WriteString(provenanceToken(entry.Original))
	b.WriteString(`"]`)
	b.WriteString("\n")
	if result.IsError {
		b.WriteString("tool error (isError=true): the tool reported a failure; content below is the tool's error output, not a protocol failure\n")
	}
	sections := renderContent(entry, result.Content)
	structured := renderStructured(result.StructuredContent)
	sections = append(sections, structured...)
	if len(sections) == 0 {
		sections = append(sections, "[no content]")
	}
	for _, section := range sections {
		b.WriteString(section)
		if !strings.HasSuffix(section, "\n") {
			b.WriteString("\n")
		}
	}
	out := strings.TrimRight(b.String(), "\n") + "\n"
	if result.IsError {
		// Surface the tool error even when the server returned no
		// content blocks at all.
		if strings.TrimSpace(out) == "" {
			out = "tool error (isError=true): no content returned\n"
		}
		return out, nil
	}
	return out, nil
}

// validateStructuredOutput checks server structured output against the
// tool's declared output schema. No schema or no structured content
// means nothing to check. Any failure — an unresolvable schema or a
// non-conforming value — is an explicit error; invalid output is
// never treated as success.
func validateStructuredOutput(entry DiscoveredTool, structured any) error {
	if entry.OutputSchema == nil || structured == nil {
		return nil
	}
	schemaData, err := json.Marshal(entry.OutputSchema)
	if err != nil {
		return fail(entry.Server, entry.Original, entry.Adapted,
			"mcp: tool %q on server %q declared an output schema that is not JSON-representable; cannot accept structured output",
			entry.Original, entry.Server)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		return fail(entry.Server, entry.Original, entry.Adapted,
			"mcp: tool %q on server %q declared an invalid output schema; cannot accept structured output",
			entry.Original, entry.Server)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return wrapFail(entry.Server, entry.Original, entry.Adapted,
			"mcp: tool %q on server %q declared an unresolvable output schema; structured output rejected",
			err, entry.Original, entry.Server)
	}
	// Round-trip through JSON so validation observes the wire
	// representation (numbers, nesting) rather than Go types.
	wire, err := json.Marshal(structured)
	if err != nil {
		return wrapFail(entry.Server, entry.Original, entry.Adapted,
			"mcp: tool %q on server %q returned structured output that is not JSON-representable",
			err, entry.Original, entry.Server)
	}
	var instance any
	// Plain decoding (float64 numbers) matches the SDK server-side
	// validation representation; json.Number would misreport as a
	// string under reflection-based type checks.
	if err := json.Unmarshal(wire, &instance); err != nil {
		return wrapFail(entry.Server, entry.Original, entry.Adapted,
			"mcp: tool %q on server %q returned structured output that is not valid JSON",
			err, entry.Original, entry.Server)
	}
	if err := resolved.Validate(&instance); err != nil {
		detail := sanitizeError(err)
		preview := previewJSON(wire)
		return fail(entry.Server, entry.Original, entry.Adapted,
			"mcp: tool %q on server %q returned structured output failing its declared output schema: %s; output preview: %s",
			entry.Original, entry.Server, detail, preview)
	}
	return nil
}

// previewJSON bounds offending output shown inside validation errors.
func previewJSON(wire []byte) string {
	const maxPreview = 300
	s := strings.TrimSpace(strings.ToValidUTF8(string(wire), "�"))
	if s == "" {
		return "<empty>"
	}
	if len(s) > maxPreview {
		s = strings.TrimSpace(s[:maxPreview]) + "…"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "<empty>"
	}
	return s
}

// renderContent converts each unstructured block to text, preserving
// order. Textual blocks render verbatim; binary or undeliverable
// blocks become explicit omission markers — never silent drops and
// never binary delivery.
func renderContent(entry DiscoveredTool, blocks []mcp.Content) []string {
	_ = entry
	var out []string
	for i, block := range blocks {
		out = append(out, renderBlock(i, block))
	}
	return out
}

// renderBlock describes one content block. The index keeps mixed
// results ordered and makes omission markers addressable.
func renderBlock(i int, block mcp.Content) string {
	switch c := block.(type) {
	case nil:
		return fmt.Sprintf("[block %d: %sempty content]", i, omissionMarkerPrefix)
	case *mcp.TextContent:
		if c == nil {
			return fmt.Sprintf("[block %d: %sempty content]", i, omissionMarkerPrefix)
		}
		text := strings.TrimRight(strings.ToValidUTF8(c.Text, "�"), "\n")
		if strings.TrimSpace(text) == "" {
			return fmt.Sprintf("[block %d: empty text content]", i)
		}
		return text
	case *mcp.EmbeddedResource:
		return renderEmbedded(i, c)
	case *mcp.ImageContent:
		mime := "<unknown>"
		size := 0
		if c != nil {
			if c.MIMEType != "" {
				mime = c.MIMEType
			}
			size = len(c.Data)
		}
		return fmt.Sprintf("[block %d: %sbinary image content mime=%q (%d bytes); binary payloads are not delivered to the model; ask the user for another way to share it if needed]", i, omissionMarkerPrefix, mime, size)
	case *mcp.AudioContent:
		mime := "<unknown>"
		size := 0
		if c != nil {
			if c.MIMEType != "" {
				mime = c.MIMEType
			}
			size = len(c.Data)
		}
		return fmt.Sprintf("[block %d: %sbinary audio content mime=%q (%d bytes); binary payloads are not delivered to the model; ask the user for another way to share it if needed]", i, omissionMarkerPrefix, mime, size)
	case *mcp.ResourceLink:
		if c == nil {
			return fmt.Sprintf("[block %d: %sempty resource link]", i, omissionMarkerPrefix)
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("[block %d: resource link uri=%q", i, c.URI))
		if c.Name != "" {
			fmt.Fprintf(&b, " name=%q", c.Name)
		}
		if c.MIMEType != "" {
			fmt.Fprintf(&b, " mime=%q", c.MIMEType)
		}
		if c.Description != "" {
			desc := strings.TrimSpace(strings.ToValidUTF8(c.Description, "�"))
			const maxLinkDesc = 300
			if len(desc) > maxLinkDesc {
				desc = strings.TrimSpace(desc[:maxLinkDesc]) + "…"
			}
			fmt.Fprintf(&b, " description=%q", desc)
		}
		b.WriteString("; links are references only and were not fetched]")
		return b.String()
	default:
		return fmt.Sprintf("[block %d: %sunsupported content type %T; content not delivered to the model]", i, omissionMarkerPrefix, block)
	}
}

// renderEmbedded renders an embedded resource's textual payload or an
// explicit omission marker for binary payloads. Resources are data,
// never fetched further: links stay references and blobs stay omitted.
func renderEmbedded(i int, c *mcp.EmbeddedResource) string {
	if c == nil || c.Resource == nil {
		return fmt.Sprintf("[block %d: %sempty embedded resource]", i, omissionMarkerPrefix)
	}
	res := c.Resource
	if len(res.Blob) > 0 {
		mime := res.MIMEType
		if mime == "" {
			mime = "<unknown>"
		}
		return fmt.Sprintf("[block %d: %sbinary resource uri=%q mime=%q (%d bytes); binary payloads are not delivered to the model; ask the user for another way to share it if needed]", i, omissionMarkerPrefix, res.URI, mime, len(res.Blob))
	}
	if !isTextualMime(res.MIMEType) {
		// A non-textual MIME type with no blob is still not rendered
		// as text: the server gave no textual payload to show.
		mime := res.MIMEType
		if mime == "" {
			mime = "<unknown>"
		}
		if strings.TrimSpace(res.Text) == "" {
			return fmt.Sprintf("[block %d: %snon-textual resource uri=%q mime=%q with no text payload; content not delivered to the model]", i, omissionMarkerPrefix, res.URI, mime)
		}
	}
	text := strings.TrimRight(strings.ToValidUTF8(res.Text, "�"), "\n")
	if strings.TrimSpace(text) == "" {
		return fmt.Sprintf("[block %d: empty embedded resource uri=%q]", i, res.URI)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[embedded resource uri=%q", res.URI)
	if res.MIMEType != "" {
		fmt.Fprintf(&b, " mime=%q", res.MIMEType)
	}
	b.WriteString("]\n")
	b.WriteString(text)
	return b.String()
}

// isTextualMime reports whether an embedded text payload may render.
// Empty MIME types default to text: servers commonly omit them for
// plain-text resources.
func isTextualMime(mime string) bool {
	mime = strings.TrimSpace(strings.ToLower(mime))
	if mime == "" {
		return true
	}
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/javascript", "application/x-json":
		return true
	}
	if strings.HasSuffix(mime, "+json") || strings.HasSuffix(mime, "+xml") {
		return true
	}
	return false
}

// renderStructured appends the canonical JSON rendering of validated
// structured output. Unstructured blocks stay first so ordering is
// stable; structured content is always labeled as JSON.
func renderStructured(structured any) []string {
	if structured == nil {
		return nil
	}
	wire, err := json.Marshal(structured)
	if err != nil {
		return []string{fmt.Sprintf("[%sstructured output that is not JSON-representable; content not delivered]", omissionMarkerPrefix)}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, wire); err != nil {
		return []string{fmt.Sprintf("[%sstructured output that is not valid JSON; content not delivered]", omissionMarkerPrefix)}
	}
	return []string{"[structured output (JSON)]\n" + compact.String()}
}

// finalize redacts secrets (Remote sink) before truncation so cutting
// a token cannot evade redaction, then bounds the text to
// MaxOutputBytes with an explicit marker. It mirrors the agent loop's
// clean-then-clip order for built-in tools.
func (e *Executor) finalize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	// A nil policy selects redaction defaults (see effective): method
	// calls on a nil *Policy are safe by design.
	s = e.policy().Text(redaction.Remote, s)
	return limitOutput(s)
}

// limitOutput bounds text to MaxOutputBytes, cutting on a UTF-8
// boundary and appending an explicit truncation marker.
func limitOutput(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	n := MaxOutputBytes - len(outputTruncation)
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n] + outputTruncation
}

// provenanceToken keeps header values single-line and bounded so a
// hostile server name cannot smuggle layout past the model.
func provenanceToken(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	s = strings.ReplaceAll(s, `"`, "'")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const maxProvenanceToken = 128
	if len(s) > maxProvenanceToken {
		s = strings.TrimSpace(s[:maxProvenanceToken]) + "…"
	}
	if s == "" {
		return "<unknown>"
	}
	return s
}
