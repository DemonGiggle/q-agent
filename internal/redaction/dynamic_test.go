package redaction

import (
	"strings"
	"testing"
)

func TestDynamicSecretsMaskBareValuesOnAllSinks(t *testing.T) {
	resetDynamicSecrets()
	p, _ := New(Config{}, nil)
	// Policy created before rotation must still mask the rotated secret.
	rotated := "rotated-bare-jwt-value-xyz-12345"
	AddSecrets(rotated)
	for _, sink := range []Sink{Terminal, Persistence, Exports, JSONEvents, Remote} {
		got := p.Text(sink, "leaked "+rotated+" tail")
		if strings.Contains(got, rotated) || !strings.Contains(got, Marker) {
			t.Fatalf("sink %d did not mask rotated secret: %q", sink, got)
		}
	}
	// JSON escaping must not hide the secret either.
	p2, _ := New(Config{}, nil)
	AddSecrets("quote\"secret")
	got := p2.Text(Remote, `{"x":"quote\"secret"}`)
	if strings.Contains(got, "quote") && strings.Contains(got, "secret") {
		// The raw secret with a quote must still be masked via escaped form.
		t.Fatalf("escaped secret not masked: %q", got)
	}
	resetDynamicSecrets()
}
