package learning

import (
	"time"
)

// Window returns the local date the current window opened. The date
// stays the same across midnight and a repeated daylight-saving hour.
func (s ScheduleConfig) Window(now time.Time) string {
	if !s.Enabled {
		return ""
	}
	zone, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return ""
	}
	local := now.In(zone)
	clock := local.Format("15:04")
	if s.WindowStart < s.WindowEnd {
		if clock >= s.WindowStart && clock < s.WindowEnd {
			return local.Format(time.DateOnly)
		}
		return ""
	}
	if clock >= s.WindowStart {
		return local.Format(time.DateOnly)
	}
	if clock < s.WindowEnd {
		return local.AddDate(0, 0, -1).Format(time.DateOnly)
	}
	return ""
}
