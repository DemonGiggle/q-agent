package redaction

import (
	"encoding/json"
	"sort"
	"sync"
)

// dynamicSecrets is a process-wide registry for short-lived credentials
// (for example rotated ChatGPT OAuth tokens) that must be masked by every
// Policy, including Policies created before the rotation. Policies remain
// immutable; this registry is consulted on every Text call in addition to the
// per-policy literals.
var (
	dynamicMu       sync.RWMutex
	dynamicSeen     = map[string]bool{}
	dynamicLiterals []string
)

// AddSecrets registers bare secret values for all sinks. It is idempotent and
// safe for concurrent use. Empty values and Marker are ignored. Both the raw
// value and its JSON-escaped form are registered so escaping cannot hide a
// credential in tool output or persisted history.
func AddSecrets(secrets ...string) {
	dynamicMu.Lock()
	defer dynamicMu.Unlock()
	for _, secret := range secrets {
		if secret == "" || secret == Marker || dynamicSeen[secret] {
			continue
		}
		dynamicSeen[secret] = true
		dynamicLiterals = append(dynamicLiterals, secret)
		encoded, _ := json.Marshal(secret)
		if len(encoded) >= 2 {
			escaped := string(encoded[1 : len(encoded)-1])
			if escaped != secret && !dynamicSeen[escaped] {
				dynamicSeen[escaped] = true
				dynamicLiterals = append(dynamicLiterals, escaped)
			}
		}
	}
	sort.Slice(dynamicLiterals, func(i, j int) bool { return len(dynamicLiterals[i]) > len(dynamicLiterals[j]) })
}

func dynamicSnapshot() []string {
	dynamicMu.RLock()
	defer dynamicMu.RUnlock()
	if len(dynamicLiterals) == 0 {
		return nil
	}
	out := make([]string, len(dynamicLiterals))
	copy(out, dynamicLiterals)
	return out
}

// resetDynamicSecrets clears the registry. Tests only.
func resetDynamicSecrets() {
	dynamicMu.Lock()
	defer dynamicMu.Unlock()
	dynamicSeen = map[string]bool{}
	dynamicLiterals = nil
}
