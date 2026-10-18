package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

// digestTranscriptCap bounds a single night's transcript sent for distillation.
const digestTranscriptCap = 20000

// fallbackMemoryContextBytes applies when configuration is absent (tests).
const fallbackMemoryContextBytes = 2000

// digestTopicPairs carries one topic's undigested pairs into the transcript.
type digestTopicPairs struct {
	topic int64
	pairs []state.Pair
}

func memoryLocation(cfg string) *time.Location {
	if cfg == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(cfg); err == nil {
		return loc
	}
	return time.Local
}

// untilNextDigest returns the duration until the next daily HH:MM wall time.
func untilNextDigest(now time.Time, loc *time.Location, hour, min int) time.Duration {
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, min, 0, 0, loc)
	if !next.After(local) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(local)
}

// digestSweep distills new conversation pairs for every registered chat.
// Chats with active tasks (or during maintenance) are skipped and retried
// the next night; their digest watermarks are left untouched.
func (r *Runtime) digestSweep(ctx context.Context) error {
	if !r.Config.Memory.Enabled {
		return nil
	}
	chats, err := r.Store.ListChats()
	if err != nil {
		return err
	}
	for _, chat := range chats {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		_, busy := r.active[chat.Key]
		maintenanceBusy := r.Maintenance != nil && r.Maintenance.Busy()
		r.mu.Unlock()
		if busy || maintenanceBusy {
			if busy {
				r.logf("digest skip busy workspace=%s", chat.Key)
			}
			continue
		}
		if err := r.digestChat(ctx, chat.Key, chat.Record); err != nil {
			r.report(err)
			r.logf("digest workspace=%s error: %q", chat.Key, r.Options.Redaction.Text(redaction.Terminal, err.Error()))
		}
	}
	return nil
}

// digestChat distills one chat's new pairs into long-term memory and notifies
// the chat at topic level zero on success.
func (r *Runtime) digestChat(ctx context.Context, key string, rec state.ChatRecord) error {
	mem, err := r.Store.LoadMemory(key)
	if err != nil {
		return err
	}
	var pending []digestTopicPairs
	counts := map[string]int{}
	total := 0
	for _, topic := range rec.Topics {
		convKey := fmt.Sprintf("%s:%d", key, topic)
		conv, err := r.Store.Load(convKey)
		if err != nil {
			return err
		}
		seen := mem.Digested[convKey]
		if seen > len(conv.Pairs) {
			seen = len(conv.Pairs)
		}
		if seen < len(conv.Pairs) {
			pending = append(pending, digestTopicPairs{topic: topic, pairs: conv.Pairs[seen:]})
			total += len(conv.Pairs) - seen
		}
		counts[convKey] = len(conv.Pairs)
	}
	if total == 0 {
		return nil
	}
	route := platform.Message{Platform: rec.Platform, ChatID: rec.ChatID, TopicID: 0, UserID: rec.Owner}
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
	transcript := digestTranscript(pending)
	summary, err := r.distill(ctx, provider, model, thinking, transcript)
	if err != nil {
		return err
	}
	summary = strings.TrimSpace(r.Options.Redaction.Text(redaction.Remote, summary))
	if summary == "" {
		return fmt.Errorf("empty digest summary")
	}
	loc := memoryLocation(r.Config.Memory.Timezone)
	date := time.Now().In(loc).Format("2006-01-02")
	compacted := false
	mem.Entries = append(mem.Entries, state.MemoryEntry{Date: date, Text: summary})
	if r.Config.Memory.MaxBytes > 0 && mem.Bytes() > r.Config.Memory.MaxBytes {
		var cerr error
		mem, compacted, cerr = r.compactMemory(ctx, provider, model, thinking, mem, date)
		if cerr != nil {
			r.report(cerr)
		}
		for r.Config.Memory.MaxBytes > 0 && mem.Bytes() > r.Config.Memory.MaxBytes && len(mem.Entries) > 1 {
			mem.Entries = mem.Entries[1:]
		}
	}
	if err := r.Store.SaveMemory(key, mem); err != nil {
		return err
	}
	if err := r.Store.MarkDigested(key, counts, time.Now()); err != nil {
		return err
	}
	topics := len(pending)
	preamble := fmt.Sprintf("Midnight digest done: distilled %d exchanges across %d topic(s) into long-term memory. Memory %d/%d bytes.",
		total, topics, mem.Bytes(), r.Config.Memory.MaxBytes)
	if compacted {
		preamble += " Older notes were compacted to stay within budget."
	}
	return r.reply(route, preamble+"\n\n"+summary)
}

// distill asks the provider for durable memory notes without tools.
func (r *Runtime) distill(ctx context.Context, provider llm.Provider, model, thinking, transcript string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Config.TaskDuration())
	defer cancel()
	resp, err := provider.Complete(ctx, llm.Request{
		Model:    model,
		Thinking: thinking,
		Messages: []llm.Message{
			{Role: "system", Content: "You distill a day of chat history into durable long-term memory notes. Extract only lasting facts worth remembering: user preferences, decisions made, project context, names, and ongoing topics. Omit greetings, small talk, and routine tool output. Never repeat secrets, tokens, or credentials. Reply with 1-8 short bullet lines using - markers, plain text, no preamble."},
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
	summary := strings.TrimSpace(r.Options.Redaction.Text(redaction.Remote, resp.Message.Content))
	if summary == "" {
		return mem, false, fmt.Errorf("empty compaction summary")
	}
	compacted := state.ChatMemory{
		Entries:      append([]state.MemoryEntry{{Date: date, Text: "Earlier notes (compacted):\n" + summary}}, mem.Entries[len(mem.Entries)-1:]...),
		Digested:     mem.Digested,
		LastDigestAt: mem.LastDigestAt,
	}
	return compacted, true, nil
}

// digestTranscript renders new pairs grouped by topic for distillation,
// bounded so one busy day cannot blow the digest request.
func digestTranscript(pending []digestTopicPairs) string {
	var b strings.Builder
	for _, p := range pending {
		b.WriteString(fmt.Sprintf("Topic %d:\n", p.topic))
		for _, pair := range p.pairs {
			b.WriteString("user: ")
			b.WriteString(pair.User)
			b.WriteString("\nassistant: ")
			b.WriteString(pair.Answer)
			b.WriteString("\n")
		}
	}
	out := b.String()
	if len(out) > digestTranscriptCap {
		out = out[:digestTranscriptCap] + "\n[transcript truncated]"
	}
	return out
}

// memoryHistory prepends capped workspace long-term memory to the topic
// history when the digest feature is enabled. Failures fall back to plain
// history so memory issues never block a chat turn.
func (r *Runtime) memoryHistory(m platform.Message, conv state.Conversation) []llm.Message {
	history := conv.Messages()
	if !r.Config.Memory.Enabled {
		return history
	}
	maxChars := r.Config.Memory.MaxContextBytes
	if maxChars <= 0 {
		maxChars = fallbackMemoryContextBytes
	}
	mem, err := r.Store.LoadMemory(m.WorkspaceKey())
	if err != nil || len(mem.Entries) == 0 {
		return history
	}
	text := r.Options.Redaction.Text(redaction.Remote, memoryText(mem, maxChars))
	if strings.TrimSpace(text) == "" {
		return history
	}
	return append([]llm.Message{{Role: "system", Content: text}}, history...)
}

// memoryText renders workspace memory newest-first within maxChars for chat
// context injection. Empty when nothing is stored.
func memoryText(mem state.ChatMemory, maxChars int) string {
	if len(mem.Entries) == 0 || maxChars <= 0 {
		return ""
	}
	var lines []string
	used := 0
	for i := len(mem.Entries) - 1; i >= 0; i-- {
		line := "- [" + mem.Entries[i].Date + "] " + mem.Entries[i].Text
		if used+len(line) > maxChars && len(lines) > 0 {
			break
		}
		lines = append([]string{line}, lines...)
		used += len(line) + 1
		if used >= maxChars {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Long-term memory for this chat (distilled nightly, newest last):\n" + strings.Join(lines, "\n")
}

// digestLoop runs the startup catch-up sweep, then fires daily at the
// configured wall time in the configured timezone (server-local when empty).
func (r *Runtime) digestLoop(ctx context.Context) {
	if !r.Config.Memory.Enabled {
		return
	}
	loc := memoryLocation(r.Config.Memory.Timezone)
	hour, minute, err := parseDigestAtLocal(r.Config.Memory.DigestAt)
	if err != nil {
		r.report(err)
		return
	}
	r.report(r.digestSweep(ctx))
	timer := time.NewTimer(untilNextDigest(time.Now(), loc, hour, minute))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.report(r.digestSweep(ctx))
			timer.Reset(untilNextDigest(time.Now(), loc, hour, minute))
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
