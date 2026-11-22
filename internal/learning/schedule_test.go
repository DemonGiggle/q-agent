package learning

import (
	"testing"
	"time"
)

func TestLearningWindowFollowsLocalCalendar(t *testing.T) {
	schedule := ScheduleConfig{Enabled: true, TimeZone: "Asia/Taipei", WindowStart: "22:00", WindowEnd: "03:00"}
	for _, tc := range []struct {
		utc, window string
	}{
		{"2026-11-22T13:59:59Z", ""},
		{"2026-11-22T14:00:00Z", "2026-11-22"},
		{"2026-11-22T16:00:00Z", "2026-11-22"},
		{"2026-11-22T18:59:59Z", "2026-11-22"},
		{"2026-11-22T19:00:00Z", ""},
	} {
		now, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := schedule.Window(now); got != tc.window {
			t.Errorf("window at %s = %q, want %q", tc.utc, got, tc.window)
		}
	}
	schedule.TimeZone, schedule.WindowStart, schedule.WindowEnd = "America/New_York", "01:00", "03:00"
	for _, stamp := range []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"} {
		now, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			t.Fatal(err)
		}
		if got := schedule.Window(now); got != "2026-11-01" {
			t.Errorf("window at %s = %q", stamp, got)
		}
	}
}
