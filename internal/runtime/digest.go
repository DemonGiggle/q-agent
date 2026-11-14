package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
const fallbackMemoryContextBytes = 6000

// Daytime fast-path thresholds (issue #34 Phase 0): a chat with this much
// unconsolidated journal does not wait for the nightly sweep. digestPass
// enrolls it on the next minute tick even when sweep==false.
const smallDigestMinPending = 20
const smallDigestMinBytes = 512 * 1024

// maxDigestAttempts bounds per-chat digest retries. The prompt allows the
// model to return [] when a batch has no durable facts (handled as a
// successful no-op ack), so any remaining "empty digest summary" or provider
// error that reaches the retry loop is either transient or a poison pill.
// Retrying hourly forever (attempt 12+ observed) only spams logs; after a few
// tries we park until the next nightly sweep re-enrolls the chat.
const maxDigestAttempts = 4

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
		hasQueued := r.hasQueuedForLocked(chat.Key)
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
			if retry.Attempts >= maxDigestAttempts {
				// Park until the next nightly sweep re-enrolls; pending stays.
				r.rememberDigestRetry(chat.Key, local.epoch, state.MemoryRetry{})
				r.mu.Lock()
				delete(r.digestRetries, chat.Key)
				r.mu.Unlock()
				r.report(loadErr)
				r.logf("[MEMORY] Digest giving up after %d attempts | workspace=%s error=%q", retry.Attempts, chat.Key, r.Options.Redaction.Text(redaction.Terminal, loadErr.Error()))
				continue
			}
			retry.NextAt = time.Now().Add(digestRetryDelay(retry.Attempts))
			r.rememberDigestRetry(chat.Key, local.epoch, retry)
			r.report(loadErr)
			continue
		}
		if mem.LegacyBaseline && !mem.MigrationReported {
			r.report(r.Store.ReportMemoryMigration(chat.Key, chat.WorkspaceRoute()))
		}
		if sweep && (len(mem.Pending) > 0 || busy || hasQueued || maintenance) {
			retry.Work = true
		}
		// Daytime fast path: a chat with lots of unconsolidated journal does
		// not wait for the nightly sweep; the minute tick picks it up.
		if !sweep && (len(mem.Pending) >= smallDigestMinPending || mem.PendingBytes() >= smallDigestMinBytes) {
			retry.Work = true
		}
		if len(mem.Pending) == 0 {
			if busy || hasQueued || maintenance {
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
		if busy || hasQueued || maintenance {
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
					if retry.Attempts >= maxDigestAttempts {
						r.rememberDigestRetry(chat.Key, mem.Epoch, state.MemoryRetry{})
						r.mu.Lock()
						delete(r.digestRetries, chat.Key)
						r.mu.Unlock()
						r.report(err)
						r.logf("[MEMORY] Digest giving up after %d attempts | workspace=%s error=%q", retry.Attempts, chat.Key, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
					} else {
						retry.NextAt = time.Now().Add(digestRetryDelay(retry.Attempts))
						r.rememberDigestRetry(chat.Key, mem.Epoch, retry)
					}
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
			if retry.Attempts >= maxDigestAttempts {
				// Park until the next nightly sweep re-enrolls; pending stays.
				// This bounds hourly retries to maxDigestAttempts per cycle
				// instead of growing to attempt 12+ with hourly 1h backoff.
				r.rememberDigestRetry(chat.Key, mem.Epoch, state.MemoryRetry{})
				r.mu.Lock()
				delete(r.digestRetries, chat.Key)
				r.mu.Unlock()
				r.report(err)
				r.logf("[MEMORY] Digest giving up after %d attempts | workspace=%s error=%q", retry.Attempts, chat.Key, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
				continue
			}
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
	thinking := ""
	if selected.Thinking != nil {
		thinking = *selected.Thinking
	}
	providerName, model, thinking := r.Config.ResolveModelOverride(
		r.Config.Memory.Override, selected.Provider, selected.Model, thinking)
	provider, err := r.Providers(state.Conversation{Provider: providerName, Model: model})
	var summaries []string
	for _, transcript := range transcripts {
		summary, err := r.distill(ctx, provider, model, thinking, transcript)
		if err != nil {
			return err
		}
		summary = strings.TrimSpace(r.Store.RedactMemoryText(r.Options.Redaction.Text(redaction.Remote, summary)))
		if summary == "" {
			// Empty model output is a prompt violation (the prompt asks for
			// strict JSON, and [] is the valid "no facts" reply), so keep it
			// retryable - the retry cap in digestPass bounds it. Skipping
			// here would silently drop the batch on a transient glitch.
			// Fragment-level empties are still tolerated below only when at
			// least one fragment produced output; all-empty stays an error.
			// Note: valid [] is handled later as a successful no-op ack.
			if len(transcripts) > 1 {
				continue
			}
			return fmt.Errorf("empty digest summary")
		}
		summaries = append(summaries, summary)
	}
	if len(summaries) == 0 {
		return fmt.Errorf("empty digest summary")
	}
	date := time.Now().In(memoryLocation(r.Config.Memory.Timezone)).Format("2006-01-02")
	var fresh []state.MemoryEntry
	var bulletParts []string
	for _, s := range summaries {
		if notes, ok := parseDistilledJSON(s, date); ok {
			fresh = append(fresh, notes...)
			continue
		}
		if extracted := extractBracketJSON(s); extracted != "" {
			if notes, ok := parseDistilledJSON(extracted, date); ok {
				fresh = append(fresh, notes...)
				continue
			}
		}
		bulletParts = append(bulletParts, s)
	}
	if len(bulletParts) > 0 {
		// Legacy bullet path stays one entry per digest batch so fragments of
		// a single oversized source commit as one group, as before.
		joined := strings.Join(bulletParts, "\n")
		fresh = append(fresh, state.MemoryEntry{Date: date, Text: joined, Kind: "fact", Keys: state.ExtractMemoryKeys(joined), Importance: 3})
	}
	if len(fresh) == 0 {
		// The prompt allows "an array of 0-8 objects": [] validly means the
		// batch had no lasting facts (small talk, one-off output). The
		// provider is alive and normal chat works; retrying the same
		// transcript would return [] again forever. Ack without new notes.
		result := r.Store.PrepareMemory(mem, r.Config.Memory.MaxBytes)
		r.logf("[MEMORY] Digest acknowledged without new notes | workspace=%s count=%d", key, count)
		return r.Store.CommitDigest(key, mem, result, count, route, nil, time.Now(), r.Config.Memory.MaxBytes)
	}
	result := mem
	result.Entries = mergeMemoryEntries(mem.Entries, fresh)
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

// distill asks the provider for durable memory notes without tools. The
// prompt requests strict JSON so notes arrive structured (kind/keys/
// importance) for dedup and retrieval; bullet fallback keeps old and small
// models working (see parseDistilledNotes).
func (r *Runtime) distill(ctx context.Context, provider llm.Provider, model, thinking, transcript string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Config.TaskDuration())
	defer cancel()
	resp, err := provider.Complete(ctx, llm.Request{
		Model:    model,
		Thinking: thinking,
		Messages: []llm.Message{
			{Role: "system", Content: "Distill a day of chat history into durable long-term memory notes. Keep only lasting facts: user preferences, decisions made, project context with names and versions, and lessons from failures. Merge duplicates, update corrected preferences instead of adding a second note, drop one-off tool output and small talk. Never repeat secrets, tokens, or credentials. Reply in the same language the user uses most in the transcript. Reply with strict JSON only: an array of 0-8 objects like [{\"text\":\"...\",\"kind\":\"preference|project|decision|lesson|fact\",\"keys\":[\"keyword1\",\"keyword2\"],\"importance\":1-5}], no preamble, no code fences. importance 5 = lasting preference/identity, 1 = trivia."},
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

// parseDistilledNotes converts a distill response into structured entries.
// Strict JSON is preferred; bullet/plain lines fall back to kind=fact,
// importance=3 so legacy prompts and small models keep working.
func parseDistilledNotes(raw, date string) []state.MemoryEntry {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if entries, ok := parseDistilledJSON(trimmed, date); ok {
		return entries
	}
	// Tolerate code fences around JSON.
	if extracted := extractBracketJSON(trimmed); extracted != "" {
		if entries, ok := parseDistilledJSON(extracted, date); ok {
			return entries
		}
	}
	var out []state.MemoryEntry
	for _, line := range strings.Split(trimmed, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		t = strings.TrimLeft(t, "-*•·> ")
		i := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
		if i > 0 && i < len(t) && (t[i] == '.' || t[i] == ')') {
			t = strings.TrimSpace(t[i+1:])
		}
		t = strings.TrimSpace(strings.Trim(t, "\"'"))
		if t == "" || strings.EqualFold(t, "[]") {
			continue
		}
		out = append(out, state.MemoryEntry{
			Date: date, Text: t, Kind: "fact",
			Keys: state.ExtractMemoryKeys(t), Importance: 3,
		})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func parseDistilledJSON(raw, date string) ([]state.MemoryEntry, bool) {
	var items []struct {
		Text       string   `json:"text"`
		Kind       string   `json:"kind"`
		Keys       []string `json:"keys"`
		Importance int      `json:"importance"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, false
	}
	var out []state.MemoryEntry
	for _, it := range items {
		text := strings.TrimSpace(it.Text)
		if text == "" {
			continue
		}
		imp := it.Importance
		if imp < 1 || imp > 5 {
			imp = 3
		}
		keys := it.Keys
		if len(keys) == 0 {
			keys = state.ExtractMemoryKeys(text)
		}
		if len(keys) > 8 {
			keys = keys[:8]
		}
		out = append(out, state.MemoryEntry{
			Date: date, Text: text, Kind: state.NormalizeMemoryKind(it.Kind),
			Keys: keys, Importance: imp,
		})
		if len(out) >= 8 {
			break
		}
	}
	return out, true
}

// mergeMemoryEntries appends fresh notes with de-dup and correction merging
// (issue #34 acceptance: same preference said three times stays one note).
func mergeMemoryEntries(existing []state.MemoryEntry, fresh []state.MemoryEntry) []state.MemoryEntry {
	merged := append([]state.MemoryEntry(nil), existing...)
	for _, n := range fresh {
		if idx := findDuplicateNote(merged, n); idx >= 0 {
			cur := merged[idx]
			if len(n.Text) > len(cur.Text) {
				cur.Text = n.Text
			}
			seen := map[string]bool{}
			for _, k := range cur.Keys {
				seen[strings.ToLower(k)] = true
			}
			for _, k := range n.Keys {
				if k == "" || seen[strings.ToLower(k)] {
					continue
				}
				seen[strings.ToLower(k)] = true
				cur.Keys = append(cur.Keys, k)
			}
			if n.Importance > cur.Importance && !cur.Pinned {
				cur.Importance = n.Importance
			}
			if n.Kind != "" && n.Kind != "fact" && (cur.Kind == "" || cur.Kind == "fact") {
				cur.Kind = n.Kind
			}
			merged[idx] = cur
			continue
		}
		merged = append(merged, n)
	}
	return merged
}

func findDuplicateNote(existing []state.MemoryEntry, n state.MemoryEntry) int {
	norm := normalizeNoteText(n.Text)
	if norm == "" {
		return -1
	}
	nKeys := map[string]bool{}
	for _, k := range n.Keys {
		nKeys[strings.ToLower(strings.TrimSpace(k))] = true
	}
	for i, e := range existing {
		enorm := normalizeNoteText(e.Text)
		if enorm == "" {
			continue
		}
		if enorm == norm {
			return i
		}
		if len(norm) > 20 && len(enorm) > 20 && (strings.Contains(enorm, norm) || strings.Contains(norm, enorm)) {
			return i
		}
		if len(nKeys) > 0 {
			overlap := 0
			for _, k := range e.Keys {
				if nKeys[strings.ToLower(strings.TrimSpace(k))] {
					overlap++
				}
			}
			if overlap >= 2 && (strings.Contains(enorm, norm) || strings.Contains(norm, enorm) || overlap >= 3) {
				return i
			}
		}
	}
	return -1
}

func normalizeNoteText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func extractBracketJSON(s string) string {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// compactMemory compresses low-signal older notes first, preserving pinned
// notes and the newest entry verbatim. On summarization failure it returns the
// input unchanged so the caller's hard budget (importance-aware) still bounds
// disk; the error is reported but the digest still commits.
func (r *Runtime) compactMemory(ctx context.Context, provider llm.Provider, model, thinking string, mem state.ChatMemory, date string) (state.ChatMemory, bool, error) {
	if len(mem.Entries) <= 1 {
		return mem, false, nil
	}
	newest := mem.Entries[len(mem.Entries)-1]
	var pinnedOlder []state.MemoryEntry
	var compressible []state.MemoryEntry
	for _, e := range mem.Entries[:len(mem.Entries)-1] {
		if e.Pinned {
			pinnedOlder = append(pinnedOlder, e)
		} else {
			compressible = append(compressible, e)
		}
	}
	if len(compressible) == 0 {
		return mem, false, nil
	}
	var b strings.Builder
	for _, e := range compressible {
		label := e.Date
		if e.Kind != "" {
			label += "|" + e.Kind
		}
		b.WriteString("- [")
		b.WriteString(label)
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
	compactedNotes := parseDistilledNotes(summary, date)
	if len(compactedNotes) == 0 {
		compactedNotes = []state.MemoryEntry{{Date: date, Text: "Earlier notes (compacted):\n" + summary, Kind: "fact", Importance: 2}}
	} else if len(compactedNotes) == 1 && !strings.Contains(compactedNotes[0].Text, "compacted") {
		compactedNotes[0].Date = date
	}
	out := append([]state.MemoryEntry(nil), pinnedOlder...)
	out = append(out, compactedNotes...)
	out = append(out, newest)
	mem.Entries = out
	compacted := mem
	return compacted, true, nil
}

func (r *Runtime) memoryHistory(m platform.Message, conv state.Conversation) []llm.Message {
	query := ""
	for i := len(conv.Pairs) - 1; i >= 0; i-- {
		if strings.TrimSpace(conv.Pairs[i].User) != "" {
			query = conv.Pairs[i].User
			break
		}
	}
	return r.memoryHistoryWithQuery(m, conv, query)
}

// memoryHistoryWithQuery injects consolidated notes plus the newest
// unconsolidated journal (issue #34 Phase 0): same-day preferences are
// readable before the nightly digest, explicitly marked as not yet digested.
func (r *Runtime) memoryHistoryWithQuery(m platform.Message, conv state.Conversation, query string) []llm.Message {
	history := append(r.companionConversationContext(m), conv.Messages()...)
	// Harness-owned live schedule ledger (issue #64): tiny, position-stable,
	// survives the 12-turn/32KB eviction because it lives in schedules.json,
	// not in Pairs. Empty ledger = block omitted, like check-in reference.
	scheduleMsg := r.scheduleLedgerMessage(m)
	if !r.Config.Memory.Enabled {
		if scheduleMsg == nil {
			return history
		}
		return append([]llm.Message{*scheduleMsg}, history...)
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
	pendingHeading := r.Options.Redaction.Text(redaction.Remote, pendingMemoryHeading)
	for i := range mem.Entries {
		mem.Entries[i].Text = r.Options.Redaction.Text(redaction.Remote, mem.Entries[i].Text)
		mem.Entries[i].Date = r.Options.Redaction.Text(redaction.Remote, mem.Entries[i].Date)
		mem.Entries[i].Kind = r.Options.Redaction.Text(redaction.Remote, mem.Entries[i].Kind)
		for j, k := range mem.Entries[i].Keys {
			mem.Entries[i].Keys[j] = r.Options.Redaction.Text(redaction.Remote, k)
		}
	}
	tokens := queryTokens(query)
	pendingBudget := limit / 3
	if pendingBudget > 2000 {
		pendingBudget = 2000
	}
	if len(mem.Entries) == 0 {
		pendingBudget = limit
	}
	pendingText := renderPendingText(mem.Pending, pendingBudget, pendingHeading)
	entriesLimit := limit - len(pendingText)
	entriesText := renderMemoryEntries(mem, entriesLimit, heading, tokens)
	text := entriesText
	if pendingText != "" {
		if text != "" {
			text += "\n"
		}
		text += pendingText
	}
	filtered := r.Options.Redaction.Text(redaction.Remote, text)
	if len(filtered) > limit {
		// Redaction expanded the format: pending goes first, then fall back
		// to the single highest-signal note so the cap never sacrifices the
		// best fact for an older partial note.
		entriesText = renderMemoryEntries(mem, limit, heading, tokens)
		filtered = r.Options.Redaction.Text(redaction.Remote, entriesText)
		if len(filtered) > limit && len(mem.Entries) > 0 {
			top := topMemoryEntry(mem, tokens)
			mem.Entries = []state.MemoryEntry{top}
			entriesText = renderMemoryEntries(mem, limit, heading, tokens)
			filtered = r.Options.Redaction.Text(redaction.Remote, entriesText)
			for len(filtered) > limit && limit > 0 {
				limit -= len(filtered) - limit
				if limit <= len(heading) {
					return history
				}
				entriesText = renderMemoryEntries(mem, limit, heading, tokens)
				if entriesText == "" {
					return history
				}
				filtered = r.Options.Redaction.Text(redaction.Remote, entriesText)
			}
		}
	}
	text = filtered
	var prefix []llm.Message
	if scheduleMsg != nil {
		prefix = append(prefix, *scheduleMsg)
	}
	if strings.TrimSpace(text) != "" {
		prefix = append(prefix, llm.Message{Role: "system", Content: text})
	}
	if len(prefix) == 0 {
		return history
	}
	return append(prefix, history...)
}

// scheduleLedgerMessage renders the harness-owned live schedule ledger for
// issue #64 as a stable, clearly labeled system block. It carries IDs plus
// one-line summaries so the model never has to guess after the turn closes,
// names sibling topics when this topic is empty, and warns against
// recreating recently deleted texts from past discussion or long-term
// memory. Empty returns nil so the caller omits the block.
func (r *Runtime) scheduleLedgerMessage(m platform.Message) *llm.Message {
	if r.Store == nil {
		return nil
	}
	raw := r.Store.ScheduleLedgerText(m)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	filtered := raw
	if r.Options.Redaction != nil {
		filtered = r.Options.Redaction.Text(redaction.Remote, raw)
	}
	if strings.TrimSpace(filtered) == "" {
		return nil
	}
	// Bound the injected block so a full topic cannot blow the prompt: the
	// ledger already caps at 20 live + 20 recent lines, this is defense.
	filtered = truncateLedgerBlock(filtered, 4000)
	return &llm.Message{Role: "system", Content: filtered}
}

// truncateLedgerBlock caps s to limit bytes on a UTF-8 boundary.
func truncateLedgerBlock(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n… (truncated; use schedule_list for full list)"
}

const memoryHeading = "Long-term memory for this chat (distilled nightly, newest last):\n"
const pendingMemoryHeading = "Unconsolidated notes from today (not yet digested, may contain noise):\n"

// memoryText reserves the entire format and favors high-signal notes. Partial
// notes are trimmed on UTF-8 boundaries; impossible minimum formats yield no
// context.
func memoryText(mem state.ChatMemory, limit int) string {
	return renderMemoryEntries(mem, limit, memoryHeading, nil)
}

// renderMemoryText keeps the historic newest-first truncation order for
// callers that need byte-stable output (tests). New prompt paths should use
// renderMemoryEntries scoring instead.
func renderMemoryText(mem state.ChatMemory, limit int, heading string) string {
	return legacyRenderMemoryText(mem, limit, heading)
}

func legacyRenderMemoryText(mem state.ChatMemory, limit int, heading string) string {
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

// renderMemoryEntries selects top-scoring notes within budget (issue #34
// Phase 2) and prints them chronologically. Scoring prefers pinned,
// high-importance, kind-weighted, query-matching, recent notes.
func renderMemoryEntries(mem state.ChatMemory, limit int, heading string, tokens map[string]bool) string {
	if len(mem.Entries) == 0 || limit <= len(heading) {
		return ""
	}
	ranked := rankMemoryEntries(mem.Entries, tokens)
	remaining := limit - len(heading)
	type picked struct {
		e    state.MemoryEntry
		text string
	}
	var selected []picked
	for idx, e := range ranked {
		prefix := memoryEntryPrefix(e)
		separator := 0
		if len(selected) > 0 {
			separator = 1
		}
		available := remaining - len(prefix) - separator
		if available <= 0 {
			// Legacy semantics: when the highest-signal note cannot fit,
			// emit nothing rather than substituting a lower-signal note.
			if idx == 0 {
				return ""
			}
			continue
		}
		text := state.UTF8Prefix(e.Text, available)
		if text == "" {
			if idx == 0 {
				return ""
			}
			continue
		}
		selected = append(selected, picked{e: e, text: text})
		remaining -= len(prefix) + len(text) + separator
	}
	if len(selected) == 0 {
		return ""
	}
	// Chronological display keeps diffs stable; score decides admission.
	sort.Slice(selected, func(a, b int) bool {
		if selected[a].e.Date != selected[b].e.Date {
			return selected[a].e.Date < selected[b].e.Date
		}
		return selected[a].text < selected[b].text
	})
	lines := make([]string, 0, len(selected))
	for _, s := range selected {
		lines = append(lines, memoryEntryPrefix(s.e)+s.text)
	}
	return heading + strings.Join(lines, "\n")
}

func memoryEntryPrefix(e state.MemoryEntry) string {
	var b strings.Builder
	b.WriteString("- [")
	b.WriteString(e.Date)
	b.WriteString("]")
	if e.Kind != "" && e.Kind != "fact" {
		b.WriteString("[" + e.Kind + "]")
	}
	if e.Pinned {
		b.WriteString("[pinned]")
	}
	if e.EffectiveImportance() != 3 {
		b.WriteString("[!" + itoa(e.EffectiveImportance()) + "]")
	}
	b.WriteString(" ")
	return b.String()
}

// scoreMemoryEntry ranks notes: pinned dominates, then kind weight, then
// importance, then query key/text hits, then recency.
func scoreMemoryEntry(e state.MemoryEntry, tokens map[string]bool, now time.Time) int {
	score := 0
	if e.Pinned {
		score += 40
	}
	switch state.NormalizeMemoryKind(e.Kind) {
	case "preference":
		score += 10
	case "project":
		score += 8
	case "decision":
		score += 5
	case "lesson":
		score += 4
	default:
		score += 2
	}
	score += e.EffectiveImportance() * 3
	if len(tokens) > 0 {
		lower := strings.ToLower(e.Text)
		hits := 0
		for _, k := range e.Keys {
			if tokens[strings.ToLower(k)] {
				score += 3
				hits++
			}
		}
		for t := range tokens {
			if len(t) >= 2 && strings.Contains(lower, t) {
				hits++
			}
		}
		if hits > 0 {
			score += 1
			if hits > 4 {
				score += 2
			}
		}
		_ = hits
	}
	if d, err := time.Parse("2006-01-02", e.Date); err == nil {
		if age := now.Sub(d); age >= 0 && age < 30*24*time.Hour {
			score += 2
		}
	} else if strings.TrimSpace(e.Date) != "" {
		score += 1
	}
	return score
}

func rankMemoryEntries(entries []state.MemoryEntry, tokens map[string]bool) []state.MemoryEntry {
	now := time.Now()
	type scored struct {
		e     state.MemoryEntry
		score int
		idx   int
	}
	tmp := make([]scored, 0, len(entries))
	for i, e := range entries {
		tmp = append(tmp, scored{e: e, score: scoreMemoryEntry(e, tokens, now), idx: i})
	}
	sort.Slice(tmp, func(a, b int) bool {
		if tmp[a].score != tmp[b].score {
			return tmp[a].score > tmp[b].score
		}
		return tmp[a].idx > tmp[b].idx
	})
	out := make([]state.MemoryEntry, 0, len(tmp))
	for _, s := range tmp {
		out = append(out, s.e)
	}
	return out
}

func topMemoryEntry(mem state.ChatMemory, tokens map[string]bool) state.MemoryEntry {
	ranked := rankMemoryEntries(mem.Entries, tokens)
	return ranked[0]
}

func queryTokens(query string) map[string]bool {
	tokens := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(query)) {
		w = strings.Trim(w, ".,!?;:\"'()[]{}<>/-_+*=|@#$%^&~`")
		if len(w) < 2 || len(w) > 32 {
			continue
		}
		tokens[w] = true
		if len(tokens) >= 32 {
			break
		}
	}
	return tokens
}

// renderPendingText injects the newest unconsolidated journal (latest 2
// sources) so same-day facts are readable before the digest runs.
func renderPendingText(pending []state.SourceEvent, limit int, heading string) string {
	if len(pending) == 0 || limit <= len(heading) {
		return ""
	}
	n := len(pending)
	if n > 2 {
		pending = pending[n-2:]
		n = 2
	}
	remaining := limit - len(heading)
	var lines []string
	for _, ev := range pending {
		label := "- [pending " + ev.At.Format("15:04") + "] "
		separator := 0
		if len(lines) > 0 {
			separator = 1
		}
		available := remaining - len(label) - separator
		if available <= 0 {
			continue
		}
		snippet := state.UTF8Prefix(strings.TrimSpace(ev.Transcript), available)
		if snippet == "" {
			continue
		}
		// Single-line the snippet so one long transcript cannot flood the prompt.
		snippet = strings.Join(strings.Fields(snippet), " ")
		lines = append(lines, label+snippet)
		remaining -= len(label) + len(snippet) + separator
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
