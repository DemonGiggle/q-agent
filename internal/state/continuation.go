package state

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Interrupted records are deliberately smaller than the whole history budget,
// so one large failed task cannot evict its own original request.
const MaxInterruptedPairBytes = 16 << 10
const interruptedOmission = "\n[context omitted to fit history budget]\n"

// fitInterruptedText budgets JSON-encoded bytes (including escapes), retaining
// the head and tail on UTF-8 boundaries. Never truncate serialized JSON itself.
func fitInterruptedText(s string, budget int) string {
	s = strings.ToValidUTF8(s, "�")
	size := func(text string) int { b, _ := json.Marshal(text); return len(b) }
	if size(s) <= budget {
		return s
	}
	candidate := func(n int) string {
		head := UTF8Prefix(s, n/2)
		start := len(s) - (n - n/2)
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		return head + interruptedOmission + s[start:]
	}
	low, high := 0, len(s)
	for low < high {
		mid := (low + high + 1) / 2
		if size(candidate(mid)) <= budget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return candidate(low)
}

func boundInterrupted(p Pair) Pair {
	p.User = fitInterruptedText(p.User, 4<<10)
	p.Answer = fitInterruptedText(p.Answer, 2<<10)
	if len(p.Steers) > 0 {
		p.Steers = []string{fitInterruptedText(strings.Join(p.Steers, "\n\n"), 3<<10)}
	}
	p.Progress = fitInterruptedText(p.Progress, 6<<10)
	return p
}
