package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/telegram"
)

// liveSink is the Telegram live-preview capability. *telegram.Client
// implements it; tests may substitute a fake. Discovery is via type
// assertion so existing Sink fakes without live methods keep working.
type liveSink interface {
	SendLive(ctx context.Context, m platform.Message, text string) (int64, error)
	EditLive(ctx context.Context, m platform.Message, messageID int64, text string) error
	DeleteLive(ctx context.Context, m platform.Message, messageID int64) error
}

const (
	liveThrottle     = 1200 * time.Millisecond
	livePreviewLimit = 3800 // UTF-16 code units, safely under Telegram's 4096
	liveTimeout      = 10 * time.Second
)

// liveReporter shows best-effort progress while the durable outbox remains
// the authoritative delivery path. All network errors disable further live
// updates; the run continues and the final answer is still queued durably.
type liveReporter struct {
	sink   liveSink
	route  platform.Message
	policy *redaction.Policy

	mu       sync.Mutex
	id       int64
	sent     bool
	disabled bool
	lastEdit time.Time
	lastText string
	failures int
	status   string
	stream   strings.Builder
}

func newLiveReporter(sink Sink, route platform.Message, policy *redaction.Policy) *liveReporter {
	if sink == nil {
		return &liveReporter{disabled: true}
	}
	ls, ok := sink.(liveSink)
	if !ok {
		return &liveReporter{disabled: true}
	}
	return &liveReporter{sink: ls, route: route, policy: policy}
}

func truncateLive(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	u := utf16.Encode([]rune(s))
	if len(u) <= limit {
		return s
	}
	// Avoid splitting a surrogate pair.
	n := limit
	for n > 0 && (u[n] >= 0xD800 && u[n] <= 0xDBFF) {
		n--
	}
	return string(utf16.Decode(u[:n])) + "…"
}

func (l *liveReporter) renderLocked() string {
	stream := l.stream.String()
	if stream != "" && l.policy != nil {
		stream = l.policy.Text(redaction.Remote, stream)
	} else if stream != "" {
		// Nil-safe: default policy still redacts bearer-style secrets.
		var nilPolicy *redaction.Policy
		stream = nilPolicy.Text(redaction.Remote, stream)
	}
	stream = truncateLive(stream, livePreviewLimit-len(l.status)-2)
	if l.status != "" && stream != "" {
		return l.status + "\n\n" + stream
	}
	if stream != "" {
		return stream
	}
	if l.status != "" {
		return l.status
	}
	return "Working…"
}

func (l *liveReporter) ensureLocked() bool {
	if l.disabled || l.sent {
		return l.sent && !l.disabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	text := l.renderLocked()
	if text == "" {
		text = "Working…"
	}
	id, err := l.sink.SendLive(ctx, l.route, text)
	if err != nil {
		var api *telegram.APIError
		if ok := errorAsAPI(err, &api); ok && api != nil && api.RetryAfter > 0 {
			// Rate limited on first send: skip live for this run.
			l.disabled = true
			return false
		}
		l.disabled = true
		return false
	}
	l.id = id
	l.sent = true
	l.lastEdit = time.Now()
	l.lastText = text
	if id == 0 {
		// Boolean test stub: count as sent but disable edits.
		l.disabled = true
		return false
	}
	return true
}

func errorAsAPI(err error, target **telegram.APIError) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}

func (l *liveReporter) editLocked(force bool) {
	if l.disabled || !l.sent || l.id == 0 {
		return
	}
	now := time.Now()
	if !force && now.Sub(l.lastEdit) < liveThrottle {
		return
	}
	text := l.renderLocked()
	if text == l.lastText {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	err := l.sink.EditLive(ctx, l.route, l.id, text)
	if err == nil {
		l.lastText = text
		l.lastEdit = now
		l.failures = 0
		return
	}
	var api *telegram.APIError
	if errorAsAPI(err, &api) && api != nil && api.RetryAfter > 0 {
		l.lastEdit = now
		return
	}
	l.failures++
	if l.failures >= 3 {
		l.disabled = true
	}
}

// SetStatus replaces the progress header and flushes throttled.
func (l *liveReporter) SetStatus(status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled && !l.sent {
		// Still allow first send attempt.
		if l.sink == nil {
			return
		}
	}
	l.status = status
	if !l.ensureLocked() {
		return
	}
	l.editLocked(false)
}

// ToolStart announces a tool; count tracks tools used so far.
func (l *liveReporter) ToolStart(name string, count int) {
	status := "Working… 🔧 " + name
	if count > 0 {
		status += shortCount(count)
	}
	l.SetStatus(status)
}

// ToolEnd refreshes the header after a tool finishes.
func (l *liveReporter) ToolEnd(name string, count int, err error) {
	status := "Working… ✅ " + name
	if err != nil {
		status = "Working… ⚠️ " + name
	}
	if count > 0 {
		status += shortCount(count)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = status
	if !l.ensureLocked() {
		return
	}
	l.editLocked(false)
}

func shortCount(n int) string {
	if n <= 1 {
		return ""
	}
	return " (" + itoa(n) + " tools)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// StreamEvent appends final-answer preview text. Thinking events are ignored;
// tool-call turns rarely carry content, so forwarding everything harmless.
func (l *liveReporter) StreamEvent(ev llm.StreamEvent) {
	if ev.Kind != llm.StreamOutput || ev.Text == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Detached from run cancellation: best-effort preview.
	l.stream.WriteString(ev.Text)
	if !l.ensureLocked() {
		return
	}
	// Paragraph boundaries are natural flush points; still throttled to
	// respect Telegram edit limits.
	l.editLocked(false)
}

// Flush forces a preview edit, used after streaming completes.
func (l *liveReporter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ensureLocked() {
		return
	}
	l.editLocked(true)
}

// FinishSuccess removes the preview so the durable reply is the single
// authoritative answer. Best effort; failures are ignored.
func (l *liveReporter) FinishSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.sent || l.id == 0 || l.disabled && l.id == 0 {
		return
	}
	if l.sink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	_ = l.sink.DeleteLive(ctx, l.route, l.id)
}

// FinishError leaves a terminal status when the run failed after the preview
// was already shown. The durable queue still delivers the same message.
func (l *liveReporter) FinishError(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.sent || l.id == 0 {
		return
	}
	if l.disabled {
		return
	}
	text = truncateLive(text, livePreviewLimit)
	if text == "" {
		return
	}
	l.status = ""
	l.stream.Reset()
	l.stream.WriteString(text)
	l.editLocked(true)
}
