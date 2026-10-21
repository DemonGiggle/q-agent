package process

import (
	"fmt"
	"strings"
)

// Keys translates terminal key names. Pipe sessions may use the same byte
// sequences; Ctrl-D is a terminal EOF key, not a close of a pipe's stdin.
func Keys(keys []string) (string, error) {
	if len(keys) == 0 || len(keys) > 128 {
		return "", fmt.Errorf("keys requires 1..128 named keys")
	}
	named := map[string]string{"enter": "\r", "tab": "\t", "escape": "\x1b", "esc": "\x1b", "backspace": "\x7f", "space": " ", "up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D", "home": "\x1b[H", "end": "\x1b[F", "delete": "\x1b[3~", "pageup": "\x1b[5~", "pagedown": "\x1b[6~", "f1": "\x1bOP", "f2": "\x1bOQ", "f3": "\x1bOR", "f4": "\x1bOS"}
	var b strings.Builder
	for _, key := range keys {
		k := strings.ToLower(key)
		if s, ok := named[k]; ok {
			b.WriteString(s)
			continue
		}
		if len(k) == 6 && strings.HasPrefix(k, "ctrl-") && k[5] >= 'a' && k[5] <= 'z' {
			b.WriteByte(k[5] - 'a' + 1)
			continue
		}
		return "", fmt.Errorf("unknown terminal key %q", key)
	}
	return b.String(), nil
}
