package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

const digestTranscriptCap = 20000
const fallbackMemoryContextBytes = 2000

type digestRetry struct {
	epoch uint64
	retry state.MemoryRetry
}

func memoryLocation(cfg string) *time.Location {
	if cfg != "" {
		if loc, err := time.LoadLocation(cfg); err == nil {
			return loc
		}
	}
	return time.Local
}

func untilNextDigest(now time.Time, loc *time.Location, hour, min int) time.Duration {
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, min, 0, 0, loc)
	if !next.After(local) {
		next = time.Date(local.Year(), local.Month(), local.Day()+1, hour, min, 0, 0, loc)
	}
	// time.Date normalizes month overflow (Day+1) and DST gaps/skips; a
	// skipped wall time may resolve off by an hour. The digest loop's minute
	// tick re-arms pendingSweep when now passes next, so DST only shifts one
	// sweep rather than skipping it.
	return next.Sub(local)
}

func digestRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		return time.Hour
	}
	delay := time.Minute * time.Duration(1<<(attempt-1))
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

// Nightly sweeps enroll pending work; completion wakes only resume
// enrolled work. One worker visits each chat once per round for fair batching.
func (r *Runtime) digestSweep(ctx context.Context) error { return r.digestWork(ctx, true) }

func (r *Runtime) digestWork(ctx context.Context, sweep bool) error {
	// Disabled memory retains notes/pending for /memory, /forget, and a
	// later re-enable; it only stops new journaling (see run) and sweeps.
	if !r.Config.Memory.Enabled {
		return nil
	}
	r.digestMu.Lock()
	defer r.digestMu.Unlock()
	for {
		progress, err := r.digestPass(ctx, sweep)
		if err != nil {
			return err
		}
		if !progress {
			return nil
		}
		sweep = false
	}
}

func (r *Runtime) rememberDigestRetry(key string, epoch uint64, retry state.MemoryRetry) error {
	r.mu.Lock()
	if r.digestRetries == nil {
		r.digestRetries = map[string]digestRetry{}
	}
	r.digestRetries[key] = digestRetry{epoch: epoch, retry: retry}
	r.mu.Unlock()
	err := r.Store.RetryMemory(key, epoch, retry)
	if err != nil && !errors.Is(err, state.ErrStaleDigest) {
		r.report(err)
	}
	return err
}

func (r *Runtime) digestPass(ctx context.Context, sweep bool) (bool, error) {
	chats, err := r.Store.ListChats()
	if err != nil {
		return false, err
	}
	progress := false
	for _, chat := range chats {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		mem, loadErr := r.Store.SnapshotDigest(chat.Key)
		r.mu.Lock()
		local, hasLocal := r.digestRetries[chat.Key]
		_, busy := r.active[chat.Key]
		maintenance := r.Maintenance != nil && r.Maintenance.Busy()
		r.mu.Unlock()
		retry := mem.Retry
		if hasLocal && (loadErr != nil || local.epoch == 0 || local.epoch == mem.Epoch) {
			retry = local.retry
		}
		if loadErr != nil {
			// Keep storage failures scheduled even when their retry state cannot write.
			if hasLocal && time.Now().Before(retry.NextAt) {
				continue
			}
			retry.Work = true
			retry.Attempts++
			retry.NextAt = time.Now().Add(digestRetryDelay(retry.Attempts))
			r.rememberDigestRetry(chat.Key, local.epoch, retry)
			r.report(loadErr)
			continue
		}
		if mem.LegacyBaseline && !mem.MigrationReported {
			r.report(r.Store.ReportMemoryMigration(chat.Key, chat.WorkspaceRoute()))
		}
		if sweep && (len(mem.Pending) > 0 || busy || maintenance) {
			retry.Work = true
		}
		if len(mem.Pending) == 0 {
			if busy || maintenance {
				if retry.Work {
					retry.Deferred = true
					r.rememberDigestRetry(chat.Key, mem.Epoch, retry)
				}
			} else {
				if retry != (state.MemoryRetry{}) || mem.Retry != (state.MemoryRetry{}) {
					r.rememberDigestRetry(chat.Key, mem.Epoch, state.MemoryRetry{})
				}
				r.mu.Lock()
				delete(r.digestRetries, chat.Key)
				r.mu.Unlock()
			}
			continue
		}
		if !retry.Work || time.Now().Before(retry.NextAt) {
			continue
		}
		if busy || maintenance {
			retry.Deferred = true
			r.rememberDigestRetry(chat.Key, mem.Epoch, retry)
			continue
		}
		if !mem.Retry.Work {
			// Enroll the round durably before model work. If capacity eviction
			// invalidates the result, the remaining sources still need a retry.
			if err := r.rememberDigestRetry(chat.Key, mem.Epoch, retry); err != nil {
				if !errors.Is(err, state.ErrStaleDigest) {
					retry.Attempts++
					retry.NextAt = time.Now().Add(digestRetryDelay(retry.Attempts))
					r.rememberDigestRetry(chat.Key, mem.Epoch, retry)
				}
				continue
			}
		}
		err := r.digestChat(ctx, chat.Key, chat.Record)
		if err != nil {
			if errors.Is(err, state.ErrStaleDigest) {
				r.mu.Lock()
				delete(r.digestRetries, chat.Key)
				r.mu.Unlock()
				// Another commit/forget/eviction changed the snapshot. Revisit fairly.
				progress = true
				continue
			}
			retry.Work, retry.Deferred = true, false
			retry.Attempts++
			retry.NextAt = time.Now().Add(digestRetryDelay(retry.Attempts))
			r.rememberDigestRetry(chat.Key, mem.Epoch, retry)
			r.report(err)
			r.logf("[MEMORY] Digest failed; retry scheduled | workspace=%s attempt=%d retry_at=%s error=%q", chat.Key, retry.Attempts, retry.NextAt.Format(time.RFC3339), r.Options.Redaction.Text(redaction.Terminal, err.Error()))
			continue
		}
		r.mu.Lock()
		delete(r.digestRetries, chat.Key)
		r.mu.Unlock()
		progress = true
	}
	return progress, nil
}

// splitDigestFragments splits text into labeled fragments, each within cap
// bytes. It always makes progress, even for oversized labels or multi-byte
// runes wider than the remaining budget, so callers cannot loop forever.
func splitDigestFragments(text string, labelFor func(n int) string, cap int) []string {
	var fragments []string
	for len(text) > 0 {
		label := labelFor(len(fragments) + 1)
		avail := cap - len(label)
		if avail < 4 {
			// Reserve room for one rune; labels are small in practice.
			label = state.UTF8Prefix(label, cap-4)
			avail = cap - len(label)
		}
		if avail <= 0 {
			_, size := utf8.DecodeRuneInString(text)
			if size <= 0 || size > len(text) {
				size = 1
			}
			fragments = append(fragments, label+text[:size])
			text = text[size:]
			continue
		}
		chunk := state.UTF8Prefix(text, avail)
		if chunk == "" {
			_, size := utf8.DecodeRuneInString(text)
			if size <= 0 || size > len(text) {
				size = 1
			}
			chunk = text[:size]
		}
		fragments = append(fragments, label+chunk)
		text = text[len(chunk):]
	}
	return fragments
}

// digestBatch packs complete sources in sequence order. An exceptional large
// source is processed as a group of labeled UTF-8 fragments in one transaction.
func (r *Runtime) digestBatch(mem state.ChatMemory) ([]string, int) {
	var b strings.Builder
	count := 0
	for _, event := range mem.Pending {
		text := fmt.Sprintf("Source %d (%s), topic %d, %s:\n%s\n", event.Sequence, event.ID, event.Topic, event.At.Format(time.RFC3339), event.Transcript)
		text = r.Options.Redaction.Text(redaction.Remote, text)
		if b.Len()+len(text) > digestTranscriptCap {
			if count > 0 {
				break
			}
			labelFor := func(n int) string {
				return r.Options.Redaction.Text(redaction.Remote, fmt.Sprintf("Source %d, topic %d, fragment %d:\n", event.Sequence, event.Topic, n))
			}
			// A provider sees every fragment, including the final unprocessed tail.
			return splitDigestFragments(text, labelFor, digestTranscriptCap), 1
		}
		b.WriteString(text)
		count++
	}
	if count == 0 {
		return nil, 0
	}
	return []string{b.String()}, count
}

func (r *Runtime) digestChat(ctx context.Context, key string, rec state.ChatRecord) error {
	mem, err := r.Store.SnapshotDigest(key)
	if err != nil {
		return err
	}
	transcripts, count := r.digestBatch(mem)
	if count == 0 {
		return nil
	}
	route := platform.Message{Platform: rec.Platform, ChatID: rec.ChatID, UserID: rec.Owner}
	selected, err := r.chatModel(route)
	if err != nil {
		return err
	}
	model := selected.Model
	if r.Config.Memory.Model != "" {
		model = r.Config.Memory.Model
	}
	provider, err := r.Providers(state.Conversation{Provider: selected.Provider, Model: model})
	if err != nil {
		return err
	}
	thinking := ""
	if selected.Thinking != nil {
		thinking = *selected.Thinking
	}
	var summaries []string
	for _, transcript := range transcripts {
		summary, err := r.distill(ctx, provider, model, thinking, transcript)
		if err != nil {
			return err
		}
		summary = strings.TrimSpace(r.Store.RedactMemoryText(r.Options.Redaction.Text(redaction.Remote, summary)))
		if summary == "" {
			return fmt.Errorf("empty digest summary")
		}
		summaries = append(summaries, summary)
	}
	summary := strings.Join(summaries, "\n")
	date := time.Now().In(memoryLocation(r.Config.Memory.Timezone)).Format("2006-01-02")
	result := mem
	result.Entries = append(append([]state.MemoryEntry(nil), mem.Entries...), state.MemoryEntry{Date: date, Text: summary})
	compacted := false
	if result.Bytes() > r.Config.Memory.MaxBytes {
		var compactErr error
		result, compacted, compactErr = r.compactMemory(ctx, provider, model, thinking, result, date)
		if compactErr != nil {
			r.report(compactErr)
		}
	}
	result = r.Store.PrepareMemory(result, r.Config.Memory.MaxBytes)
	dropped := result.Loss.DroppedNoteBytes > mem.Loss.DroppedNoteBytes
	// Store the full digest; only notify a two-line highlight to keep chat quiet.
	notice := ""
	if len(result.Entries) > 0 {
		notice = result.Entries[len(result.Entries)-1].Text
	}
	notice = digestOneLineNotice(date, notice, compacted, dropped)
	return r.Store.CommitDigest(key, mem, result, count, route, telegram.FormatAndSplit(notice), time.Now(), r.Config.Memory.MaxBytes)
}

// digestOneLineNotice keeps the full digest in storage (/memory) but notifies
// only a two-line highlight, localized to the language the user uses most.
// Line 1 is the digest notice, line 2 is the labeled highlight so users don't
// mistake it for the only note stored that day.
func digestOneLineNotice(date, note string, compacted, dropped bool) string {
	highlight := digestHighlight(note)
	chinese := digestIsChinese(note)
	if highlight == "" {
		if chinese {
			return fmt.Sprintf("%s 記憶已整理，詳見 /memory。", date)
		}
		return fmt.Sprintf("%s memory digested, see /memory for full notes.", date)
	}
	if chinese {
		head := fmt.Sprintf("%s 記憶已整理，詳見 /memory。", date)
		if compacted {
			head += "（舊筆記已壓縮）"
		}
		if dropped {
			head += "（部分筆記已精簡）"
		}
		return head + "\n重點：" + highlight
	}
	head := fmt.Sprintf("%s memory digested, see /memory for full notes.", date)
	if compacted {
		head += " Older notes compacted."
	}
	if dropped {
		head += " Some notes trimmed."
	}
	return head + "\nHighlight: " + highlight
}

// digestHighlight picks today's top highlight: the first bullet of the newest
// stored note, collapsed to one line.
func digestHighlight(note string) string {
	for _, line := range strings.Split(note, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		t = strings.TrimLeft(t, "-*•·> ")
		// Strip ordered-list prefixes like "1. " or "1) ".
		i := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
		if i > 0 && i < len(t) && (t[i] == '.' || t[i] == ')') {
			t = strings.TrimSpace(t[i+1:])
		}
		t = strings.Join(strings.Fields(t), " ")
		if t == "" {
			continue
		}
		const maxHighlightRunes = 140
		if r := []rune(t); len(r) > maxHighlightRunes {
			t = string(r[:maxHighlightRunes]) + "…"
		}
		return t
	}
	return ""
}

// digestIsChinese reports whether text is predominantly in the user's Chinese,
// used to pick the notice wrapper language. The highlight itself already
// carries the user's wording via distill.
func digestIsChinese(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// distill asks the provider for durable memory notes without tools.
func (r *Runtime) distill(ctx context.Context, provider llm.Provider, model, thinking, transcript string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Config.TaskDuration())
	defer cancel()
	resp, err := provider.Complete(ctx, llm.Request{
		Model:    model,
		Thinking: thinking,
		Messages: []llm.Message{
			{Role: "system", Content: "You distill a day of chat history into durable long-term memory notes. Extract only lasting facts worth remembering: user preferences, decisions made, project context, names, and ongoing topics. Omit greetings, small talk, and routine tool output. Never repeat secrets, tokens, or credentials. Reply in the same language the user uses most in the transcript. Reply with 1-8 short bullet lines using - markers, plain text, no preamble, most important first."},
			{Role: "user", Content: transcript},
		},
		Tools:       nil,
		Temperature: 0.2,
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.Content), nil
}

// compactMemory summarizes all but the newest entry into one, preserving the
// newest verbatim. On summarization failure it hard-drops oldest entries.
func (r *Runtime) compactMemory(ctx context.Context, provider llm.Provider, model, thinking string, mem state.ChatMemory, date string) (state.ChatMemory, bool, error) {
	if len(mem.Entries) <= 1 {
		return mem, false, nil
	}
	var b strings.Builder
	for _, e := range mem.Entries[:len(mem.Entries)-1] {
		b.WriteString("- [")
		b.WriteString(e.Date)
		b.WriteString("] ")
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	ctx, cancel := context.WithTimeout(ctx, r.Config.TaskDuration())
	defer cancel()
	resp, err := provider.Complete(ctx, llm.Request{
		Model:    model,
		Thinking: thinking,
		Messages: []llm.Message{
			{Role: "system", Content: "Compress these older long-term memory notes into fewer lines, preserving every durable fact (preferences, decisions, project context, names). Never repeat secrets. Reply with short bullet lines using - markers, plain text, no preamble."},
			{Role: "user", Content: b.String()},
		},
		Tools:       nil,
		Temperature: 0.2,
	}, nil)
	if err != nil {
		return mem, false, err
	}
	summary := strings.TrimSpace(r.Store.RedactMemoryText(r.Options.Redaction.Text(redaction.Remote, resp.Message.Content)))
	if summary == "" {
		return mem, false, fmt.Errorf("empty compaction summary")
	}
	mem.Entries = append([]state.MemoryEntry{{Date: date, Text: "Earlier notes (compacted):\n" + summary}}, mem.Entries[len(mem.Entries)-1:]...)
	compacted := mem
	return compacted, true, nil
}

func (r *Runtime) memoryHistory(m platform.Message, conv state.Conversation) []llm.Message {
	history := append(r.companionConversationContext(m), conv.Messages()...)
	if !r.Config.Memory.Enabled {
		return history
	}
	limit := r.Config.Memory.MaxContextBytes
	if limit <= 0 {
		limit = fallbackMemoryContextBytes
	}
	mem, err := r.Store.LoadMemory(m.WorkspaceKey())
	if err != nil {
		return history
	}
	// Redact the heading, date prefixes, and note text before allocation. A
	// second pass covers matches spanning formatting boundaries.
	heading := r.Options.Redaction.Text(redaction.Remote, memoryHeading)
	for i := range mem.Entries {
		mem.Entries[i].Text = r.Options.Redaction.Text(redaction.Remote, mem.Entries[i].Text)
		mem.Entries[i].Date = r.Options.Redaction.Text(redaction.Remote, mem.Entries[i].Date)
	}
	text := renderMemoryText(mem, limit, heading)
	filtered := r.Options.Redaction.Text(redaction.Remote, text)
	if len(filtered) > limit {
		hardLimit := limit
		// If formatting expanded, rerender with only the newest note so the
		// final cap cannot sacrifice recent facts to an older partial note.
		mem.Entries = mem.Entries[len(mem.Entries)-1:]
		for len(filtered) > hardLimit {
			limit -= len(filtered) - hardLimit
			text = renderMemoryText(mem, limit, heading)
			if text == "" {
				return history
			}
			filtered = r.Options.Redaction.Text(redaction.Remote, text)
		}
	}
	text = filtered
	if strings.TrimSpace(text) == "" {
		return history
	}
	return append([]llm.Message{{Role: "system", Content: text}}, history...)
}

const memoryHeading = "Long-term memory for this chat (distilled nightly, newest last):\n"

// memoryText reserves the entire format and favors recent notes. Partial notes
// are trimmed on UTF-8 boundaries; impossible minimum formats yield no context.
func memoryText(mem state.ChatMemory, limit int) string {
	return renderMemoryText(mem, limit, memoryHeading)
}

func renderMemoryText(mem state.ChatMemory, limit int, heading string) string {
	if len(mem.Entries) == 0 || limit <= len(heading) {
		return ""
	}
	remaining := limit - len(heading)
	var lines []string
	for i := len(mem.Entries) - 1; i >= 0; i-- {
		prefix := "- [" + mem.Entries[i].Date + "] "
		separator := 0
		if len(lines) > 0 {
			separator = 1
		}
		available := remaining - len(prefix) - separator
		if available <= 0 {
			break
		}
		text := state.UTF8Prefix(mem.Entries[i].Text, available)
		if text == "" {
			break
		}
		line := prefix + text
		lines = append([]string{line}, lines...)
		remaining -= len(line) + separator
	}
	if len(lines) == 0 {
		return ""
	}
	return heading + strings.Join(lines, "\n")
}

// A single digest worker handles nightly, deferred, and retry work. The minute
// tick also recovers from a missed wake or a failed registry/storage read.
// No startup sweep: pending journal waits for the next wall-clock digest;
// boot only resumes already-enrolled retry/deferred work.
func (r *Runtime) digestLoop(ctx context.Context) {
	loc := memoryLocation(r.Config.Memory.Timezone)
	hour, minute, err := parseDigestAtLocal(r.Config.Memory.DigestAt)
	if err != nil {
		r.report(err)
		return
	}
	next := time.Now().Add(untilNextDigest(time.Now(), loc, hour, minute))
	pendingSweep := false
	globalRetry := state.MemoryRetry{}
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(next) {
			pendingSweep = true
			next = now.Add(untilNextDigest(now, loc, hour, minute))
		}
		if !now.Before(globalRetry.NextAt) {
			err := r.digestWork(ctx, pendingSweep)
			if err != nil {
				r.report(err)
				globalRetry.Attempts++
				globalRetry.NextAt = time.Now().Add(digestRetryDelay(globalRetry.Attempts))
			} else {
				pendingSweep = false
				globalRetry = state.MemoryRetry{}
			}
			r.mu.Lock()
			r.digestGlobalRetry = globalRetry
			r.mu.Unlock()
		}
		delay := time.Until(next)
		if delay > time.Minute {
			delay = time.Minute
		}
		if d := time.Until(globalRetry.NextAt); !globalRetry.NextAt.IsZero() && d > 0 && d < delay {
			delay = d
		}
		r.mu.Lock()
		for _, retry := range r.digestRetries {
			if d := time.Until(retry.retry.NextAt); !retry.retry.NextAt.IsZero() && d > 0 && d < delay {
				delay = d
			}
		}
		wake := r.digestWake
		r.mu.Unlock()
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func parseDigestAtLocal(s string) (int, int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, fmt.Errorf("invalid memory digest_at %q: want HH:MM", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 || len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("invalid memory digest_at %q: want HH:MM", s)
	}
	return h, m, nil
}
