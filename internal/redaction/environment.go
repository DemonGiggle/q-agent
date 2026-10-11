package redaction

import (
	"os"
	"regexp"
	"sort"
	"strings"
)

var credentialName = regexp.MustCompile(`(?i)TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|API_KEY|APIKEY`)

// FilterEnvironment returns an independently owned environment and the values
// removed from it. Configured credential names are matched case-insensitively.
func FilterEnvironment(environment []string, configured ...string) (safe, removed []string) {
	names := make(map[string]bool)
	for _, name := range append(append([]string(nil), CredentialEnvironment...), configured...) {
		if name != "" {
			names[strings.ToUpper(name)] = true
		}
	}
	safe = make([]string, 0, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if names[strings.ToUpper(name)] || credentialName.MatchString(name) {
			if value != "" {
				removed = append(removed, value)
			}
		} else {
			safe = append(safe, entry)
		}
	}
	return safe, removed
}

func EnvironmentValues() []string {
	_, values := FilterEnvironment(os.Environ())
	return values
}

// WithValues extends an immutable policy without losing its configured rules.
func (p *Policy) WithValues(values []string) *Policy {
	copy := *effective(p)
	extra, _ := New(Config{}, values)
	copy.literals = append(append([]string(nil), copy.literals...), extra.literals...)
	sort.Slice(copy.literals, func(i, j int) bool { return len(copy.literals[i]) > len(copy.literals[j]) })
	return &copy
}
