package state

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

// CompanionRun records an actual sweep, not a predicted wake-up. A missing
// Finished means running or interrupted; queued and delivered are distinct.
type CompanionRun struct {
	ID          string    `json:"id"`
	Started     time.Time `json:"started"`
	Finished    time.Time `json:"finished,omitempty"`
	Result      string    `json:"result"`
	Detail      string    `json:"detail,omitempty"`
	SentAt      time.Time `json:"sent_at,omitempty"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
	Draft       string    `json:"draft,omitempty"`
	ContentHash string    `json:"content_hash,omitempty"`
	Reply       *Reply    `json:"reply,omitempty"`
}

func (s *Store) recoverCompanion() error {
	if err := read(filepath.Join(s.dir, "companion_activity.json"), &s.companionRuns); err != nil {
		return err
	}
	changed := false
	for key, runs := range s.companionRuns {
		for i := range runs {
			if reply := runs[i].Reply; reply != nil && (reply.Total() == 0 || reply.Next < 0 || reply.Next >= reply.Total() || reply.ID != runs[i].ID) {
				return fmt.Errorf("invalid companion outbox")
			}
			if runs[i].Result == "running" {
				runs[i].Result, runs[i].Detail = "interrupted", "service restarted before completion"
				changed = true
			}
		}
		s.companionRuns[key] = runs
	}
	if changed {
		return s.atomic(filepath.Join(s.dir, "companion_activity.json"), s.companionRuns)
	}
	return nil
}

func (s *Store) CompanionRuns(key string) ([]CompanionRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never expose mutable outbox entries to callers.
	runs := append([]CompanionRun(nil), s.companionRuns[key]...)
	for i := range runs {
		runs[i].Reply = nil
	}
	return runs, nil
}

// writeCompanionRuns commits delivery state, history, and rate-limit evidence
// together. A crash cannot enqueue a message without recording its cooldown.
func (s *Store) writeCompanionRuns(key string, runs []CompanionRun) error {
	next := make(map[string][]CompanionRun, len(s.companionRuns)+1)
	for k, v := range s.companionRuns {
		next[k] = v
	}
	next[key] = runs
	if err := s.atomic(filepath.Join(s.dir, "companion_activity.json"), next); err != nil {
		return err
	}
	s.companionRuns = next
	return nil
}

func (s *Store) StartCompanionRun(key string, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := make([]CompanionRun, 0, 128)
	var lastSent time.Time
	for _, run := range s.companionRuns[key] {
		if run.SentAt.After(lastSent) {
			lastSent = run.SentAt
		}
	}
	protected := func(run CompanionRun) bool {
		return run.Reply != nil || !run.SentAt.IsZero() && (run.SentAt.Equal(lastSent) || now.Sub(run.SentAt) < 24*time.Hour)
	}
	for _, run := range s.companionRuns[key] {
		if protected(run) || now.Sub(run.Started) < 7*24*time.Hour {
			runs = append(runs, run)
		}
	}
	// Bound ordinary diagnostics, never prune pending delivery, the latest
	// check-in, or evidence needed for the rolling 24h cap.
	for len(runs) >= 128 {
		removed := false
		for i, run := range runs {
			if !protected(run) {
				runs = append(runs[:i], runs[i+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	id := fmt.Sprintf("companion:%s:%d", key, now.UnixNano())
	runs = append(runs, CompanionRun{ID: id, Started: now, Result: "running"})
	return id, s.writeCompanionRuns(key, runs)
}

func (s *Store) FinishCompanionRun(key, id, result, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := append([]CompanionRun(nil), s.companionRuns[key]...)
	for i := range runs {
		if runs[i].ID == id {
			runs[i].Finished, runs[i].Result, runs[i].Detail = time.Now(), result, detail
			return s.writeCompanionRuns(key, runs)
		}
	}
	return fmt.Errorf("companion run not found")
}

func (s *Store) QueueCompanion(key, id, hash, draft string, route platform.Message, parts []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(parts) == 0 {
		return fmt.Errorf("empty companion reply")
	}
	runs := append([]CompanionRun(nil), s.companionRuns[key]...)
	for i := range runs {
		if runs[i].ID != id {
			continue
		}
		if runs[i].Reply != nil || !runs[i].SentAt.IsZero() {
			return nil
		}
		for _, run := range runs {
			if run.Reply != nil {
				return fmt.Errorf("companion reply already pending")
			}
		}
		copyParts := append([]string(nil), parts...)
		for j := range copyParts {
			copyParts[j] = s.policy.Text(redaction.Remote, copyParts[j])
		}
		route.Text, route.Images, route.PhotoIDs, route.Files, route.ReplyTo = "", nil, nil, nil, nil
		runs[i].Reply = &Reply{ID: id, Route: route, Parts: copyParts}
		runs[i].SentAt, runs[i].Finished = time.Now(), time.Now()
		runs[i].Result, runs[i].Detail = "queued", "awaiting delivery"
		runs[i].ContentHash, runs[i].Draft = hash, s.policy.Text(redaction.Remote, draft)
		return s.writeCompanionRuns(key, runs)
	}
	return fmt.Errorf("companion run not found")
}

func (s *Store) pendingCompanionReply() (Reply, bool) {
	keys := make([]string, 0, len(s.companionRuns))
	for key := range s.companionRuns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Round-robin on delivery/defer so an unreachable chat does not starve
	// every other companion message.
	split := sort.SearchStrings(keys, s.companionLastKey)
	if split < len(keys) && keys[split] == s.companionLastKey {
		split++
	}
	keys = append(append([]string(nil), keys[split:]...), keys[:split]...)
	for _, key := range keys {
		for _, run := range s.companionRuns[key] {
			if run.Reply != nil {
				reply := *run.Reply
				reply.Parts = append([]string(nil), reply.Parts...)
				return reply, true
			}
		}
	}
	return Reply{}, false
}

func (s *Store) updateCompanionReply(id string, delivered bool) error {
	for key, old := range s.companionRuns {
		for i, run := range old {
			if run.ID != id || run.Reply == nil {
				continue
			}
			runs := append([]CompanionRun(nil), old...)
			reply := *run.Reply
			if delivered {
				reply.Next++
				if reply.Next >= reply.Total() {
					runs[i].Reply = nil
					runs[i].Result, runs[i].Detail = "delivered", ""
					runs[i].DeliveredAt = time.Now()
				} else {
					runs[i].Reply = &reply
				}
			} else {
				runs[i].Detail = "delivery failed; retry pending"
			}
			if err := s.writeCompanionRuns(key, runs); err != nil {
				return err
			}
			s.companionLastKey, s.preferSchedules = key, false
			return nil
		}
	}
	return fmt.Errorf("companion outbox changed")
}
