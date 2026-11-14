package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Recurrences work even in minimal host installations.

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

const ScheduleGrace = 5 * time.Minute
const maxSchedules = 100
const maxQueuedRuns = 1000
const retainedRuns = 1000

// ScheduleLedgerVersion marks schedules.json files migrated for issue #64.
// Version 0 is the legacy file (no version field). Version 1 adds the
// additive Deleted tombstones used for the live ledger anti-resurrection
// block. New fields stay omitempty so downgraded binaries ignore them
// instead of deleting schedules.
const ScheduleLedgerVersion = 1

// recentDeleteWindow is how long a deleted schedule stays in the live-ledger
// "recently deleted" block so the model does not recreate it from past
// discussion or long-term memory without an explicit user request.
const recentDeleteWindow = 24 * time.Hour

// tombstoneRetention bounds schedules.json growth: tombstones older than
// this are pruned on save. The 24h anti-resurrection window is a subset.
const tombstoneRetention = 7 * 24 * time.Hour

// Retry policy for scheduled agent tasks: at most 3 retries after the
// initial failure, each with a longer delay than the last.
const MaxScheduleRetries = 3

// ScheduleRetryDelays controls backoff: index 0 is the delay before retry 1.
var ScheduleRetryDelays = []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute}

func scheduleRetryDelay(retry int) time.Duration {
	if retry >= 1 && retry <= len(ScheduleRetryDelays) {
		return ScheduleRetryDelays[retry-1]
	}
	if len(ScheduleRetryDelays) > 0 {
		return ScheduleRetryDelays[len(ScheduleRetryDelays)-1]
	}
	return 45 * time.Minute
}

type Schedule struct {
	ID        string           `json:"id"`
	Route     platform.Message `json:"route"`
	Kind      string           `json:"kind"` // reminder or agent
	Text      string           `json:"text"`
	At        time.Time        `json:"at"`
	Repeat    string           `json:"repeat"` // once, daily, weekly
	Timezone  string           `json:"timezone"`
	NextRunAt time.Time        `json:"next_run_at"`
	Missed    int              `json:"missed"`
}

type ScheduledRun struct {
	ID          string            `json:"id"`
	ScheduleID  string            `json:"schedule_id"`
	Route       platform.Message  `json:"route"`
	Kind        string            `json:"kind"`
	Text        string            `json:"text"`
	ScheduledAt time.Time         `json:"scheduled_at"`
	Status      string            `json:"status"`
	Parts       []string          `json:"parts,omitempty"`
	Photos      []PhotoAttachment `json:"photos,omitempty"`
	Next        int               `json:"next"`
	// Attempts counts failed executions so far (0 = first try not yet failed).
	// RetryAt gates a pending retry: dispatch must wait until now >= RetryAt.
	Attempts int       `json:"attempts,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
}

// Total counts deliverable items: photos first, then text parts.
func (r ScheduledRun) Total() int { return len(r.Photos) + len(r.Parts) }

type scheduleState struct {
	Version   int               `json:"version,omitempty"`
	Schedules []Schedule        `json:"schedules"`
	Runs      []ScheduledRun    `json:"runs"`
	Deleted   []DeletedSchedule `json:"deleted,omitempty"`
}

// DeletedSchedule is a tombstone for issue #64 anti-resurrection. It records
// enough to warn the model ("recently deleted, do not recreate without an
// explicit request") and to scrub matching long-term notes. Route scopes the
// tombstone to the owning chat/user so cross-chat deletes never interfere.
type DeletedSchedule struct {
	Route       platform.Message `json:"route"`
	TextHash    string           `json:"text_hash"`
	TextPreview string           `json:"text_preview,omitempty"`
	DeletedAt   time.Time        `json:"deleted_at"`
	Repeat      string           `json:"repeat,omitempty"`
	Kind        string           `json:"kind,omitempty"`
}

// Copy before mutation: failed writes must not advance in-memory state.
func (s *Store) cloneSchedules() scheduleState {
	return scheduleState{
		Version:   s.schedules.Version,
		Schedules: append([]Schedule(nil), s.schedules.Schedules...),
		Runs:      append([]ScheduledRun(nil), s.schedules.Runs...),
		Deleted:   append([]DeletedSchedule(nil), s.schedules.Deleted...),
	}
}
func (s *Store) saveSchedules(next scheduleState) error {
	// Keep bounded terminal history, but never discard work or undelivered output.
	terminals := 0
	for _, r := range next.Runs {
		if !runActive(r) && r.Next == r.Total() {
			terminals++
		}
	}
	if terminals > retainedRuns {
		kept := make([]ScheduledRun, 0, len(next.Runs))
		for _, r := range next.Runs {
			if terminals > retainedRuns && !runActive(r) && r.Next == len(r.Parts) {
				terminals--
				continue
			}
			kept = append(kept, r)
		}
		next.Runs = kept
	}
	// Bound tombstones so schedules.json stays small. The 24h
	// anti-resurrection window is a subset of this retention.
	if len(next.Deleted) > 0 {
		cutoff := time.Now().Add(-tombstoneRetention)
		kept := next.Deleted[:0]
		for _, d := range next.Deleted {
			if d.DeletedAt.IsZero() || d.DeletedAt.After(cutoff) {
				kept = append(kept, d)
			}
		}
		// Clear tail so pruned tombstones do not linger via backing array.
		for i := len(kept); i < len(next.Deleted); i++ {
			next.Deleted[i] = DeletedSchedule{}
		}
		next.Deleted = kept
	}
	if next.Version == 0 {
		next.Version = ScheduleLedgerVersion
	}
	if err := Atomic(filepath.Join(s.dir, "schedules.json"), next); err != nil {
		return err
	}
	s.schedules = next
	return nil
}
func runActive(r ScheduledRun) bool { return r.Status == "pending" || r.Status == "running" }

// normalizeScheduleText folds a schedule/memory text for fuzzy comparison:
// lowercase, trim, collapse whitespace. Empty stays empty.
func normalizeScheduleText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// scheduleTextHash is the tombstone key for issue #64 anti-resurrection.
// It hashes the normalized text so near-identical casing/whitespace still
// matches.
func scheduleTextHash(s string) string {
	sum := sha256.Sum256([]byte(normalizeScheduleText(s)))
	return hex.EncodeToString(sum[:])
}

func scheduleSignificantTokens(s string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(s)) {
		// Strip common punctuation at the edges; keep the core token.
		w = strings.Trim(w, ".,;:!?\"'()[]{}<>-_/\\@#*+=|~`")
		if len(w) < 3 {
			continue
		}
		out = append(out, w)
	}
	return out
}

// scheduleTextsMatch reports exact, substring, or near-duplicate matches.
// It powers single-"stop" removal of near-duplicates (cause #4) and memory
// scrubbing (cause #6): one word or one minute of difference still matches
// when the token overlap is high.
func scheduleTextsMatch(a, b string) bool {
	na, nb := normalizeScheduleText(a), normalizeScheduleText(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	if strings.Contains(na, nb) || strings.Contains(nb, na) {
		return true
	}
	ta, tb := scheduleSignificantTokens(na), scheduleSignificantTokens(nb)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	set := make(map[string]bool, len(ta))
	for _, w := range ta {
		set[w] = true
	}
	common := 0
	for _, w := range tb {
		if set[w] {
			common++
			// Count each distinct token once.
			delete(set, w)
		}
	}
	if common >= 3 {
		return true
	}
	minLen := len(ta)
	if len(tb) < minLen {
		minLen = len(tb)
	}
	if minLen <= 3 {
		return common >= 2
	}
	// For longer texts, half the shorter side overlapping counts as near-duplicate.
	return common >= 2 && common*2 >= minLen
}

func sanitizeScheduleLine(s string) string {
	// Strip CR/LF/TAB so a schedule text cannot fabricate extra ledger lines
	// like "- deadbeef | agent daily | next ...". Collapse whitespace.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}

func schedulePreviewText(text string) string {
	text = sanitizeScheduleLine(text)
	chars := []rune(text)
	if len(chars) > 40 {
		return string(chars[:40]) + "…"
	}
	return text
}

// scheduleMemoryMatches is the conservative matcher for anti-resurrection
// scrubbing (issue #64 cause #6). Unlike scheduleTextsMatch (used for
// delete-by-text), it requires exact normalized equality or a normalized
// substring with a minimum length, so deleting "Remind Alex about the Acme
// Corp invoice" does not erase "User's name is Alex; they work at Acme Corp".
// Callers restrict scrubbing to recurring agent tasks as documented.
func scheduleMemoryMatches(entryText, scheduleText string) bool {
	na, nt := normalizeScheduleText(entryText), normalizeScheduleText(scheduleText)
	if na == "" || nt == "" {
		return false
	}
	if na == nt {
		return true
	}
	if len(nt) >= 10 && strings.Contains(na, nt) {
		return true
	}
	if len(na) >= 10 && strings.Contains(nt, na) {
		return true
	}
	return false
}

// isLiveSchedule reports whether a stored schedule belongs in the live
// ledger. One-shot schedules whose NextRunAt already advanced to zero are
// done: they must not linger as "live" and must not be re-created.
func isLiveSchedule(v Schedule) bool {
	if v.Repeat == "once" {
		return !v.NextRunAt.IsZero()
	}
	return true
}

func validateSchedule(v Schedule) error {
	if v.Kind != "reminder" && v.Kind != "agent" {
		return fmt.Errorf("kind must be reminder or agent")
	}
	if strings.TrimSpace(v.Text) == "" || len(v.Text) > 8192 {
		return fmt.Errorf("text must contain 1–8192 bytes")
	}
	if v.At.IsZero() {
		return fmt.Errorf("at is required")
	}
	if v.Repeat != "once" && v.Repeat != "daily" && v.Repeat != "weekly" {
		return fmt.Errorf("repeat must be once, daily, or weekly")
	}
	if v.Timezone == "" {
		return fmt.Errorf("IANA timezone is required")
	}
	if _, err := time.LoadLocation(v.Timezone); err != nil {
		return fmt.Errorf("invalid timezone: %w", err)
	}
	if v.Route.Platform != "telegram" || v.Route.ChatID == 0 || v.Route.UserID == 0 {
		return fmt.Errorf("invalid schedule owner")
	}
	return nil
}

// nextOccurrence preserves local wall time, not elapsed 24-hour intervals.
// Nonexistent DST wall times are skipped; an ambiguous wall time fires once.
func nextOccurrence(v Schedule, after time.Time) time.Time {
	if v.Repeat == "once" {
		return time.Time{}
	}
	loc, _ := time.LoadLocation(v.Timezone)
	anchor, local := v.At.In(loc), after.In(loc)
	for day := 0; day <= 8; day++ {
		date := time.Date(local.Year(), local.Month(), local.Day()+day, 12, 0, 0, 0, loc)
		if v.Repeat == "weekly" && date.Weekday() != anchor.Weekday() {
			continue
		}
		candidate := time.Date(date.Year(), date.Month(), date.Day(), anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), loc)
		if candidate.Hour() != anchor.Hour() || candidate.Minute() != anchor.Minute() {
			continue
		}
		if candidate.After(after) && !candidate.Before(v.At) {
			return candidate.UTC()
		}
	}
	return time.Time{}
}

func (s *Store) CreateSchedule(v Schedule, now time.Time) (Schedule, error) {
	return s.createScheduleLocked(v, now, false)
}

// CreateScheduleConfirmed recreates a recently deleted exact-text schedule
// after the user explicitly asks in the current message. The 24h tombstone
// is consumed atomically with the create, so a failed create (past at, bad
// timezone, live cap) does not burn the guard.
func (s *Store) CreateScheduleConfirmed(v Schedule, now time.Time) (Schedule, error) {
	return s.createScheduleLocked(v, now, true)
}

func (s *Store) createScheduleLocked(v Schedule, now time.Time, confirmed bool) (Schedule, error) {
	if v.Repeat == "" {
		v.Repeat = "once"
	}
	if err := validateSchedule(v); err != nil {
		return Schedule{}, err
	}
	if !v.At.After(now) {
		return Schedule{}, fmt.Errorf("at must be in the future")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	// Harness-owned anti-resurrection guard for recurring agent tasks only
	// (reminder/once recreation is routine): an exact-text tombstone younger
	// than 24h in this chat blocks silent re-creation from past discussion
	// or memory. With confirmed=true the matching tombstones are consumed
	// in the same atomic save as the new schedule.
	if v.Kind == "agent" && v.Repeat != "once" {
		wantHash := scheduleTextHash(s.policy.Text(redaction.Persistence, v.Text))
		if d, ok := findRecentTombstoneLocked(next.Deleted, v.Route, wantHash, now); ok {
			if !confirmed {
				return Schedule{}, fmt.Errorf("similar schedule was recently deleted (%s, deleted %s); confirm explicitly to recreate", sanitizeScheduleLine(d.TextPreview), d.DeletedAt.UTC().Format(time.RFC3339))
			}
			kept := make([]DeletedSchedule, 0, len(next.Deleted))
			for _, t := range next.Deleted {
				if t.TextHash == wantHash && t.Kind == "agent" && t.Repeat != "once" && sameChatUser(t.Route, v.Route) && now.Sub(t.DeletedAt) <= recentDeleteWindow {
					continue
				}
				kept = append(kept, t)
			}
			next.Deleted = kept
		}
	}
	count := 0
	for _, old := range next.Schedules {
		if sameOwner(old.Route, v.Route) && old.Kind == v.Kind && old.Text == s.policy.Text(redaction.Persistence, v.Text) && old.At.Equal(v.At) && old.Repeat == v.Repeat && old.Timezone == v.Timezone {
			return old, nil
		}
		// Cap counts live schedules only so fired one-shots do not block the
		// topic forever (done one-shots have NextRunAt zero).
		if old.Route.ConversationKey() == v.Route.ConversationKey() && isLiveSchedule(old) {
			count++
		}
	}
	if count >= maxSchedules {
		return Schedule{}, fmt.Errorf("at most %d schedules per conversation", maxSchedules)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return Schedule{}, err
	}
	v.ID = hex.EncodeToString(random)
	v.Text = s.policy.Text(redaction.Persistence, v.Text)
	v.At = v.At.UTC()
	v.NextRunAt = v.At
	v.Missed = 0
	v.Route.Text = ""
	v.Route.UpdateID = 0
	v.Route.MessageID = 0
	next.Schedules = append(next.Schedules, v)
	if err := s.saveSchedules(next); err != nil {
		return Schedule{}, err
	}
	return v, nil
}

// ListSchedules and DeleteSchedule are scoped to both topic and creating user.
func sameOwner(a, b platform.Message) bool {
	return a.ConversationKey() == b.ConversationKey() && a.UserID == b.UserID
}

// sameChatUser groups schedules for the cross-topic hint: same chat and
// same creating user, regardless of topic. Memory is chat-wide, so a stop
// said in the wrong topic must name the owning topic instead of claiming
// success.
func sameChatUser(a, b platform.Message) bool {
	return a.Platform == b.Platform && a.ChatID == b.ChatID && a.UserID == b.UserID
}

// The filter helpers below are lock-free over a snapshot slice so public
// methods (which lock) and ledger rendering (which already holds the lock)
// share one implementation instead of duplicating scope predicates.
func filterLiveSchedules(all []Schedule, owner platform.Message) []Schedule {
	var out []Schedule
	for _, v := range all {
		if sameOwner(v.Route, owner) && isLiveSchedule(v) {
			out = append(out, v)
		}
	}
	return out
}

func filterMatchingSchedules(all []Schedule, owner platform.Message, query string) []Schedule {
	var out []Schedule
	q := normalizeScheduleText(query)
	for _, v := range all {
		if !sameOwner(v.Route, owner) || !isLiveSchedule(v) {
			continue
		}
		if q == "" || scheduleTextsMatch(v.Text, query) {
			out = append(out, v)
		}
	}
	return out
}

func countCrossTopicSchedules(all []Schedule, owner platform.Message) (int, []int64, map[int64]int) {
	byTopic := map[int64]int{}
	for _, v := range all {
		if !isLiveSchedule(v) || !sameChatUser(v.Route, owner) {
			continue
		}
		if v.Route.ConversationKey() == owner.ConversationKey() {
			continue
		}
		byTopic[v.Route.TopicID]++
	}
	topics := make([]int64, 0, len(byTopic))
	total := 0
	for t, n := range byTopic {
		topics = append(topics, t)
		total += n
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i] < topics[j] })
	return total, topics, byTopic
}
func (s *Store) ListSchedules(owner platform.Message) ([]Schedule, []ScheduledRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	schedules := []Schedule{}
	runs := []ScheduledRun{}
	for _, v := range s.schedules.Schedules {
		if sameOwner(v.Route, owner) {
			schedules = append(schedules, v)
		}
	}
	for _, v := range s.schedules.Runs {
		if sameOwner(v.Route, owner) {
			v.Parts = append([]string(nil), v.Parts...)
			v.Photos = append([]PhotoAttachment(nil), v.Photos...)
			runs = append(runs, v)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].ScheduledAt.Before(runs[j].ScheduledAt) })
	if len(runs) > 20 {
		runs = runs[len(runs)-20:]
	}
	return schedules, runs
}

// LiveSchedules returns only ledger-live schedules for this topic+user:
// recurring schedules always, one-shots only while NextRunAt is still set.
// Done one-shots (NextRunAt zero) are excluded so they do not linger as
// "live" and cannot be resurrected.
func (s *Store) LiveSchedules(owner platform.Message) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return filterLiveSchedules(s.schedules.Schedules, owner)
}

// CrossTopicScheduleInfo counts live schedules in the same chat for the same
// user but in other topics. It powers the scope hint: stop said in the wrong
// topic must name the owning topic instead of claiming success.
func (s *Store) CrossTopicScheduleInfo(owner platform.Message) (int, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total, topics, _ := countCrossTopicSchedules(s.schedules.Schedules, owner)
	return total, topics
}

// FindMatchingSchedules returns live schedules in this topic+user whose text
// matches the query (exact, substring, or near-duplicate token overlap). An
// empty query matches all live schedules so one "stop" can clear
// near-duplicates without N round trips.
func (s *Store) FindMatchingSchedules(owner platform.Message, query string) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return filterMatchingSchedules(s.schedules.Schedules, owner, query)
}

// ownerTopicForSchedule finds the owning topic of a schedule ID regardless of
// caller scope, for "wrong topic" error hints. Returns false when unknown.
func (s *Store) ownerTopicForSchedule(id string, owner platform.Message) (platform.Message, bool) {
	for _, v := range s.schedules.Schedules {
		if v.ID != id {
			continue
		}
		if sameChatUser(v.Route, owner) {
			return v.Route, true
		}
		return v.Route, false
	}
	return platform.Message{}, false
}

func scheduleExistsLocked(schedules []Schedule, id string) bool {
	for _, v := range schedules {
		if v.ID == id {
			return true
		}
	}
	return false
}

// RecentTombstoneMatch reports whether an exact-text tombstone younger than
// 24h exists in this chat for the same user. It backs the create-time guard
// so TextHash is compared, not merely written. Only recurring agent
// tombstones are considered; reminder/once recreation is routine.
func (s *Store) RecentTombstoneMatch(owner platform.Message, text string) (DeletedSchedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return findRecentTombstoneLocked(s.schedules.Deleted, owner, scheduleTextHash(text), time.Now())
}

func findRecentTombstoneLocked(deleted []DeletedSchedule, owner platform.Message, wantHash string, now time.Time) (DeletedSchedule, bool) {
	for _, d := range deleted {
		if d.Kind != "agent" || d.Repeat == "once" {
			continue
		}
		if now.Sub(d.DeletedAt) > recentDeleteWindow {
			continue
		}
		// Tombstones are chat-scoped (sameChatUser) while the ledger display
		// is topic-scoped (sameOwner): stopping in one topic guards
		// silent recreation from any sibling topic in the same chat.
		if !sameChatUser(d.Route, owner) {
			continue
		}
		if d.TextHash == wantHash {
			return d, true
		}
	}
	return DeletedSchedule{}, false
}

// ClearRecentTombstones removes 24h tombstones matching the exact text in this
// chat for the same user. It is a low-level helper for direct Store users;
// the schedule_create confirm_recreate path uses CreateScheduleConfirmed so
// the guard is consumed atomically with the create.
func (s *Store) ClearRecentTombstones(owner platform.Message, text string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := scheduleTextHash(text)
	kept := make([]DeletedSchedule, 0, len(s.schedules.Deleted))
	removed := 0
	for _, d := range s.schedules.Deleted {
		if d.TextHash == want && sameChatUser(d.Route, owner) && time.Since(d.DeletedAt) <= recentDeleteWindow {
			removed++
			continue
		}
		kept = append(kept, d)
	}
	if removed == 0 {
		return 0, nil
	}
	next := s.cloneSchedules()
	next.Deleted = kept
	if err := s.saveSchedules(next); err != nil {
		return 0, err
	}
	return removed, nil
}

// ScheduleLedgerText renders the harness-owned live-ledger block that is
// auto-injected before every task in this topic (issue #64 proposal §2).
// It lists live IDs with one-line summaries plus recently deleted texts
// (do-not-recreate guard) and a cross-topic hint when this topic is empty
// but siblings are not. Empty returns "" so the caller omits the block,
// exactly like the check-in reference today.
func (s *Store) ScheduleLedgerText(owner platform.Message) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := filterLiveSchedules(s.schedules.Schedules, owner)
	sort.Slice(live, func(i, j int) bool {
		if live[i].NextRunAt.Equal(live[j].NextRunAt) {
			return live[i].ID < live[j].ID
		}
		return live[i].NextRunAt.Before(live[j].NextRunAt)
	})
	now := time.Now()
	var recent []DeletedSchedule
	for _, d := range s.schedules.Deleted {
		if now.Sub(d.DeletedAt) > recentDeleteWindow {
			continue
		}
		if !sameOwner(d.Route, owner) {
			continue
		}
		recent = append(recent, d)
	}
	otherCount, _, otherTopics := countCrossTopicSchedules(s.schedules.Schedules, owner)
	if len(live) == 0 && len(recent) == 0 && otherCount == 0 {
		return ""
	}
	var b strings.Builder
	if len(live) > 0 {
		fmt.Fprintf(&b, "Live schedules in this topic (%d):\n", len(live))
		shown := live
		truncated := 0
		if len(shown) > 20 {
			truncated = len(shown) - 20
			shown = shown[:20]
		}
		for _, v := range shown {
			next := v.NextRunAt.UTC().Format(time.RFC3339)
			if v.NextRunAt.IsZero() {
				next = "done"
			}
			fmt.Fprintf(&b, "- %s | %s %s | next %s | %s\n", v.ID, v.Kind, v.Repeat, next, schedulePreviewText(v.Text))
		}
		if truncated > 0 {
			fmt.Fprintf(&b, "+ %d more (use schedule_list to see all).\n", truncated)
		}
	} else {
		b.WriteString("No live schedules in this topic.\n")
	}
	if otherCount > 0 {
		topics := make([]int64, 0, len(otherTopics))
		for t := range otherTopics {
			topics = append(topics, t)
		}
		sort.Slice(topics, func(i, j int) bool { return topics[i] < topics[j] })
		fmt.Fprintf(&b, "Note: %d schedule(s) exist in other topic(s) of this chat for you:", otherCount)
		for _, t := range topics {
			fmt.Fprintf(&b, " topic %d (%d)", t, otherTopics[t])
		}
		b.WriteString(". They are invisible to schedule_list here; go there or delete there.\n")
	}
	if len(recent) > 0 {
		sort.Slice(recent, func(i, j int) bool { return recent[i].DeletedAt.After(recent[j].DeletedAt) })
		b.WriteString("Recently deleted in this topic (do not recreate unless the current message explicitly asks):\n")
		shownRecent := recent
		truncatedRecent := 0
		if len(shownRecent) > 20 {
			truncatedRecent = len(shownRecent) - 20
			shownRecent = shownRecent[:20]
		}
		for _, d := range shownRecent {
			fmt.Fprintf(&b, "- %s %s | deleted %s | %s\n", d.Kind, d.Repeat, d.DeletedAt.UTC().Format(time.RFC3339), schedulePreviewText(d.TextPreview))
		}
		if truncatedRecent > 0 {
			fmt.Fprintf(&b, "+ %d more recently deleted (kept 7d).\n", truncatedRecent)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// DeleteSchedulesResult reports a deterministic multi-delete for issue #64
// acceptance: which IDs died, how many pending runs were cancelled, and how
// many long-term notes were scrubbed. Running attempts are cancelled when
// their parent schedule is gone (see ClaimRun/FinishRun/RecoverRuns guards);
// pending runs are always cancelled. NotFound lists requested IDs that were
// not live in this topic (for partial-delete proof).
type DeleteSchedulesResult struct {
	Deleted               []Schedule
	PendingCancelled      int
	MemoryNotesRemoved    int
	MemoryPendingRemoved  int
	MemoryRemovedPreviews []string
	NotFound              []string
}

// DeleteSchedules atomically deletes every listed ID owned by this
// topic+user, cancels their pending runs, appends tombstones, and scrubs
// matching long-term notes + pending journal sources so the next digest
// cannot resurrect them. It returns the deleted schedules for reply proof.
// Partial deletes succeed with NotFound listing the missing IDs instead of
// hiding failures.
func (s *Store) DeleteSchedules(owner platform.Message, ids []string) (DeleteSchedulesResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var want []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, id)
	}
	if len(want) == 0 {
		return DeleteSchedulesResult{}, fmt.Errorf("id, ids, or text is required")
	}
	next := s.cloneSchedules()
	var deleted []Schedule
	remaining := make([]Schedule, 0, len(next.Schedules))
	wantSet := make(map[string]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	for _, v := range next.Schedules {
		if wantSet[v.ID] && sameOwner(v.Route, owner) {
			deleted = append(deleted, v)
			continue
		}
		remaining = append(remaining, v)
	}
	if len(deleted) == 0 {
		// Wrong-topic hint: name the owning topic instead of letting the
		// model claim success.
		for _, id := range want {
			if route, ok := s.ownerTopicForSchedule(id, owner); ok {
				if sameChatUser(route, owner) {
					return DeleteSchedulesResult{}, fmt.Errorf("schedule %s lives in topic %d, not in this topic %d for this user; go there or delete there", id, route.TopicID, owner.TopicID)
				}
				return DeleteSchedulesResult{}, fmt.Errorf("schedule not found in this conversation for this user")
			}
		}
		// No ID matched at all: hint at cross-topic existence when relevant.
		other := 0
		for _, v := range next.Schedules {
			if sameChatUser(v.Route, owner) && isLiveSchedule(v) {
				other++
			}
		}
		if other > 0 {
			return DeleteSchedulesResult{}, fmt.Errorf("schedule not found in this conversation for this user; %d schedule(s) exist in other topic(s) of this chat for you", other)
		}
		return DeleteSchedulesResult{}, fmt.Errorf("schedule not found in this conversation for this user")
	}
	// Partial-delete proof: report requested IDs that were not live here.
	var notFound []string
	deletedSet := make(map[string]bool, len(deleted))
	for _, v := range deleted {
		deletedSet[v.ID] = true
	}
	for _, id := range want {
		if !deletedSet[id] {
			notFound = append(notFound, id)
		}
	}
	next.Schedules = remaining
	pendingCancelled := 0
	delSet := make(map[string]bool, len(deleted))
	for _, v := range deleted {
		delSet[v.ID] = true
	}
	for i := range next.Runs {
		if delSet[next.Runs[i].ScheduleID] && next.Runs[i].Status == "pending" {
			next.Runs[i].Status = "cancelled"
			pendingCancelled++
		}
	}
	now := time.Now()
	for _, v := range deleted {
		// Tombstone route keeps only the ownership tuple (platform/chat/
		// topic/user); input-only fields (text, update/message IDs, images)
		// are never persisted, matching CreateSchedule's stripping.
		next.Deleted = append(next.Deleted, DeletedSchedule{
			Route:       platform.Message{Platform: v.Route.Platform, ChatID: v.Route.ChatID, TopicID: v.Route.TopicID, UserID: v.Route.UserID},
			TextHash:    scheduleTextHash(v.Text),
			TextPreview: schedulePreviewText(v.Text),
			DeletedAt:   now,
			Repeat:      v.Repeat,
			Kind:        v.Kind,
		})
	}
	// Anti-resurrection: scrub matching long-term notes + pending sources
	// in this chat so past discussion cannot rebuild the schedule. Only
	// recurring agent tasks scrub, using the conservative exact/substring
	// matcher (see scheduleMemoryMatches).
	var texts []string
	for _, v := range deleted {
		if v.Kind == "agent" && v.Repeat != "once" {
			texts = append(texts, v.Text)
		}
	}
	// Save schedules first; memory second. Stop stays reliable when memory
	// scrub fails: schedules are already deleted, so return success and keep
	// the scrub best-effort.
	if err := s.saveSchedules(next); err != nil {
		return DeleteSchedulesResult{}, err
	}
	memKey := owner.WorkspaceKey()
	mem, err := s.loadMemory(memKey)
	if err != nil {
		return DeleteSchedulesResult{Deleted: deleted, PendingCancelled: pendingCancelled, NotFound: notFound}, nil
	}
	eRemoved, pRemoved, previews := filterScheduleMemory(&mem, texts)
	if eRemoved > 0 || pRemoved > 0 {
		mem.Revision++
		if err := s.writeMemory(memKey, mem); err != nil {
			log.Printf("[SCHEDULE] memory scrub after delete failed: %v", err)
			return DeleteSchedulesResult{Deleted: deleted, PendingCancelled: pendingCancelled, NotFound: notFound}, nil
		}
	}
	return DeleteSchedulesResult{Deleted: deleted, PendingCancelled: pendingCancelled, MemoryNotesRemoved: eRemoved, MemoryPendingRemoved: pRemoved, MemoryRemovedPreviews: previews, NotFound: notFound}, nil
}

// filterScheduleMemory removes distilled notes and pending journal sources
// whose text conservatively matches a deleted recurring agent task (exact
// normalized equality or normalized substring). It returns removed counts
// plus up to 5 entry previews for logging, and updates loss counters. The
// caller owns Revision bump + persistence.
func filterScheduleMemory(mem *ChatMemory, texts []string) (int, int, []string) {
	if len(texts) == 0 {
		return 0, 0, nil
	}
	var previews []string
	keptEntries := mem.Entries[:0]
	removedEntries := 0
	var removedBytes uint64
	for _, e := range mem.Entries {
		hit := false
		for _, t := range texts {
			if scheduleMemoryMatches(e.Text, t) {
				hit = true
				break
			}
			for _, k := range e.Keys {
				if scheduleMemoryMatches(k, t) {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if hit {
			removedEntries++
			removedBytes += uint64(len(e.Text))
			if len(previews) < 5 {
				previews = append(previews, sanitizeScheduleLine(UTF8Prefix(e.Text, 80)))
			}
			continue
		}
		keptEntries = append(keptEntries, e)
	}
	if removedEntries > 0 {
		for i := len(keptEntries); i < len(mem.Entries); i++ {
			mem.Entries[i] = MemoryEntry{}
		}
		mem.Entries = keptEntries
		mem.Loss.DroppedNotes += uint64(removedEntries)
		mem.Loss.DroppedNoteBytes += removedBytes
	}
	keptPending := mem.Pending[:0]
	removedPending := 0
	var pendingBytes uint64
	for _, p := range mem.Pending {
		hit := false
		for _, t := range texts {
			if scheduleMemoryMatches(p.Transcript, t) {
				hit = true
				break
			}
		}
		if hit {
			removedPending++
			pendingBytes += uint64(len(p.Transcript))
			continue
		}
		keptPending = append(keptPending, p)
	}
	if removedPending > 0 {
		for i := len(keptPending); i < len(mem.Pending); i++ {
			mem.Pending[i] = SourceEvent{}
		}
		mem.Pending = keptPending
		mem.Loss.LostEvents += uint64(removedPending)
		mem.Loss.LostBytes += pendingBytes
	}
	return removedEntries, removedPending, previews
}

// DeleteSchedulesMatching resolves a free-text stop request to every live ID
// whose text matches (exact + fuzzy) and deletes them atomically. One "stop
// X" removes near-duplicates without N round trips.
func (s *Store) DeleteSchedulesMatching(owner platform.Message, query string) (DeleteSchedulesResult, error) {
	// Reuse the shared matchers to avoid duplicated scope logic.
	matched := s.FindMatchingSchedules(owner, query)
	if len(matched) == 0 {
		if other, _ := s.CrossTopicScheduleInfo(owner); other > 0 {
			return DeleteSchedulesResult{}, fmt.Errorf("no matching schedules in this topic for this user; %d schedule(s) exist in other topic(s) of this chat for you", other)
		}
		return DeleteSchedulesResult{}, fmt.Errorf("no matching schedules in this topic for this user")
	}
	ids := make([]string, 0, len(matched))
	for _, v := range matched {
		ids = append(ids, v.ID)
	}
	return s.DeleteSchedules(owner, ids)
}
func (s *Store) DeleteSchedule(owner platform.Message, id string) error {
	res, err := s.DeleteSchedules(owner, []string{id})
	if err != nil {
		return err
	}
	if len(res.Deleted) == 0 {
		return fmt.Errorf("schedule not found in this conversation for this user")
	}
	return nil
}

// ScanDue atomically advances schedules AND creates their occurrences. It
// coalesces missed recurrences to the most recent one within the grace window.
// Each schedule may have at most one pending/running occurrence.
func (s *Store) ScanDue(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	changed := false
	active := map[string]bool{}
	queued := 0
	for _, r := range next.Runs {
		if runActive(r) {
			active[r.ScheduleID] = true
		}
		if runActive(r) || r.Next < r.Total() {
			queued++
		}
	}
	for i := range next.Schedules {
		v := &next.Schedules[i]
		if v.NextRunAt.IsZero() || v.NextRunAt.After(now) {
			continue
		}
		if queued >= maxQueuedRuns {
			continue
		} // Backpressure: don't advance work not admitted.
		due := v.NextRunAt
		if v.Repeat != "once" {
			// Look back at most one recurrence, independent of downtime duration.
			days := 1
			if v.Repeat == "weekly" {
				days = 7
			}
			recent := nextOccurrence(*v, now.AddDate(0, 0, -days-1))
			for !recent.IsZero() && !recent.After(now) {
				if recent.After(due) {
					due = recent
				}
				recent = nextOccurrence(*v, recent)
			}
		}
		v.NextRunAt = nextOccurrence(*v, now)
		changed = true
		id := "schedule:" + v.ID + ":" + due.UTC().Format(time.RFC3339Nano)
		exists := false
		for _, r := range next.Runs {
			if r.ID == id {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		status := "pending"
		if now.Sub(due) > ScheduleGrace || active[v.ID] {
			status = "missed"
			v.Missed++
		}
		next.Runs = append(next.Runs, ScheduledRun{ID: id, ScheduleID: v.ID, Route: v.Route, Kind: v.Kind, Text: v.Text, ScheduledAt: due, Status: status})
		if status == "pending" {
			active[v.ID] = true
			queued++
		}
	}
	if !changed {
		return nil
	}
	return s.saveSchedules(next)
}

func (s *Store) PendingRuns() []ScheduledRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScheduledRun
	for _, r := range s.schedules.Runs {
		if r.Status == "pending" {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ScheduledAt.Before(out[j].ScheduledAt) })
	return out
}

// ForceRetryReady clears a pending retry's gate so tests can simulate time
// passing without sleeping through backoff delays.
func (s *Store) ForceRetryReady(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	for i := range next.Runs {
		if next.Runs[i].ID == id && next.Runs[i].Status == "pending" {
			next.Runs[i].RetryAt = time.Time{}
			return s.saveSchedules(next)
		}
	}
	return fmt.Errorf("run not found")
}
func (s *Store) ClaimRun(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	now := time.Now()
	for i := range next.Runs {
		if next.Runs[i].ID == id && next.Runs[i].Status == "pending" {
			if !next.Runs[i].RetryAt.IsZero() && now.Before(next.Runs[i].RetryAt) {
				return false, nil
			}
			// Stop is deterministic: a run whose parent schedule was deleted
			// must not be claimed. Cancel it so retries and restarts end.
			if !scheduleExistsLocked(next.Schedules, next.Runs[i].ScheduleID) {
				next.Runs[i].Status = "cancelled"
				next.Runs[i].RetryAt = time.Time{}
				if err := s.saveSchedules(next); err != nil {
					return false, err
				}
				return false, nil
			}
			next.Runs[i].Status = "running"
			next.Runs[i].RetryAt = time.Time{}
			if err := s.saveSchedules(next); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// FinishRun is also the scheduled outbox commit. Delivery progress lives in
// this same record, so a delivered reply cannot cause a run to be replayed.
// Failed/interrupted agent runs are retried up to MaxScheduleRetries times
// with increasing delays; intermediate failures stay pending with no
// deliverable output and only the final failure is committed.
func (s *Store) FinishRun(id, status string, parts []string) error {
	return s.FinishRunWithPhotos(id, status, parts, nil)
}

func (s *Store) FinishRunWithPhotos(id, status string, parts []string, photos []PhotoAttachment) error {
	if status != "succeeded" && status != "failed" && status != "interrupted" && status != "cancelled" {
		return fmt.Errorf("invalid terminal status")
	}
	if len(photos) > 10 {
		return fmt.Errorf("at most 10 photos per reply")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	for i := range next.Runs {
		if next.Runs[i].ID == id {
			if !runActive(next.Runs[i]) {
				return nil
			}
			// Retry failed/interrupted agent work with backoff. Cancelled,
			// succeeded, and reminder runs commit immediately.
			if (status == "failed" || status == "interrupted") && next.Runs[i].Kind == "agent" && next.Runs[i].Attempts < MaxScheduleRetries {
				// Deleted schedules never retry: mark cancelled instead of
				// re-queueing with backoff.
				if !scheduleExistsLocked(next.Schedules, next.Runs[i].ScheduleID) {
					next.Runs[i].Status = "cancelled"
					next.Runs[i].RetryAt = time.Time{}
					next.Runs[i].Parts = nil
					next.Runs[i].Photos = nil
					next.Runs[i].Next = 0
					return s.saveSchedules(next)
				}
				next.Runs[i].Attempts++
				next.Runs[i].Status = "pending"
				next.Runs[i].RetryAt = time.Now().Add(scheduleRetryDelay(next.Runs[i].Attempts))
				next.Runs[i].Parts = nil
				next.Runs[i].Photos = nil
				next.Runs[i].Next = 0
				return s.saveSchedules(next)
			}
			next.Runs[i].Status = status
			next.Runs[i].RetryAt = time.Time{}
			next.Runs[i].Parts = append([]string(nil), parts...)
			for j := range next.Runs[i].Parts {
				next.Runs[i].Parts[j] = s.policy.Text(redaction.Remote, next.Runs[i].Parts[j])
			}
			next.Runs[i].Photos = append([]PhotoAttachment(nil), photos...)
			for j := range next.Runs[i].Photos {
				next.Runs[i].Photos[j].Caption = s.policy.Text(redaction.Remote, next.Runs[i].Photos[j].Caption)
				next.Runs[i].Photos[j].Fallback = s.policy.Text(redaction.Remote, next.Runs[i].Photos[j].Fallback)
			}
			return s.saveSchedules(next)
		}
	}
	return fmt.Errorf("run not found")
}

// RecoverRuns is called once before workers and delivery start. Reminder
// output can safely be reconstructed; agent work interrupted by a restart is
// retried with the same backoff as other failures until MaxScheduleRetries.
// Runs whose parent schedule was deleted are cancelled instead of retried.
func (s *Store) RecoverRuns() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	changed := false
	for i := range next.Runs {
		r := &next.Runs[i]
		if r.Status == "running" {
			changed = true
			if !scheduleExistsLocked(next.Schedules, r.ScheduleID) {
				r.Status = "cancelled"
				r.RetryAt = time.Time{}
				r.Parts = nil
				r.Photos = nil
				r.Next = 0
				continue
			}
			if r.Kind == "reminder" {
				r.Status = "pending"
			} else if r.Attempts < MaxScheduleRetries {
				r.Attempts++
				r.Status = "pending"
				r.RetryAt = time.Now().Add(scheduleRetryDelay(r.Attempts))
				r.Parts = nil
				r.Photos = nil
				r.Next = 0
			} else {
				r.Status = "interrupted"
				r.RetryAt = time.Time{}
				r.Parts = []string{"Scheduled task interrupted by restart; retries exhausted: " + r.ScheduleID}
			}
		}
	}
	if !changed {
		return nil
	}
	return s.saveSchedules(next)
}

func (s *Store) pendingScheduleReply() (Reply, bool) {
	for _, r := range s.schedules.Runs {
		if r.Next < r.Total() {
			return Reply{ID: r.ID, Route: r.Route, Parts: append([]string(nil), r.Parts...), Photos: append([]PhotoAttachment(nil), r.Photos...), Next: r.Next}, true
		}
	}
	return Reply{}, false
}
func (s *Store) deliveredScheduleReply(id string) error {
	next := s.cloneSchedules()
	for i := range next.Runs {
		if next.Runs[i].ID == id && next.Runs[i].Next < next.Runs[i].Total() {
			next.Runs[i].Next++
			return s.saveSchedules(next)
		}
	}
	return fmt.Errorf("scheduled outbox changed")
}
func (s *Store) deferScheduleReply(id string) error {
	next := s.cloneSchedules()
	for i, r := range next.Runs {
		if r.ID == id {
			next.Runs = append(append(next.Runs[:i:i], next.Runs[i+1:]...), r)
			return s.saveSchedules(next)
		}
	}
	return fmt.Errorf("scheduled outbox changed")
}

func (s *Store) validateSchedules() error {
	if s.schedules.Version != 0 && s.schedules.Version != ScheduleLedgerVersion {
		return fmt.Errorf("unsupported schedules version %d", s.schedules.Version)
	}
	ids := map[string]bool{}
	for _, v := range s.schedules.Schedules {
		if err := validateSchedule(v); err != nil {
			return err
		}
		if v.ID == "" || ids[v.ID] {
			return fmt.Errorf("invalid schedule ID")
		}
		ids[v.ID] = true
	}
	for _, d := range s.schedules.Deleted {
		if strings.TrimSpace(d.TextHash) == "" || d.DeletedAt.IsZero() {
			return fmt.Errorf("invalid deleted schedule tombstone")
		}
		if d.Route.Platform != "telegram" || d.Route.ChatID == 0 || d.Route.UserID == 0 {
			return fmt.Errorf("invalid deleted schedule owner")
		}
	}
	ids = map[string]bool{}
	for _, r := range s.schedules.Runs {
		if !strings.HasPrefix(r.ID, "schedule:") || ids[r.ID] || r.Next < 0 || r.Next > r.Total() {
			return fmt.Errorf("invalid scheduled run")
		}
		if r.ScheduleID == "" || r.ScheduledAt.IsZero() || r.ID != "schedule:"+r.ScheduleID+":"+r.ScheduledAt.UTC().Format(time.RFC3339Nano) || (r.Kind != "agent" && r.Kind != "reminder") || r.Route.Platform != "telegram" || r.Route.ChatID == 0 || r.Route.UserID == 0 || strings.TrimSpace(r.Text) == "" || len(r.Text) > 8192 {
			return fmt.Errorf("invalid scheduled run payload")
		}
		if len(r.Photos) > 10 {
			return fmt.Errorf("invalid scheduled run")
		}
		for _, ph := range r.Photos {
			if (ph.LocalPath == "") == (ph.RemoteURL == "") {
				return fmt.Errorf("invalid scheduled run")
			}
		}
		ids[r.ID] = true
		if r.Attempts < 0 || r.Attempts > MaxScheduleRetries {
			return fmt.Errorf("invalid run attempts")
		}
		if !r.RetryAt.IsZero() && r.Status != "pending" {
			return fmt.Errorf("invalid run retry state")
		}
		switch r.Status {
		case "pending", "running", "succeeded", "failed", "interrupted", "missed", "cancelled":
		default:
			return fmt.Errorf("invalid run status")
		}
	}
	return nil
}

// migrateSchedules upgrades a legacy schedules.json (no version field) in
// place for issue #64. It preserves IDs, At/Timezone anchors, NextRunAt,
// Attempts/RetryAt, and cancelled runs; it never re-queues done one-shots or
// recomputes recurrences. The ledger is derived, so near-duplicates stay
// separate IDs. Re-running after a crash is a no-op via the version marker.
// New fields stay additive/omitempty so downgraded binaries ignore them.
func (s *Store) migrateSchedules() error {
	if s.schedules.Version != 0 {
		return nil
	}
	path := filepath.Join(s.dir, "schedules.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.schedules.Version = ScheduleLedgerVersion
			return nil
		}
		return err
	}
	if len(data) == 0 {
		s.schedules.Version = ScheduleLedgerVersion
		return nil
	}
	// Backup before first migration. Keep Atomic write semantics below.
	stamp := time.Now().UTC().Format("20060102-150405")
	bak := filepath.Join(s.dir, "schedules.json.bak."+stamp)
	if err := os.WriteFile(bak, data, 0600); err != nil {
		return err
	}
	if err := SyncDir(s.dir); err != nil {
		return err
	}
	pruneScheduleBackups(s.dir)
	once, repeat := 0, 0
	for _, v := range s.schedules.Schedules {
		if v.Repeat == "once" {
			once++
		} else {
			repeat++
		}
	}
	runs := len(s.schedules.Runs)
	next := s.cloneSchedules()
	next.Version = ScheduleLedgerVersion
	// Prune only fully-delivered terminal runs per the existing
	// retainedRuns rule; saveSchedules enforces the bound. Everything else
	// (pending/running/cancelled, Attempts/RetryAt, Missed) is preserved.
	if err := s.saveSchedules(next); err != nil {
		return err
	}
	log.Printf("[SCHEDULE] migrated %d once + %d repeat, %d runs carried (backup %s)", once, repeat, runs, bak)
	return nil
}

// pruneScheduleBackups keeps the newest 5 migration backups so one upgrade
// does not accumulate .bak files forever. Best-effort: failures are ignored.
func pruneScheduleBackups(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var baks []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "schedules.json.bak.") {
			baks = append(baks, filepath.Join(dir, e.Name()))
		}
	}
	if len(baks) <= 5 {
		return
	}
	sort.Strings(baks)
	for _, old := range baks[:len(baks)-5] {
		_ = os.Remove(old)
	}
}
