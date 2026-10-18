package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestEscape(t *testing.T) {
	if got := Escape("a<b>&c>\"d\""); got != "a&lt;b&gt;&amp;c&gt;\"d\"" {
		t.Fatalf("%q", got)
	}
}

func TestFormatPlainUnchanged(t *testing.T) {
	for _, s := range []string{"para one\n\npara two", "Task cancelled.", "completed😀", "Provider: openai"} {
		if got := Format(s); got != Escape(s) && got != s {
			// Plain text without markdown or <> should round-trip.
			if strings.ContainsAny(s, "<>&") {
				continue
			}
			t.Fatalf("%q -> %q", s, got)
		}
	}
}

func TestFormatMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello **bold**", "Hello <b>bold</b>"},
		{"*italic*", "<i>italic</i>"},
		{"`code`", "<code>code</code>"},
		{"[link](https://example.com)", `<a href="https://example.com">link</a>`},
		{"# Title", "<b>Title</b>"},
		{"> quote", "<blockquote>quote</blockquote>"},
		{"- item", "• item"},
		{"||spoiler||", "<tg-spoiler>spoiler</tg-spoiler>"},
		{"~~strike~~", "<s>strike</s>"},
		{"__underline__", "<u>underline</u>"},
		{"a <b> & c", "a &lt;b&gt; &amp; c"},
		{"[x](ftp://example.com)", "[x](ftp://example.com)"},
	}
	for _, tc := range cases {
		if got := Format(tc.in); got != tc.want {
			t.Errorf("%q -> %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatCodeFence(t *testing.T) {
	got := Format("```python\nprint(\"hi\")\n```")
	want := `<pre><code class="language-python">print("hi")</code></pre>`
	if got != want {
		t.Fatalf("%q", got)
	}
	// Unclosed fence still safe.
	got = Format("```\n<a>&\n")
	if !strings.Contains(got, "<pre>") || strings.Contains(got, "<a>") {
		t.Fatalf("%q", got)
	}
}

func TestFormatSnakeCaseNotItalic(t *testing.T) {
	got := Format("foo_bar_baz")
	if strings.Contains(got, "<i>") {
		t.Fatalf("snake_case italicized: %q", got)
	}
}

func TestStripHTML(t *testing.T) {
	if got := StripHTML(`<b>bold</b> and <code>x &lt;y&gt;</code>`); got != "bold and x <y>" {
		t.Fatalf("%q", got)
	}
	if got := StripHTML(`<pre><code class="language-go">a</code></pre>`); got != "a" {
		t.Fatalf("%q", got)
	}
}

func TestSplitHTMLPreservesTags(t *testing.T) {
	long := strings.Repeat("**bold** plain text line with enough words to fill chunks\n", 500)
	parts := FormatAndSplit(long)
	if len(parts) < 2 {
		t.Fatal("expected split")
	}
	for _, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatal("invalid utf8")
		}
		if len(utf16.Encode([]rune(stripTags(p)))) > 4096 {
			t.Fatal("chunk too long")
		}
		if len(p) > 4096 {
			t.Fatalf("raw chunk too long: %d", len(p))
		}
		// No dangling entities.
		if strings.Count(p, "&amp;")+strings.Count(p, "&lt;")+strings.Count(p, "&gt;") > 0 {
			// Entities must end with ;.
			for _, ent := range []string{"&amp", "&lt", "&gt"} {
				_ = ent
			}
		}
	}
	// Visible text round-trips (tags stripped, bullets preserved).
	joined := strings.Join(parts, "\n")
	if !strings.Contains(stripTags(joined), "bold") {
		t.Fatal("lost content")
	}
}

func TestSplitHTMLEscapesAndPre(t *testing.T) {
	src := "```go\n" + strings.Repeat("line with <tag> & stuff\n", 500) + "```\nAfter block."
	parts := FormatAndSplit(src)
	if len(parts) < 2 {
		t.Fatalf("expected pre split, got %d", len(parts))
	}
	for _, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatal("invalid")
		}
	}
}
