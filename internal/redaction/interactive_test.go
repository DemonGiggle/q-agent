package redaction

import (
	"strings"
	"testing"
)

func TestInteractiveMaskPreservesCursorsAcrossFragmentsAndWraps(t *testing.T) {
	secret := "configured-sensitive-value"
	p, _ := New(Config{}, []string{secret})
	for _, s := range []string{"configured-", "configured-sensitive-value", "configured-\nsensitive-value", "configured-go\r\nsensitive-value", "previous\nghp_abcdefghijklmnopqrstu123456789\n", "ghp_abcd\nefghijklmno", "password=hunter2", "Bearer livecredential"} {
		safe := p.InteractiveMask(s)
		if len(safe) != len(s) {
			t.Fatal("redaction moved history cursor")
		}
		if strings.Contains(safe, "configured-") || strings.Contains(safe, "sensitive-value") || strings.Contains(safe, "ghp_") || strings.Contains(safe, "hunter2") || strings.Contains(safe, "livecredential") {
			t.Fatal("credential exposed: " + safe)
		}
	}
	if s := "Open https://github.com/login/device with ABCD-1234 for task-scoped authorization"; p.InteractiveMask(s) != s {
		t.Fatal("authorization instructions hidden")
	}
}
