package runtime

import (
	"fmt"
	"runtime"
	"testing"
)

func TestParseRSS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   uint64
		bad    bool
	}{
		{"resident not virtual or peak", "VmPeak:\t99999999 kB\nVmSize:\t88888888 kB\nVmHWM:\t65536 kB\nVmRSS:\t12345 kB\nRssAnon:\t8192 kB\n", 12345 * 1024, false},
		{"whitespace", "Name: q-agent\n  VmRSS:   1536\tkB\n", 1536 * 1024, false},
		{"zero", "VmRSS: 0 kB\n", 0, false},
		{"missing", "VmSize: 1048576 kB\nVmHWM: 2048 kB\n", 0, true},
		{"empty", "", 0, true},
		{"negative", "VmRSS: -1 kB", 0, true},
		{"not a number", "VmRSS: unknown kB", 0, true},
		{"missing value", "VmRSS:", 0, true},
		{"missing unit", "VmRSS: 1024", 0, true},
		{"wrong unit", "VmRSS: 1024 MB", 0, true},
		{"extra fields", "VmRSS: 1024 kB extra", 0, true},
		{"parse overflow", "VmRSS: 18446744073709551616 kB", 0, true},
		{"byte overflow", "VmRSS: 18446744073709551615 kB", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRSS(tc.status)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("parseRSS = %d, %v; want %d, error=%t", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestFormatRSS(t *testing.T) {
	if got := formatRSS(12345*1024, nil); got != "12.1 MiB" {
		t.Fatalf("wrong binary units: %q", got)
	}
	if got := formatRSS(0, nil); got != "0.0 MiB" {
		t.Fatalf("zero RSS: %q", got)
	}
	if got := formatRSS(12345, fmt.Errorf("open /private/proc/status: permission denied")); got != "Unavailable" {
		t.Fatalf("read failure should not expose a path or a memory value: %q", got)
	}
}

func TestCurrentRSS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RSS is supplied by Linux procfs")
	}
	bytes, err := currentRSS()
	if err != nil || bytes == 0 {
		t.Fatalf("current process RSS = %d, %v", bytes, err)
	}
	t.Logf("current test process RSS: %d bytes (%s)", bytes, formatRSS(bytes, nil))
}
