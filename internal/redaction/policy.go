// Package redaction filters copies of text crossing local output boundaries.
// Policies are immutable and safe to share. Streams belong to one writer.
package redaction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const Marker = "[REDACTED]"

type Sink uint8

const (
	Terminal Sink = iota
	Persistence
	Exports
	JSONEvents
	Remote
)

type Config struct {
	Terminal        *bool    `toml:"terminal"`
	Persistence     *bool    `toml:"persistence"`
	Exports         *bool    `toml:"exports"`
	JSONEvents      *bool    `toml:"json_events"`
	Remote          *bool    `toml:"remote"`
	CustomPatterns  []string `toml:"custom_patterns"`
	SensitivePaths  []string `toml:"sensitive_paths"`
	SensitiveFields []string `toml:"sensitive_fields"`
}

type Policy struct {
	enabled    [5]bool
	patterns   []*regexp.Regexp
	literals   []string
	fields     map[string]bool
	assignment *regexp.Regexp
}

var tokens = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]{16,}|gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|AKIA[A-Z0-9]{16})\b`)
var bearer = regexp.MustCompile(`(?i)\bBearer[ \t]+[a-z0-9._~+/=-]+`)
var pemBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
var pemEnd = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
var builtinFields = []string{"api_key", "apikey", "password", "passwd", "secret", "access_token", "refresh_token", "auth_token", "token", "authorization", "client_secret", "private_key"}
var defaultPolicy, _ = New(Config{}, nil)

// CredentialEnvironment lists supported credential sources. Values are never logged.
var CredentialEnvironment = []string{"TELEGRAM_BOT_TOKEN", "Q_AGENT_API_KEY", "QCODE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_API_KEY", "OPENCODE_GO_API_KEY", "GITHUB_TOKEN", "GH_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GOOGLE_API_KEY", "GEMINI_API_KEY", "BRAVE_API_KEY", "TAVILY_API_KEY", "SERPER_API_KEY"}

func EnvironmentValues() []string {
	var values []string
	for _, name := range CredentialEnvironment {
		if value := os.Getenv(name); value != "" {
			values = append(values, value)
		}
	}
	return values
}

type InvalidPatternError struct{ Index int }

func (e *InvalidPatternError) Error() string {
	return fmt.Sprintf("redaction.custom_patterns[%d]: invalid regular expression", e.Index)
}

// Validate reports only the setting and index, never the regex or compiler error.
func Validate(c Config) error {
	for i, pattern := range c.CustomPatterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return &InvalidPatternError{Index: i}
		}
	}
	return nil
}

func New(c Config, exactValues []string) (*Policy, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	p := &Policy{enabled: [5]bool{true, true, true, true, true}, fields: map[string]bool{}}
	for i, value := range []*bool{c.Terminal, c.Persistence, c.Exports, c.JSONEvents, c.Remote} {
		if value != nil {
			p.enabled[i] = *value
		}
	}
	for _, pattern := range c.CustomPatterns {
		p.patterns = append(p.patterns, regexp.MustCompile(pattern))
	}
	names := append(append([]string(nil), builtinFields...), c.SensitiveFields...)
	var assignments []string
	for _, name := range names {
		p.fields[normalize(name)] = true
		if name != "" {
			assignments = append(assignments, regexp.QuoteMeta(name))
		}
	}
	for _, name := range paymentFields {
		p.fields[normalize(name)] = true
	}
	// Retain the learning detector's spelling variants, including API KEY.
	assignments = append(assignments, `api[_ -]?key`, `access[_ -]?token`, `client[_ -]?secret`, `private[_ -]?key`, `refresh[_ -]?token`)
	p.assignment = regexp.MustCompile(`(?i)(?:["']?(?:\b|_)(?:` + strings.Join(assignments, "|") + `)["']?[ \t]*[:=][ \t]*)(?:(?:\[REDACTED\])|"(?:\\.|[^"\\])*"?|'(?:\\.|[^'\\])*'?|[^\s"'<>;,}\]]+)`)
	literals := append([]string(nil), exactValues...)
	for _, path := range c.SensitivePaths {
		literals = append(literals, path)
		expanded := os.ExpandEnv(path)
		if expanded == "~" || strings.HasPrefix(expanded, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				if expanded == "~" {
					expanded = home
				} else {
					expanded = filepath.Join(home, expanded[2:])
				}
			}
		}
		literals = append(literals, expanded)
	}
	seen := map[string]bool{}
	for _, value := range literals {
		if value != "" && value != Marker && !seen[value] {
			p.literals = append(p.literals, value)
			seen[value] = true
			encoded, _ := json.Marshal(value)
			escaped := string(encoded[1 : len(encoded)-1])
			if escaped != value && !seen[escaped] {
				p.literals = append(p.literals, escaped)
				seen[escaped] = true
			}
		}
	}
	sort.Slice(p.literals, func(i, j int) bool { return len(p.literals[i]) > len(p.literals[j]) })
	return p, nil
}
func effective(p *Policy) *Policy {
	if p == nil {
		return defaultPolicy
	}
	return p
}
func (p *Policy) Enabled(s Sink) bool { return effective(p).enabled[s] }
func normalize(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
}
func (p *Policy) SensitiveField(s string) bool { return effective(p).fields[normalize(s)] }

// protect makes repeated filtering idempotent even with broad custom expressions.
func protect(s string, filter func(string) string) string {
	parts := strings.Split(s, Marker)
	for i := range parts {
		if parts[i] != "" {
			parts[i] = filter(parts[i])
		}
	}
	return strings.Join(parts, Marker)
}
func (p *Policy) line(line string) string {
	p = effective(p)
	line = redactPayments(line)
	// Match assignments before replacing individual values to preserve syntax on reruns.
	line = p.assignment.ReplaceAllStringFunc(line, func(match string) string {
		separator := strings.IndexAny(match, ":=")
		prefix, value := match[:separator+1], strings.TrimSpace(match[separator+1:])
		if value == Marker || value == `"`+Marker+`"` || value == "'"+Marker+"'" {
			return match
		}
		if strings.HasPrefix(value, `"`) {
			return prefix + ` "` + Marker + `"`
		}
		if strings.HasPrefix(value, "'") {
			return prefix + " '" + Marker + "'"
		}
		return prefix + " " + Marker
	})
	return protect(line, func(part string) string {
		for _, literal := range p.literals {
			part = protect(part, func(piece string) string { return strings.ReplaceAll(piece, literal, Marker) })
		}
		part = tokens.ReplaceAllString(part, Marker)
		part = bearer.ReplaceAllString(part, Marker)
		for _, pattern := range p.patterns {
			part = protect(part, func(piece string) string { return pattern.ReplaceAllString(piece, Marker) })
		}
		return part
	})
}
func (p *Policy) Text(s Sink, text string) string {
	if !p.Enabled(s) {
		return text
	}
	stream := p.Stream(s)
	return stream.Feed(text) + stream.Flush()
}

// Redact applies detection regardless of sink switches, for learning review.
func (p *Policy) Redact(text string) string {
	detector := *effective(p)
	detector.enabled[Persistence] = true
	return detector.Text(Persistence, text)
}
func (p *Policy) ContainsSecret(text string) bool { return p.Redact(text) != text }

// JSON decodes before matching, so escaping cannot hide a credential. Malformed
// tool arguments remain text; valid JSON remains valid JSON.
func (p *Policy) JSON(s Sink, data []byte) []byte { return p.filterJSON(s, data, false, false) }

// Replay preserves opaque encrypted material and image payloads while filtering
// inspectable provider text and JSON-encoded function arguments.
func (p *Policy) Replay(s Sink, data []byte) []byte { return p.filterJSON(s, data, true, true) }
func (p *Policy) filterJSON(s Sink, data []byte, transport, opaque bool) []byte {
	if !p.Enabled(s) || len(data) == 0 {
		return append([]byte(nil), data...)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if json.Valid(data) && decoder.Decode(&value) == nil {
		filtered, _ := json.Marshal(p.value(s, value, transport, opaque))
		return filtered
	}
	return []byte(p.Text(s, string(data)))
}

// Value sanitizes JSON-shaped values without changing keys or routing identifiers.
func (p *Policy) Value(s Sink, value any) any { return p.value(s, value, true, false) }
func (p *Policy) value(s Sink, value any, transport, opaque bool) any {
	if !p.Enabled(s) {
		return value
	}
	switch v := value.(type) {
	case json.Number:
		// Ordinary JSON may encode a PAN as a number. Keep numeric transport
		// metadata intact, since typed counters and identifiers must round-trip.
		if !transport && cardCandidate.FindString(v.String()) == v.String() && luhn(v.String()) {
			return Marker
		}
		return value
	case nil, bool, float64, float32, int, int64, uint64:
		return value
	case string:
		return p.Text(s, v)
	case json.RawMessage:
		return json.RawMessage(p.JSON(s, v))
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = p.value(s, item, transport, opaque)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			if opaque && opaqueField(key) {
				result[key] = item
			} else if transport && preserved(key) {
				result[key] = item
			} else if p.SensitiveField(key) && item != nil {
				result[key] = Marker

			} else if strings.EqualFold(key, "arguments") {
				if text, ok := item.(string); ok {
					result[key] = string(p.JSON(s, []byte(text)))
				} else {
					result[key] = p.maskValue(item, opaque)
				}
			} else {
				result[key] = p.value(s, item, transport, opaque)
			}
		}
		return result
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return value
		}
		var decoded any
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if d.Decode(&decoded) != nil {
			return value
		}
		return p.value(s, decoded, transport, opaque)
	}
}

// Routing, images and opaque replay material are outside the text guarantee.
func preserved(key string) bool {
	switch normalize(key) {
	case "id", "agentid", "requestid", "taskid", "parenttaskid", "activetaskid", "targetid", "interactionid", "toolcallid", "callid", "replaces", "replacedby", "active", "role", "name", "model", "models", "provider", "status", "state", "kind", "intent", "source", "type", "created", "createdat", "saved", "left", "time", "started", "startedat", "finished", "updated":
		return true
	}
	return false
}

// Copy makes a typed, independently owned copy suitable for a boundary. Owners
// must separately decode byte-backed text and retain operational metadata.
func Copy[T any](p *Policy, s Sink, value T) T {
	data, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(p.filterJSON(s, data, true, false), &result)
	return result
}

func opaqueField(key string) bool {
	switch normalize(key) {
	case "encryptedcontent", "signature", "data", "images", "imageurl":
		return true
	}
	return false
}

func (p *Policy) maskValue(value any, opaque bool) any {
	switch v := value.(type) {
	case string:
		return Marker
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = p.maskValue(item, opaque)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			if preserved(key) || opaque && opaqueField(key) {
				result[key] = item
			} else {
				result[key] = p.maskValue(item, opaque)
			}
		}
		return result
	default:
		return value
	}
}
