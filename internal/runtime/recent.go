package runtime

import (
	"strings"
	"time"

	"q-agent/internal/learning"
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

// ObserveTapMessage stores authorized human group messages for the enabled
// consumers, including messages that never result in an interactive task.
func (r *Runtime) ObserveTapMessage(m platform.Message, text string, at time.Time) {
	if m.ChatID >= 0 || !r.allowed(m) || strings.TrimSpace(text) == "" {
		return
	}
	companion := r.Config.Proactive.Enabled && r.Config.Proactive.GroupPolicy != "off"
	cfg := r.Learning
	learn := r.Config.Memory.Enabled && cfg.Enabled && cfg.Permissions.IncludeGroupChats &&
		!cfg.Permissions.ExcludesTopic(m.ChatID, m.TopicID) &&
		!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
	if !companion && !learn {
		return
	}
	maxMessages, maxBytes := r.recentCaps()
	if err := r.Store.AppendRecentFor(m, at, text, maxMessages, maxBytes, companion, learn); err != nil {
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
