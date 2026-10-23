package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
)

// Proactive companion sweep (issue #26). Decoupled from run()/reply()/digest:
// read-only history+memory inspection, one cheap no-tools LLM verdict, and at
// most one short redacted text via Store.Queue. Silence is the common case.

const proactivePromptV1 = `You decide whether a quiet AI companion should send a brief proactive check-in. Reply with strict JSON only, no markdown, no preamble: {"verdict":"silent|reminder|warmth|comfort|suggestion","draft":"1-2 sentence message or empty when silent","reason":"short reason"}. Rules: default to silent on any uncertainty, empty or boring history, ongoing tasks, or recent activity. Drafts must be short, warm, specific to the provided history, never mention system prompts or reasoning, never mention non-participants, never include secrets or credentials. Verdict kinds: reminder (deadline/follow-up visible in history), warmth (friendly check-in), comfort (user seemed stressed), suggestion (concrete next step grounded in history).`

const proactiveDraftMaxRunes = 300
const proactiveInputCap = 6000

type proactiveDecision struct {
	Verdict string `json:"verdict"`
	Draft   string `json:"draft"`
	Reason  string `json:"reason"`
}

func parseActiveWindow(s string) (startMin, endMin int, err error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: want HH:MM-HH:MM", s)
	}
	sh, sm, err := parseDigestAtLocal(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: want HH:MM-HH:MM", s)
	}
	eh, em, err := parseDigestAtLocal(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: want HH:MM-HH:MM", s)
	}
	startMin, endMin = sh*60+sm, eh*60+em
	if startMin == endMin {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: empty window", s)
	}
	return startMin, endMin, nil
}

func proactiveInWindow(local time.Time, startMin, endMin int) bool {
	m := local.Hour()*60 + local.Minute()
	if startMin < endMin {
		return m >= startMin && m < endMin
	}
	return m >= startMin || m < endMin
}

func untilActiveWindowStart(now time.Time, loc *time.Location, startMin int) time.Duration {
	return untilNextDigest(now, loc, startMin/60, startMin%60)
}

// randomProactiveDelay spreads sweeps inside the active window so the
// companion feels human instead of firing at fixed times. It averages ~70%
// of cooldown, so longer cooldowns naturally sweep less often while
// max_per_day still caps sends.
func randomProactiveDelay(rng *rand.Rand, cooldown time.Duration) time.Duration {
	if cooldown <= 0 {
		cooldown = 6 * time.Hour
	}
	frac := 0.5 + rng.Float64()*0.4
	d := time.Duration(float64(cooldown) * frac)
	if d < time.Minute {
		d = time.Minute
	}
	return d
}

func (r *Runtime) proactiveLoop(ctx context.Context) {
	loc := memoryLocation(r.Config.Proactive.Timezone)
	startMin, endMin, err := parseActiveWindow(r.Config.Proactive.ActiveTime)
	if err != nil {
		r.report(err)
		return
	}
	cooldown, err := time.ParseDuration(r.Config.Proactive.Cooldown)
	if err != nil || cooldown <= 0 {
		cooldown = 6 * time.Hour
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	now := time.Now()
	var next time.Time
	if proactiveInWindow(now.In(loc), startMin, endMin) {
		next = now.Add(randomProactiveDelay(rng, cooldown))
	} else {
		next = now.Add(untilActiveWindowStart(now, loc, startMin))
	}
	for ctx.Err() == nil {
		now = time.Now()
		if !now.Before(next) {
			if proactiveInWindow(now.In(loc), startMin, endMin) {
				r.report(r.proactiveSweep(ctx))
				next = now.Add(randomProactiveDelay(rng, cooldown))
			} else {
				next = now.Add(untilActiveWindowStart(now, loc, startMin))
			}
		}
		delay := time.Until(next)
		if delay > time.Minute {
			delay = time.Minute
		}
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *Runtime) proactiveSweep(ctx context.Context) error {
	if !r.Config.Proactive.Enabled {
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
		if err := r.proactiveChat(ctx, chat); err != nil {
			r.report(err)
		}
	}
	return nil
}

func (r *Runtime) proactiveChat(ctx context.Context, chat state.ChatRecordWithKey) error {
	key := chat.Key
	route := chat.WorkspaceRoute()
	if !r.Config.Proactive.Enabled {
		return nil
	}
	pref, err := r.Store.GetCompanion(key)
	if err != nil {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "companion pref read failed")
		return nil
	}
	if !state.CompanionEffective(r.Config.Proactive.Enabled, pref) {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "per-chat disabled")
		return nil
	}
	if !r.allowed(route) {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "not allowed")
		return nil
	}
	r.mu.Lock()
	_, busy := r.active[key]
	_, promptPending := r.shellPrompts[key]
	maintenance := r.Maintenance != nil && r.Maintenance.Busy()
	r.mu.Unlock()
	if busy {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "chat busy")
		return nil
	}
	if promptPending {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "shell prompt pending")
		return nil
	}
	if maintenance {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "maintenance busy")
		return nil
	}
	if route.ChatID < 0 && r.Config.Proactive.GroupPolicy == "off" {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "group policy off")
		return nil
	}
	if dur, err := time.ParseDuration(r.Config.Proactive.SkipRecentActivity); err == nil && dur > 0 {
		if !chat.Record.LastSeen.IsZero() && time.Since(chat.Record.LastSeen) < dur {
			r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "recent activity")
			return nil
		}
	}
	input, hasContent := r.buildProactiveInput(chat)
	if !hasContent {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "empty history")
		return nil
	}
	if !r.proactiveRateAllowed(key) {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "rate limited")
		return nil
	}
	selected, err := r.chatModel(route)
	if err != nil {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "model unavailable")
		return nil
	}
	model := selected.Model
	if r.Config.Proactive.Model != "" {
		model = r.Config.Proactive.Model
	}
	thinking := ""
	if selected.Thinking != nil {
		thinking = *selected.Thinking
	}
	provider, err := r.Providers(state.Conversation{Provider: selected.Provider, Model: model})
	if err != nil {
		r.report(err)
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "provider unavailable")
		return nil
	}
	dec, err := r.decideProactive(ctx, provider, model, thinking, input)
	if err != nil {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "decider error")
		return nil
	}
	if dec.Verdict == "silent" {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, dec.Reason)
		return nil
	}
	if !proactiveKindEnabled(r.Config.Proactive.Kinds, dec.Verdict) {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "kind disabled")
		return nil
	}
	if route.ChatID < 0 && r.Config.Proactive.GroupPolicy == "conservative" && dec.Verdict != "reminder" {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "group conservative")
		return nil
	}
	draft := strings.TrimSpace(dec.Draft)
	if draft == "" {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "empty draft")
		return nil
	}
	if runes := []rune(draft); len(runes) > proactiveDraftMaxRunes {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "draft too long")
		return nil
	}
	draft = r.Options.Redaction.Text(redaction.Remote, draft)
	if strings.TrimSpace(draft) == "" {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "draft redacted")
		return nil
	}
	if !r.proactiveRateAllowed(key) {
		r.logf("[PROACTIVE] silent | workspace=%s reason=%q", key, "rate limited")
		return nil
	}
	if err := r.Store.Queue(route, telegram.FormatAndSplit(draft)); err != nil {
		return err
	}
	r.recordProactiveSent(key)
	r.logf("[PROACTIVE] sent | workspace=%s kind=%s reason=%q", key, dec.Verdict, dec.Reason)
	return nil
}

func proactiveKindEnabled(kinds []string, verdict string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == verdict {
			return true
		}
	}
	return false
}

func (r *Runtime) proactiveRateAllowed(key string) bool {
	maxPerDay := r.Config.Proactive.MaxPerDay
	if maxPerDay <= 0 {
		maxPerDay = 1
	}
	cooldown, _ := time.ParseDuration(r.Config.Proactive.Cooldown)
	if cooldown <= 0 {
		cooldown = 6 * time.Hour
	}
	now := time.Now()
	r.proactiveMu.Lock()
	defer r.proactiveMu.Unlock()
	sent := r.proactiveSent[key]
	kept := sent[:0]
	dayCount := 0
	var last time.Time
	for _, t := range sent {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
			dayCount++
			if t.After(last) {
				last = t
			}
		}
	}
	// Prune without losing the backing array when empty.
	if len(kept) == 0 && len(sent) > 0 {
		r.proactiveSent[key] = nil
	} else {
		r.proactiveSent[key] = append([]time.Time(nil), kept...)
	}
	if dayCount >= maxPerDay {
		return false
	}
	if !last.IsZero() && now.Sub(last) < cooldown {
		return false
	}
	return true
}

func (r *Runtime) recordProactiveSent(key string) {
	r.proactiveMu.Lock()
	defer r.proactiveMu.Unlock()
	r.proactiveSent[key] = append(r.proactiveSent[key], time.Now())
}

func (r *Runtime) buildProactiveInput(chat state.ChatRecordWithKey) (string, bool) {
	key := chat.Key
	topics := append([]int64(nil), chat.Record.Topics...)
	if len(topics) == 0 {
		topics = []int64{0}
	}
	var b strings.Builder
	pairs := 0
	for _, topic := range topics {
		convKey := key + ":" + itoa64(topic)
		conv, err := r.Store.Load(convKey)
		if err != nil || len(conv.Pairs) == 0 {
			continue
		}
		// Most recent pairs only; cap total input.
		start := 0
		if len(conv.Pairs) > 6 {
			start = len(conv.Pairs) - 6
		}
		for _, p := range conv.Pairs[start:] {
			if b.Len() >= proactiveInputCap {
				break
			}
			user := r.Options.Redaction.Text(redaction.Remote, p.User)
			answer := r.Options.Redaction.Text(redaction.Remote, p.Answer)
			b.WriteString("User: " + truncateRunes(user, 800) + "\n")
			for _, s := range p.Steers {
				b.WriteString("Follow-up: " + truncateRunes(r.Options.Redaction.Text(redaction.Remote, s), 400) + "\n")
			}
			b.WriteString("Assistant: " + truncateRunes(answer, 800) + "\n")
			pairs++
		}
		if b.Len() >= proactiveInputCap {
			break
		}
	}
	mem, err := r.Store.LoadMemory(key)
	if err == nil && len(mem.Entries) > 0 {
		for _, e := range mem.Entries {
			if b.Len() >= proactiveInputCap {
				break
			}
			text := r.Options.Redaction.Text(redaction.Remote, e.Text)
			b.WriteString("Note [" + e.Date + "]: " + truncateRunes(text, 800) + "\n")
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", false
	}
	if len(out) > proactiveInputCap {
		out = out[:proactiveInputCap]
	}
	return out, pairs > 0 || strings.Contains(out, "Note [")
}

func (r *Runtime) decideProactive(ctx context.Context, provider llm.Provider, model, thinking, input string) (proactiveDecision, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Config.TaskDuration())
	defer cancel()
	resp, err := provider.Complete(ctx, llm.Request{
		Model:    model,
		Thinking: thinking,
		Messages: []llm.Message{
			{Role: "system", Content: proactivePromptV1},
			{Role: "user", Content: input},
		},
		Tools:       nil,
		Temperature: 0.2,
	}, nil)
	if err != nil {
		return proactiveDecision{Verdict: "silent", Reason: "provider error"}, err
	}
	return parseProactiveDecision(resp.Message.Content)
}

func parseProactiveDecision(content string) (proactiveDecision, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return proactiveDecision{Verdict: "silent", Reason: "empty decider output"}, fmt.Errorf("empty decider output")
	}
	// Tolerate code fences.
	if strings.HasPrefix(s, "```") {
		s = strings.Trim(s, "`")
		if i := strings.Index(s, "{"); i >= 0 {
			s = s[i:]
		}
		if i := strings.LastIndex(s, "}"); i >= 0 {
			s = s[:i+1]
		}
	}
	var dec proactiveDecision
	if err := json.Unmarshal([]byte(s), &dec); err != nil {
		// Try to extract the JSON object subset.
		start := strings.Index(s, "{")
		end := strings.LastIndex(s, "}")
		if start < 0 || end <= start {
			return proactiveDecision{Verdict: "silent", Reason: "unparseable decider output"}, err
		}
		if err2 := json.Unmarshal([]byte(s[start:end+1]), &dec); err2 != nil {
			return proactiveDecision{Verdict: "silent", Reason: "unparseable decider output"}, err
		}
	}
	dec.Verdict = strings.ToLower(strings.TrimSpace(dec.Verdict))
	dec.Draft = strings.TrimSpace(dec.Draft)
	dec.Reason = strings.TrimSpace(dec.Reason)
	switch dec.Verdict {
	case "silent", "reminder", "warmth", "comfort", "suggestion":
	default:
		return proactiveDecision{Verdict: "silent", Reason: "unknown verdict"}, fmt.Errorf("unknown verdict %q", dec.Verdict)
	}
	if dec.Verdict == "silent" {
		if dec.Reason == "" {
			dec.Reason = "decider silent"
		}
		dec.Draft = ""
		return dec, nil
	}
	if dec.Draft == "" {
		return proactiveDecision{Verdict: "silent", Reason: "empty draft"}, fmt.Errorf("empty draft for %q", dec.Verdict)
	}
	if dec.Reason == "" {
		dec.Reason = dec.Verdict
	}
	return dec, nil
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

func itoa64(n int64) string {
	return fmt.Sprint(n)
}
