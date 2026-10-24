package runtime

import (
	"strings"
	"time"

	"q-agent/internal/platform"
)

// recentWindow parses [proactive] recent_window; empty or invalid falls back
// to the 12h default so a bad value never disables taps silently (Load
// already rejects invalid values; this is defense for tests/defaults).
func (r *Runtime) recentWindow() time.Duration {
	if d, err := time.ParseDuration(r.Config.Proactive.RecentWindow); err == nil && d > 0 {
		return d
	}
	return 12 * time.Hour
}

func (r *Runtime) recentCaps() (maxMessages, maxBytes int) {
	maxMessages, maxBytes = r.Config.Proactive.RecentMaxMessages, r.Config.Proactive.RecentMaxBytes
	if maxMessages <= 0 {
		maxMessages = 200
	}
	if maxBytes <= 0 {
		maxBytes = 32768
	}
	return maxMessages, maxBytes
}

// ObserveTapMessage records one tapped human message for the companion recent
// buffer. It is a no-op when the global kill switch is off, the route is not
// allowed, the group policy disables groups, or the chat is private (private
// history is already complete via addressed tasks, so tapping would only
// duplicate). Storage errors are reported but never fail the polling loop.
func (r *Runtime) ObserveTapMessage(m platform.Message, text string, at time.Time) {
	if !r.Config.Proactive.Enabled {
		return
	}
	if !r.allowed(m) {
		return
	}
	// Group policy off means groups are never visited; skip collection.
	if m.ChatID < 0 && r.Config.Proactive.GroupPolicy == "off" {
		return
	}
	// Private 1:1 history already contains every message (private is always
	// addressed). Tapping privates would only duplicate buildProactiveInput.
	if m.ChatID > 0 {
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	maxMessages, maxBytes := r.recentCaps()
	if err := r.Store.AppendRecent(m, at, text, maxMessages, maxBytes); err != nil {
		r.report(err)
	}
}

// consumeRecentProcessed deletes taps up to watermark after a terminal sweep
// that evaluated the input (silent, queued, or an evaluated skip). Early
// skips (busy, rate-limited, recent-activity, races) and failures keep taps
// for the next sweep. Expiry pruning (now-window) is done separately on every
// sweep that reaches input, so old taps disappear even when unprocessed.
func (r *Runtime) consumeRecentProcessed(key string, watermark time.Time) {
	if err := r.Store.ConsumeRecent(key, watermark); err != nil {
		r.report(err)
	}
}
