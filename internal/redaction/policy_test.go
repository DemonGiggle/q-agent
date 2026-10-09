package redaction

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCredentialFormsAndIdempotence(t *testing.T) {
	p, _ := New(Config{}, nil)
	for _, input := range []string{
		"sk-abcdefghijklmnopqrst", "ghp_abcdefghijklmnopqrstuv", "github_pat_abcdefghijklmnopqrstuv", "AKIA1234567890ABCDEF",
		"api_key = abcdef1234567890", "DB_PASSWORD = bare-credential-value", `"api_key": "quoted credential"`, "'password'='quoted credential'", "API KEY:abcdef1234567890",
		"Authorization: Bearer abcdef.123456", "Bearer short", "password=short", "access-token=abcdefghijk", "secret=abc",
		"-----BEGIN RSA PRIVATE KEY-----\nprivate material\n-----END RSA PRIVATE KEY-----\npublic text",
	} {
		t.Run(input, func(t *testing.T) {
			result := p.Text(Terminal, input)
			if result == input || !strings.Contains(result, Marker) {
				t.Fatalf("not filtered: %q", result)
			}
			if second := p.Text(Terminal, result); second != result {
				t.Fatalf("not idempotent: %q -> %q", result, second)
			}
			if strings.Contains(result, "private material") {
				t.Fatal("PEM payload leaked")
			}
		})
	}
	if result := p.Text(Terminal, "public data and monkey=hello"); result != "public data and monkey=hello" {
		t.Fatalf("false positive: %q", result)
	}
}
func TestCustomPolicyAndSinkOverrides(t *testing.T) {
	disabled := false
	t.Setenv("QCODE_TEST_SECRET_DIR", "/private/credentials")
	p, err := New(Config{Terminal: &disabled, CustomPatterns: []string{`CUSTOM-[0-9]+`}, SensitivePaths: []string{"$QCODE_TEST_SECRET_DIR"}, SensitiveFields: []string{"client_credential"}}, []string{"raw-exact-secret"})
	if err != nil {
		t.Fatal(err)
	}
	input := "CUSTOM-42 raw-exact-secret $QCODE_TEST_SECRET_DIR /private/credentials"
	if p.Text(Terminal, input) != input {
		t.Fatal("disabled terminal filtered")
	}
	for _, sink := range []Sink{Persistence, Exports, JSONEvents, Remote} {
		result := p.Text(sink, input)
		if strings.Contains(result, "CUSTOM") || strings.Contains(result, "raw-exact") || strings.Contains(result, "credentials") {
			t.Fatalf("sink %d: %q", sink, result)
		}
	}
	// Patterns operate on individual lines, even with dotall enabled.
	cross, _ := New(Config{CustomPatterns: []string{`(?s)first.*last`}}, nil)
	if cross.Text(Persistence, "first\nlast") != "first\nlast" {
		t.Fatal("custom rule crossed a line")
	}
	if !p.ContainsSecret("raw-exact-secret") {
		t.Fatal("detection depends on display switch")
	}
}
func TestJSONEscapingFieldsAndOpaqueReplay(t *testing.T) {
	p, _ := New(Config{SensitiveFields: []string{"client_credential"}}, []string{"private-exact"})
	input := []byte(`{"password":"short","nested":{"client_credential":"anything","text":"private\u002dexact"},"arguments":"{\"password\":\"raw-secret\"}","encrypted_content":"private-exact","id":"private-exact","number":42,"ok":true}`)
	result := p.Replay(Persistence, input)
	if !json.Valid(result) {
		t.Fatalf("invalid JSON: %s", result)
	}
	var value map[string]any
	if err := json.Unmarshal(result, &value); err != nil {
		t.Fatal(err)
	}
	if value["password"] != Marker || value["encrypted_content"] != "private-exact" || value["id"] != "private-exact" {
		t.Fatal(value)
	}
	nested := value["nested"].(map[string]any)
	if nested["text"] != Marker || nested["client_credential"] != Marker || strings.Contains(value["arguments"].(string), "raw-secret") {
		t.Fatal(value)
	}
	if second := p.Replay(Persistence, result); string(second) != string(result) {
		t.Fatalf("JSON not idempotent: %s", second)
	}
	if strings.Contains(string(p.JSON(Persistence, []byte(`{"password":"unfinished-secret`))), "unfinished-secret") {
		t.Fatal("malformed arguments leaked")
	}
}
func TestInvalidRegexDoesNotEchoPattern(t *testing.T) {
	_, err := New(Config{CustomPatterns: []string{"fine", "private-regex["}}, nil)
	if err == nil || !strings.Contains(err.Error(), "redaction.custom_patterns[1]") || strings.Contains(err.Error(), "private-regex") {
		t.Fatalf("diagnostic: %v", err)
	}
}
func TestStreamingAtEveryBoundary(t *testing.T) {
	p, _ := New(Config{}, []string{"exact-secret"})
	for _, input := range []string{
		"before sk-abcdefghijklmnopqrst after\r\nlast exact-secret",
		"Bearer abcdef.123456\npassword=quoted-secret",
		"-----BEGIN PRIVATE KEY-----\r\nprivate body\r\n-----END PRIVATE KEY-----\r\nlast",
		`{"password":"escaped secret"}` + "\n",
	} {
		want := p.Text(Terminal, input)
		for split := 0; split <= len(input); split++ {
			stream := p.Stream(Terminal)
			got := stream.Feed(input[:split]) + stream.Feed(input[split:]) + stream.Flush()
			if got != want {
				t.Fatalf("split %d: %q != %q", split, got, want)
			}
		}
		stream := p.Stream(Terminal)
		var got strings.Builder
		for _, b := range []byte(input) {
			got.WriteString(stream.Feed(string(b)))
		}
		got.WriteString(stream.Flush())
		if got.String() != want {
			t.Fatal("single-byte chunks differ")
		}
	}
	stream := p.Stream(Terminal)
	if stream.Feed("exact-secret") != "" {
		t.Fatal("emitted unfinished line")
	}
	if stream.Flush() != Marker {
		t.Fatal("failed final flush")
	}
}
func TestStreamOverflowAndPEMTransitions(t *testing.T) {
	var p *Policy
	stream := p.Stream(Terminal)
	if stream.Feed(strings.Repeat("x", MaxPendingLine)+"secret") != "" || stream.pending != "" {
		t.Fatal("overflow retained content")
	}
	if got := stream.Feed("more\nsafe\n"); got != Marker+"\nsafe\n" {
		t.Fatalf("overflow: %q", got)
	}
	stream = p.Stream(Terminal)
	if got := stream.Feed("-----BEGIN PRIVATE KEY-----\nbody"); got != Marker+"\n" {
		t.Fatal(got)
	}
	if stream.Flush() != Marker {
		t.Fatal("PEM flush leaked")
	}
	if got := stream.Feed("more body\n-----END PRIVATE KEY-----\nafter\n"); got != Marker+"\n"+Marker+"\nafter\n" {
		t.Fatal(got)
	}
}

func TestArgumentNamesAndDataAreNotOpaque(t *testing.T) {
	p, _ := New(Config{}, []string{"literal-credential"})
	result := p.JSON(Terminal, []byte(`{"name":"literal-credential","data":"literal-credential","encrypted_content":"literal-credential"}`))
	if strings.Contains(string(result), "literal-credential") {
		t.Fatal("ordinary JSON fields were treated as replay metadata")
	}
}

func TestPEMOverflowDelimitersAndMultipleBlocks(t *testing.T) {
	var p *Policy
	block := "-----BEGIN PRIVATE KEY-----one-----END PRIVATE KEY-----"
	if got := p.Text(Terminal, "prefix "+block+" middle "+block+" suffix"); got != "prefix "+Marker+" middle "+Marker+" suffix" {
		t.Fatalf("multiple PEM blocks: %q", got)
	}
	stream := p.Stream(Terminal)
	stream.Feed(strings.Repeat("x", MaxPendingLine) + "-----BEGIN PRIV")
	got := stream.Feed("ATE KEY-----\nPEM body\n-----END PRIVATE KEY-----\nsafe\n")
	if got != Marker+"\n"+Marker+"\n"+Marker+"\nsafe\n" {
		t.Fatal("overflow lost PEM state")
	}
	p, _ = New(Config{}, []string{`secret-with-"quote`})
	if got := p.Text(Terminal, `provider failed: {"error":"secret-with-\"quote"}`); strings.Contains(got, "secret-with") {
		t.Fatal("escaped diagnostic leaked")
	}
}
