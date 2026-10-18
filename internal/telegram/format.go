package telegram

import (
	"regexp"
	"strings"
)

// HTML formatting for Telegram Bot API (parse_mode=HTML).
//
// Supported tags (Bot API "HTML style"): b, strong, i, em, u, ins,
// s, strike, del, tg-spoiler (or span class="tg-spoiler"), a href,
// code, pre (+ nested code class="language-xxx"), blockquote
// (+ expandable), tg-emoji. There are deliberately no headings,
// tables, lists or images: those markdown constructs are converted
// to bold text, bullet lines and pre blocks.
//
// MarkdownV2 could express the same entities but requires escaping
// 18 reserved characters anywhere outside markup; a single unescaped
// "." or "-" fails the whole sendMessage call. HTML mode only needs
// &, <, > escaped, so it is the safer choice for generated text.

const (
	// VisibleTextLimit is a conservative bound under Telegram's 4096
	// character text limit (counted after entities parsing, i.e. tags
	// excluded, UTF-16 code units). Raw JSON with tags may be longer.
	VisibleTextLimit = 4000
	// maxRawLen keeps the raw HTML payload (tags included) under
	// Telegram's 4096-character request limit even for heavily
	// formatted messages, leaving room for closing tags on split.
	maxRawLen = 3900
)

// Escape escapes plain text for use inside an HTML parse_mode message.
// Only &, <, > need escaping; quotes are left alone for readability
// except inside attribute values (handled by escapeAttr).
func Escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func escapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// StripHTML renders formatted HTML back to readable plain text. Used as
// the fallback payload when Telegram rejects HTML with
// "can't parse entities": tags are removed so users never see raw
// markup, entities are decoded, and block boundaries keep newlines.
func StripHTML(htmlText string) string {
	var b strings.Builder
	inTag := false
	for i := 0; i < len(htmlText); {
		c := htmlText[i]
		if !inTag && c == '<' {
			// Find tag end; if none, treat rest as text.
			if end := strings.IndexByte(htmlText[i:], '>'); end >= 0 {
				tag := htmlText[i : i+end+1]
				// Keep block structure readable.
				name := tagName(tag)
				if name == "pre" || name == "blockquote" || name == "br" || name == "p" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
				}
				i += end + 1
				continue
			}
		}
		if c == '&' {
			if semi := strings.IndexByte(htmlText[i:], ';'); semi > 0 && semi <= 10 {
				entity := htmlText[i : i+semi+1]
				switch entity {
				case "&amp;":
					b.WriteByte('&')
					i += len(entity)
					continue
				case "&lt;":
					b.WriteByte('<')
					i += len(entity)
					continue
				case "&gt;":
					b.WriteByte('>')
					i += len(entity)
					continue
				case "&quot;":
					b.WriteByte('"')
					i += len(entity)
					continue
				}
			}
		}
		b.WriteByte(c)
		i++
	}
	out := strings.ReplaceAll(b.String(), "\r\n", "\n")
	// Collapse 3+ newlines left by block closings.
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "(No answer text returned.)"
	}
	return out
}

var (
	fenceLangRe  = regexp.MustCompile(`^[A-Za-z0-9_+\-#]+$`)
	linkRe       = regexp.MustCompile(`\[([^\[\]\n]+?)\]\(([^)\s]+?)\)`)
	boldRe       = regexp.MustCompile(`(?s)\*\*(.+?)\*\*`)
	underlineRe  = regexp.MustCompile(`(?s)__(.+?)__`)
	strikeRe     = regexp.MustCompile(`(?s)~~(.+?)~~`)
	strike1Re    = regexp.MustCompile(`(?s)~([^~\n]+?)~`)
	spoilerRe    = regexp.MustCompile(`(?s)\|\|(.+?)\|\|`)
	italicStar   = regexp.MustCompile(`\*([^*\n]+?)\*`)
	italicUnd    = regexp.MustCompile(`(^|[\s\(\[{<])_([^_\n]+?)_([\s\)\].,;:!?>/]|$)`)
	headingRe    = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	listRe       = regexp.MustCompile(`^(\s*)(?:[-*•]|(\d+)[.)])\s+(.+)$`)
	hrRe         = regexp.MustCompile(`^\s*(---|\*\*\*|___)\s*$`)
	inlineCodeRe = regexp.MustCompile("`([^`\n]+?)`")
)

// Format converts lightweight (GitHub-style) markdown to Telegram HTML
// (parse_mode=HTML). Plain text without markdown passes through
// unchanged except for &, <, > escaping, so existing callers and tests
// keep working. Code fences, inline code, bold, italic, underline,
// strikethrough, spoiler, links, headings (-> bold), blockquotes,
// lists (-> • bullets) and horizontal rules are rendered with the
// supported tag set. Tables have no Telegram entity and are left as
// plain monospaced-readable text.
func Format(src string) string {
	if strings.TrimSpace(src) == "" {
		return "(No answer text returned.)"
	}
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")

	type segment struct {
		pre  string // already-formatted <pre> HTML
		text string // markdown to format
	}
	var segments []segment
	var cur strings.Builder
	lines := strings.Split(src, "\n")
	i := 0
	flush := func() {
		if cur.Len() > 0 {
			segments = append(segments, segment{text: cur.String()})
			cur.Reset()
		}
	}
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") {
			flush()
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			// Language may be "python ..." trailing info; keep first token.
			if fields := strings.Fields(lang); len(fields) > 0 {
				lang = fields[0]
			}
			var code []string
			i++
			closed := false
			for i < len(lines) {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
					closed = true
					i++
					break
				}
				code = append(code, lines[i])
				i++
			}
			_ = closed
			body := Escape(strings.Join(code, "\n"))
			// Preserve empty code blocks as visible empty pre.
			if strings.TrimSpace(strings.Join(code, "\n")) == "" {
				body = " "
			}
			if lang != "" && fenceLangRe.MatchString(lang) {
				segments = append(segments, segment{pre: `<pre><code class="language-` + escapeAttr(lang) + `">` + body + "</code></pre>"})
			} else {
				segments = append(segments, segment{pre: "<pre>" + body + "</pre>"})
			}
			continue
		}
		cur.WriteString(lines[i])
		cur.WriteByte('\n')
		i++
	}
	flush()

	var out strings.Builder
	for si, seg := range segments {
		if seg.pre != "" {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
				out.WriteByte('\n')
			}
			out.WriteString(seg.pre)
			out.WriteByte('\n')
			continue
		}
		if si > 0 && out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(formatTextBlock(seg.text))
	}
	result := strings.TrimRight(out.String(), "\n")
	if strings.TrimSpace(result) == "" {
		return "(No answer text returned.)"
	}
	return result
}

// formatTextBlock formats a non-fence markdown segment (already split
// from fences). It extracts inline-code spans first so their content
// is never interpreted as markup, escapes the rest, then applies
// block structure and inline styles.
func formatTextBlock(text string) string {
	// Extract inline code spans.
	var codes []string
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := inlineCodeRe.FindStringSubmatch(m)[1]
		codes = append(codes, "<code>"+Escape(inner)+"</code>")
		return "\x00CODE" + itoaHTML(len(codes)-1) + "\x00"
	})

	escaped := Escape(text)

	// Links first so later inline passes can style link text.
	escaped = linkRe.ReplaceAllStringFunc(escaped, func(m string) string {
		parts := linkRe.FindStringSubmatch(m)
		if len(parts) != 3 {
			return m
		}
		label, rawURL := parts[1], parts[2]
		url := rawURL
		// Unescape entities that Escape introduced inside the URL
		// (& -> &amp;) so href stays valid.
		url = strings.ReplaceAll(url, "&amp;", "&")
		url = strings.ReplaceAll(url, "&lt;", "<")
		url = strings.ReplaceAll(url, "&gt;", ">")
		url = strings.ReplaceAll(url, "&quot;", `"`)
		lower := strings.ToLower(url)
		if !(strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "tg://")) {
			return m // leave as plain text, don't create exotic schemes
		}
		if strings.Contains(url, `"`) || strings.Contains(url, "<") || strings.Contains(url, ">") || strings.ContainsAny(url, " \n\r\t") {
			return m
		}
		return `<a href="` + escapeAttr(url) + `">` + label + `</a>`
	})

	// Block structure: group consecutive blockquotes.
	lines := strings.Split(escaped, "\n")
	// Restore inline-code placeholders per line happens later in inline();
	// blockquote detection must work with placeholders present (they contain no '>').
	var formatted []string
	j := 0
	for j < len(lines) {
		line := lines[j]
		trimmed := strings.TrimSpace(line)
		// Inline-code placeholder lines that exactly fill the line are
		// normal lines; fences are already separate segments.
		if hrRe.MatchString(line) {
			formatted = append(formatted, "────────")
			j++
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			formatted = append(formatted, "<b>"+inlineLine(m[2])+"</b>")
			j++
			continue
		}
		// Blockquote group.
		if isQuoteLine(line) {
			var inner []string
			for j < len(lines) && isQuoteLine(lines[j]) {
				content := stripQuotePrefix(lines[j])
				if strings.TrimSpace(content) == "" {
					inner = append(inner, "")
				} else {
					inner = append(inner, inlineLine(content))
				}
				j++
			}
			// Trim leading/trailing blank quote lines.
			for len(inner) > 0 && strings.TrimSpace(stripTags(inner[0])) == "" {
				inner = inner[1:]
			}
			for len(inner) > 0 && strings.TrimSpace(stripTags(inner[len(inner)-1])) == "" {
				inner = inner[:len(inner)-1]
			}
			if len(inner) == 0 {
				continue
			}
			formatted = append(formatted, "<blockquote>"+strings.Join(inner, "\n")+"</blockquote>")
			continue
		}
		if m := listRe.FindStringSubmatch(line); m != nil {
			indent, body := m[1], m[3]
			// Keep nesting with two-space indent, marker becomes •.
			if len(indent) > 6 {
				indent = indent[:6]
			}
			formatted = append(formatted, indent+"• "+inlineLine(body))
			j++
			continue
		}
		_ = trimmed
		formatted = append(formatted, inlineLine(line))
		j++
	}
	joined := strings.Join(formatted, "\n")

	// Restore inline code placeholders.
	for idx, code := range codes {
		joined = strings.ReplaceAll(joined, "\x00CODE"+itoaHTML(idx)+"\x00", code)
	}
	// Any unmatched placeholder (shouldn't happen) is dropped.
	joined = strings.ReplaceAll(joined, "\x00CODE", "")
	joined = strings.ReplaceAll(joined, "\x00", "")
	return joined
}

func isQuoteLine(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, "&gt;") || strings.HasPrefix(t, ">")
}

func stripQuotePrefix(line string) string {
	t := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(t, "&gt;") {
		rest := strings.TrimPrefix(t, "&gt;")
		if strings.HasPrefix(rest, " ") {
			rest = rest[1:]
		}
		return rest
	}
	rest := strings.TrimPrefix(t, ">")
	if strings.HasPrefix(rest, " ") {
		rest = rest[1:]
	}
	return rest
}

// inlineLine applies inline styles to a single already-escaped line.
func inlineLine(s string) string {
	// Spoiler first (||…||) so inner * etc. still get styled.
	s = spoilerRe.ReplaceAllString(s, `<tg-spoiler>$1</tg-spoiler>`)
	s = boldRe.ReplaceAllString(s, `<b>$1</b>`)
	s = underlineRe.ReplaceAllString(s, `<u>$1</u>`)
	s = strikeRe.ReplaceAllString(s, `<s>$1</s>`)
	// Single-tilde strikethrough only when not part of ~~ (already handled).
	s = strike1Re.ReplaceAllString(s, `<s>$1</s>`)
	s = italicStar.ReplaceAllString(s, `<i>$1</i>`)
	s = italicUnd.ReplaceAllStringFunc(s, func(m string) string {
		parts := italicUnd.FindStringSubmatch(m)
		if len(parts) != 4 {
			return m
		}
		return parts[1] + `<i>` + parts[2] + `</i>` + parts[3]
	})
	return s
}

func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			inTag = true
			continue
		}
		if s[i] == '>' && inTag {
			inTag = false
			continue
		}
		if !inTag {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func itoaHTML(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// visibleLen counts UTF-16 code units of visible text: tags removed,
// HTML entities count as one unit.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '<' {
			if end := strings.IndexByte(s[i:], '>'); end >= 0 {
				i += end + 1
				continue
			}
		}
		if s[i] == '&' {
			if semi := strings.IndexByte(s[i:], ';'); semi > 0 && semi <= 10 {
				i += semi + 1
				n++
				continue
			}
		}
		// Decode one rune for surrogate counting.
		c := s[i]
		var width int
		var size int
		if c < 0x80 {
			width, size = 1, 1
		} else {
			r := rune(0)
			// Manual UTF-8 decode for width; fallback to 1 on invalid.
			if c>>5 == 0x6 && i+1 < len(s) {
				r = rune(c&0x1F)<<6 | rune(s[i+1]&0x3F)
				size = 2
			} else if c>>4 == 0xE && i+2 < len(s) {
				r = rune(c&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
				size = 3
			} else if c>>3 == 0x1E && i+3 < len(s) {
				r = rune(c&0x07)<<18 | rune(s[i+1]&0x3F)<<12 | rune(s[i+2]&0x3F)<<6 | rune(s[i+3]&0x3F)
				size = 4
			} else {
				size = 1
			}
			if r > 0xFFFF {
				width = 2
			} else {
				width = 1
			}
		}
		n += width
		i += size
	}
	return n
}

// FormatAndSplit formats markdown to HTML and splits into
// Telegram-sized chunks, each self-contained valid HTML.
func FormatAndSplit(src string) []string {
	return SplitHTML(Format(src))
}

// SplitHTML splits already-formatted HTML on visible-text length,
// keeping tags valid: never cuts inside <...> or &...;, closes open
// tags at each chunk end and reopens them in the next chunk. Newlines
// are preferred cut points.
func SplitHTML(htmlText string) []string {
	if strings.TrimSpace(stripTags(htmlText)) == "" && strings.TrimSpace(htmlText) != "" {
		// Pure tags? Return as-is.
		return []string{htmlText}
	}
	if htmlText == "" {
		htmlText = "(No answer text returned.)"
	}
	if visibleLen(htmlText) <= VisibleTextLimit && len(htmlText) <= 4096 {
		return []string{htmlText}
	}
	type openTag struct {
		name string
		full string
	}
	var chunks []string
	var cur strings.Builder
	var stack []openTag
	visible := 0
	lastNewlineRaw := -1
	lastNewlineVisible := 0
	var lastNewlineStack []openTag

	flushAt := func(cutRaw int, reopen []openTag) string {
		chunk := cur.String()[:cutRaw]
		// Close open tags.
		for k := len(reopen) - 1; k >= 0; k-- {
			chunk += "</" + reopen[k].name + ">"
		}
		return chunk
	}

	// We build cur incrementally and cut when visible exceeds limit.
	// Track raw positions: since cur is a Builder we track its string length.
	pushNewlineMark := func() {
		lastNewlineRaw = cur.Len()
		lastNewlineVisible = visible
		lastNewlineStack = append([]openTag(nil), stack...)
	}

	i := 0
	chunkStartVisible := 0
	chunkStartRaw := 0
	for i < len(htmlText) {
		if visible-chunkStartVisible >= VisibleTextLimit || cur.Len()-chunkStartRaw >= maxRawLen {
			// Need to cut.
			cutRaw := cur.Len()
			cutStack := append([]openTag(nil), stack...)
			if lastNewlineRaw >= 0 && lastNewlineVisible-chunkStartVisible > VisibleTextLimit/2 {
				// Cut at last newline: rebuild chunk up to mark.
				full := cur.String()
				// full length == cur.Len(); lastNewlineRaw is offset inside it.
				chunk := full[:lastNewlineRaw]
				for k := len(lastNewlineStack) - 1; k >= 0; k-- {
					chunk += "</" + lastNewlineStack[k].name + ">"
				}
				chunks = append(chunks, chunk)
				// Start new chunk: reopen tags + remainder after newline.
				remainder := full[lastNewlineRaw:]
				cur.Reset()
				stack = append([]openTag(nil), lastNewlineStack...)
				for _, t := range stack {
					cur.WriteString(t.full)
				}
				cur.WriteString(remainder)
				chunkStartVisible = lastNewlineVisible
				chunkStartRaw = 0
				_ = cutRaw
				_ = cutStack
				lastNewlineRaw = -1
				// Re-evaluate limit without advancing i (i already consumed).
				// Avoid infinite loop: if remainder alone exceeds limit, fall through to hard cut.
				if visible-chunkStartVisible < VisibleTextLimit && cur.Len() < maxRawLen {
					continue
				}
			}
			// Hard cut here.
			chunk := flushAt(cur.Len(), stack)
			chunks = append(chunks, chunk)
			cur.Reset()
			for _, t := range stack {
				cur.WriteString(t.full)
			}
			chunkStartVisible = visible
			chunkStartRaw = 0
			lastNewlineRaw = -1
			continue
		}
		c := htmlText[i]
		if c == '<' {
			end := strings.IndexByte(htmlText[i:], '>')
			if end < 0 {
				// Treat as text.
				cur.WriteByte(c)
				visible++
				i++
				continue
			}
			tag := htmlText[i : i+end+1]
			name, closing, selfClosing := parseTag(tag)
			cur.WriteString(tag)
			if !closing && !selfClosing && name != "" {
				stack = append(stack, openTag{name: name, full: tag})
			} else if closing && len(stack) > 0 {
				// Pop matching (search from top).
				for k := len(stack) - 1; k >= 0; k-- {
					if stack[k].name == name {
						stack = stack[:k]
						break
					}
				}
			}
			i += end + 1
			continue
		}
		if c == '&' {
			semi := strings.IndexByte(htmlText[i:], ';')
			if semi > 0 && semi <= 10 {
				entity := htmlText[i : i+semi+1]
				cur.WriteString(entity)
				visible++
				i += semi + 1
				if entity == "\n" {
					pushNewlineMark()
				}
				continue
			}
			cur.WriteByte(c)
			visible++
			i++
			continue
		}
		// Regular rune: decode UTF-8 length.
		size := 1
		width := 1
		if c >= 0x80 {
			// Determine rune length.
			if c>>5 == 0x6 && i+1 < len(htmlText) {
				size = 2
				width = 1
			} else if c>>4 == 0xE && i+2 < len(htmlText) {
				size = 3
				width = 1
			} else if c>>3 == 0x1E && i+3 < len(htmlText) {
				// Check if astral (4-byte => surrogate pair).
				size = 4
				width = 2
			} else {
				size = 1
				width = 1
			}
			// Validate astral by decoding first byte range.
			if size == 4 {
				// All 4-byte runes are astral.
				width = 2
			}
		}
		if i+size > len(htmlText) {
			size = len(htmlText) - i
		}
		chunk := htmlText[i : i+size]
		cur.WriteString(chunk)
		visible += width
		if chunk == "\n" {
			pushNewlineMark()
		}
		i += size
		_ = flushAt
	}
	rest := cur.String()
	// Close any remaining opens? No: last chunk should keep original closings
	// already present in input; but if input was truncated mid-tag stack
	// (unbalanced input), close them. Our Format output is balanced except
	// when we reopened: closings are in input. The stack at end reflects
	// tags opened but never closed in input (should be empty). Close if needed.
	for k := len(stack) - 1; k >= 0; k-- {
		// Only close if the original didn't already (stack non-empty means unbalanced).
		rest += "</" + stack[k].name + ">"
	}
	chunks = append(chunks, rest)
	// Drop empty trailing chunks from newline cuts.
	var nonEmpty []string
	for _, ch := range chunks {
		if strings.TrimSpace(stripTags(ch)) == "" {
			// Keep if it carries a <pre> with whitespace? Drop pure empties.
			continue
		}
		nonEmpty = append(nonEmpty, ch)
	}
	if len(nonEmpty) == 0 {
		return []string{htmlText}
	}
	return nonEmpty
}

func parseTag(tag string) (name string, closing bool, selfClosing bool) {
	inner := strings.TrimSpace(tag)
	if len(inner) < 2 || inner[0] != '<' || inner[len(inner)-1] != '>' {
		return "", false, false
	}
	inner = inner[1 : len(inner)-1]
	inner = strings.TrimSpace(inner)
	if strings.HasPrefix(inner, "/") {
		closing = true
		inner = strings.TrimSpace(inner[1:])
	}
	if strings.HasSuffix(inner, "/") {
		selfClosing = true
		inner = strings.TrimSpace(inner[:len(inner)-1])
	}
	if inner == "" {
		return "", closing, selfClosing
	}
	// Name up to space or /.
	end := len(inner)
	for k, ch := range inner {
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '/' {
			end = k
			break
		}
	}
	name = strings.ToLower(inner[:end])
	// Normalize aliases to canonical closing names? Keep as written so
	// open/close match; stack matches exact lowercased names.
	switch name {
	case "br", "hr", "img":
		selfClosing = true
	}
	return name, closing, selfClosing
}

func tagName(tag string) string {
	name, _, _ := parseTag(tag)
	return name
}
