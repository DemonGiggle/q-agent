package redaction

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// InteractiveMask preserves byte offsets so that redaction precedes pagination
// without changing history cursors. Input must have terminal decoration removed.
// Newlines are ignored during detection, covering terminal-wrapped credentials.
// Incomplete known values and token prefixes are masked before a prompt is read.
func (p *Policy) InteractiveMask(text string) string {
	p = effective(p)
	if !p.Enabled(Remote) {
		return text
	}
	var compact strings.Builder
	positions := make([]int, 0, len(text))
	for i := 0; i < len(text); i++ {
		if text[i] != '\n' && text[i] != '\r' {
			compact.WriteByte(text[i])
			positions = append(positions, i)
		}
	}
	s := compact.String()
	masked := []byte(text)
	mask := func(start, end int) {
		for _, at := range positions[start:end] {
			masked[at] = '*'
		}
	}
	patterns := append([]*regexp.Regexp{interactiveTokens, bearer, p.assignment, pemBlock, cardCandidate}, p.patterns...)
	for _, re := range patterns {
		for _, span := range re.FindAllStringIndex(s, -1) {
			if re == interactiveTokens && strings.HasPrefix(strings.ToLower(s[span[0]:span[1]]), "sk-") && span[0] > 0 {
				previous := s[span[0]-1]
				if (previous >= 'a' && previous <= 'z' || previous >= 'A' && previous <= 'Z' || previous >= '0' && previous <= '9' || previous == '_') && positions[span[0]] == positions[span[0]-1]+1 {
					continue
				}
			}
			mask(span[0], span[1])
		}
	}
	for _, literal := range p.literals {
		for offset := 0; offset < len(s); {
			i := strings.Index(s[offset:], literal)
			if i < 0 {
				break
			}
			i += offset
			mask(i, i+len(literal))
			offset = i + len(literal)
		}
		// Echoed input or a redraw can split a known credential into separate
		// visible pieces. Conservatively mask overlapping four-byte pieces of
		// longer configured values; keep these extra rules local to terminals.
		if len(literal) >= 8 {
			seen := map[string]bool{}
			for at := 0; at+4 <= len(literal); at++ {
				piece := literal[at : at+4]
				if seen[piece] {
					continue
				}
				seen[piece] = true
				for offset := 0; offset < len(s); {
					i := strings.Index(s[offset:], piece)
					if i < 0 {
						break
					}
					i += offset
					mask(i, i+4)
					offset = i + 1
				}
			}
		}
		// Even a partial exact value must not escape in an interim read.
		for n := min(len(literal)-1, len(s)); n > 0; n-- {
			if strings.HasSuffix(s, literal[:n]) {
				mask(len(s)-n, len(s))
				break
			}
		}
	}
	// A byte-oriented fragment match must not leave half a UTF-8 rune exposed.
	for i := range text {
		_, size := utf8.DecodeRuneInString(text[i:])
		if size < 1 {
			size = 1
		}
		if string(masked[i:i+size]) != text[i:i+size] {
			for n := i; n < i+size; n++ {
				masked[n] = '*'
			}
		}
	}
	return string(masked)
}

var interactiveTokens = regexp.MustCompile(`(?i)(?:(?:sk-|gh[pousr]_|github_pat_|AKIA)[a-z0-9_-]*|Bearer[ \t]+[a-z0-9._~+/=-]*)`)
var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
