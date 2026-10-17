package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	ID          string           `json:"id"`
	ScheduleID  string           `json:"schedule_id"`
	Route       platform.Message `json:"route"`
	Kind        string           `json:"kind"`
	Text        string           `json:"text"`
	ScheduledAt time.Time        `json:"scheduled_at"`
	Status      string           `json:"status"`
	Parts       []string         `json:"parts,omitempty"`
	Next        int              `json:"next"`
}

type scheduleState struct {
	Schedules []Schedule     `json:"schedules"`
	Runs      []ScheduledRun `json:"runs"`
}

// Copy before mutation: failed writes must not advance in-memory state.
func (s *Store) cloneSchedules() scheduleState {
	return scheduleState{Schedules: append([]Schedule(nil), s.schedules.Schedules...), Runs: append([]ScheduledRun(nil), s.schedules.Runs...)}
}
func (s *Store) saveSchedules(next scheduleState) error {
	// Keep bounded terminal history, but never discard work or undelivered output.
	terminals := 0
	for _, r := range next.Runs {
		if !runActive(r) && r.Next == len(r.Parts) {
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
	if err := Atomic(filepath.Join(s.dir, "schedules.json"), next); err != nil {
		return err
	}
	s.schedules = next
	return nil
}
func runActive(r ScheduledRun) bool { return r.Status == "pending" || r.Status == "running" }

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
	count := 0
	for _, old := range s.schedules.Schedules {
		if sameOwner(old.Route, v.Route) && old.Kind == v.Kind && old.Text == s.policy.Text(redaction.Persistence, v.Text) && old.At.Equal(v.At) && old.Repeat == v.Repeat && old.Timezone == v.Timezone {
			return old, nil
		}
		if old.Route.ConversationKey() == v.Route.ConversationKey() {
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
	next := s.cloneSchedules()
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
			runs = append(runs, v)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].ScheduledAt.Before(runs[j].ScheduledAt) })
	if len(runs) > 20 {
		runs = runs[len(runs)-20:]
	}
	return schedules, runs
}
func (s *Store) DeleteSchedule(owner platform.Message, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	found := false
	for i, v := range next.Schedules {
		if v.ID == id && sameOwner(v.Route, owner) {
			next.Schedules = append(next.Schedules[:i], next.Schedules[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("schedule not found in this conversation for this user")
	}
	for i := range next.Runs {
		if next.Runs[i].ScheduleID == id && next.Runs[i].Status == "pending" {
			next.Runs[i].Status = "cancelled"
		}
	}
	return s.saveSchedules(next)
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
		if runActive(r) || r.Next < len(r.Parts) {
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
func (s *Store) ClaimRun(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	for i := range next.Runs {
		if next.Runs[i].ID == id && next.Runs[i].Status == "pending" {
			next.Runs[i].Status = "running"
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
func (s *Store) FinishRun(id, status string, parts []string) error {
	if status != "succeeded" && status != "failed" && status != "interrupted" && status != "cancelled" {
		return fmt.Errorf("invalid terminal status")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	for i := range next.Runs {
		if next.Runs[i].ID == id {
			if !runActive(next.Runs[i]) {
				return nil
			}
			next.Runs[i].Status = status
			next.Runs[i].Parts = append([]string(nil), parts...)
			for j := range next.Runs[i].Parts {
				next.Runs[i].Parts[j] = s.policy.Text(redaction.Remote, next.Runs[i].Parts[j])
			}
			return s.saveSchedules(next)
		}
	}
	return fmt.Errorf("run not found")
}

// RecoverRuns is called once before workers and delivery start. Unknown agent
// side effects are never retried; reminder output can safely be reconstructed.
func (s *Store) RecoverRuns() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneSchedules()
	changed := false
	for i := range next.Runs {
		r := &next.Runs[i]
		if r.Status == "running" {
			changed = true
			if r.Kind == "reminder" {
				r.Status = "pending"
			} else {
				r.Status = "interrupted"
				r.Parts = []string{"Scheduled task interrupted by restart; not automatically retried: " + r.ScheduleID}
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
		if r.Next < len(r.Parts) {
			return Reply{ID: r.ID, Route: r.Route, Parts: append([]string(nil), r.Parts...), Next: r.Next}, true
		}
	}
	return Reply{}, false
}
func (s *Store) deliveredScheduleReply(id string) error {
	next := s.cloneSchedules()
	for i := range next.Runs {
		if next.Runs[i].ID == id && next.Runs[i].Next < len(next.Runs[i].Parts) {
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
	ids = map[string]bool{}
	for _, r := range s.schedules.Runs {
		if !strings.HasPrefix(r.ID, "schedule:") || ids[r.ID] || r.Next < 0 || r.Next > len(r.Parts) {
			return fmt.Errorf("invalid scheduled run")
		}
		if r.ScheduleID == "" || r.ScheduledAt.IsZero() || r.ID != "schedule:"+r.ScheduleID+":"+r.ScheduledAt.UTC().Format(time.RFC3339Nano) || (r.Kind != "agent" && r.Kind != "reminder") || r.Route.Platform != "telegram" || r.Route.ChatID == 0 || r.Route.UserID == 0 || strings.TrimSpace(r.Text) == "" || len(r.Text) > 8192 {
			return fmt.Errorf("invalid scheduled run payload")
		}
		ids[r.ID] = true
		switch r.Status {
		case "pending", "running", "succeeded", "failed", "interrupted", "missed", "cancelled":
		default:
			return fmt.Errorf("invalid run status")
		}
	}
	return nil
}
