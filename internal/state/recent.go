package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

// RecentMessage is one tapped human message kept for the companion sweep.
// It covers group chatter even without a mention, so the decider sees real
// conversation instead of only addressed history. Timestamps are arrival time
// (Telegram date is not decoded); the sweep treats them as recency hints.
type RecentMessage struct {
	UpdateID int64     `json:"update_id"`
	Topic    int64     `json:"topic,omitempty"`
	User     int64     `json:"user_id"`
	At       time.Time `json:"at"`
	Text     string    `json:"text"`
}

const maxRecentTextRunes = 2000

func defaultRecentMaxMessages() int { return 200 }
func defaultRecentMaxBytes() int    { return 32768 }

func (s *Store) recentPath() string { return filepath.Join(s.dir, "recent.json") }

func (s *Store) recoverRecent() error {
	var raw map[string][]RecentMessage
	if err := read(s.recentPath(), &raw); err != nil {
		return err
	}
	if raw == nil {
		raw = map[string][]RecentMessage{}
	}
	// Validate and drop corrupt entries; quarantine syntax failures so one
	// bad file cannot block startup for all chats.
	for key, msgs := range raw {
		kept := msgs[:0]
		for _, m := range msgs {
			if m.At.IsZero() || strings.TrimSpace(m.Text) == "" {
				continue
			}
			kept = append(kept, m)
		}
		// Keep arrival order; sweep reads oldest-first.
		sort.Slice(kept, func(i, j int) bool {
			if kept[i].At.Equal(kept[j].At) {
				return kept[i].UpdateID < kept[j].UpdateID
			}
			return kept[i].At.Before(kept[j].At)
		})
		if len(kept) == 0 {
			delete(raw, key)
			continue
		}
		raw[key] = kept
	}
	s.recent = raw
	return nil
}

func quarantineRecent(dir string) error {
	src := filepath.Join(dir, "recent.json")
	dst := filepath.Join(dir, "recent.corrupt-"+time.Now().UTC().Format("20060102T150405Z999999999")+".json")
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return SyncDir(dir)
}

// AppendRecent stores one tapped message, creating the chat record if needed
// without touching LastSeen (taps must not trigger skip_recent_activity).
// It enforces per-chat count/bytes caps (oldest evicted first) and dedups by
// UpdateID so polling retries stay idempotent. Text is persistence-redacted
// and truncated to maxRecentTextRunes.
func (s *Store) AppendRecent(route platform.Message, at time.Time, text string, maxMessages, maxBytes int) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if maxMessages <= 0 {
		maxMessages = defaultRecentMaxMessages()
	}
	if maxBytes <= 0 {
		maxBytes = defaultRecentMaxBytes()
	}
	if len([]rune(trimmed)) > maxRecentTextRunes {
		trimmed = string([]rune(trimmed)[:maxRecentTextRunes])
	}
	trimmed = strings.ToValidUTF8(s.policy.Text(redaction.Persistence, trimmed), "�")
	if strings.TrimSpace(trimmed) == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	key := route.WorkspaceKey()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure the chat is visible to ListChats without marking activity.
	records, err := readChats(s.chatsPath())
	if err != nil {
		return err
	}
	rec, ok := records[key]
	if !ok {
		rec = ChatRecord{Platform: route.Platform, ChatID: route.ChatID, Owner: route.UserID, LastSeen: time.Time{}}
	}
	if rec.Platform == "" {
		rec.Platform = route.Platform
	}
	if rec.ChatID == 0 {
		rec.ChatID = route.ChatID
	}
	if route.UserID != 0 && rec.Owner == 0 {
		rec.Owner = route.UserID
	}
	found := false
	for _, t := range rec.Topics {
		if t == route.TopicID {
			found = true
			break
		}
	}
	if !found {
		rec.Topics = append(rec.Topics, route.TopicID)
		sort.Slice(rec.Topics, func(i, j int) bool { return rec.Topics[i] < rec.Topics[j] })
	}
	if !validRecord(rec) {
		return fmt.Errorf("invalid tap route")
	}
	if !ok || len(rec.Topics) != len(records[key].Topics) || records[key].Owner != rec.Owner {
		records[key] = rec
		if err := Atomic(s.chatsPath(), records); err != nil {
			return err
		}
	}

	msgs := append([]RecentMessage(nil), s.recent[key]...)
	for _, m := range msgs {
		if m.UpdateID == route.UpdateID && route.UpdateID != 0 {
			return nil
		}
	}
	msgs = append(msgs, RecentMessage{
		UpdateID: route.UpdateID,
		Topic:    route.TopicID,
		User:     route.UserID,
		At:       at,
		Text:     trimmed,
	})
	// Enforce caps: oldest first.
	for len(msgs) > maxMessages || recentBytes(msgs) > maxBytes {
		msgs = msgs[1:]
		if len(msgs) == 0 {
			break
		}
	}
	next := make(map[string][]RecentMessage, len(s.recent)+1)
	for k, v := range s.recent {
		next[k] = v
	}
	if len(msgs) == 0 {
		delete(next, key)
	} else {
		next[key] = msgs
	}
	if err := s.atomic(s.recentPath(), next); err != nil {
		// Recovery cache: reread durable file so later reads see intent
		// if rename succeeded before a directory-sync error.
		var saved map[string][]RecentMessage
		if read(s.recentPath(), &saved) == nil && saved != nil {
			s.recent = saved
		}
		return err
	}
	s.recent = next
	return nil
}

func recentBytes(msgs []RecentMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text)
	}
	return n
}

// RecentSince returns a copy of taps with At >= since, oldest-first.
func (s *Store) RecentSince(key string, since time.Time) ([]RecentMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.recent[key]
	if len(msgs) == 0 {
		return nil, nil
	}
	var out []RecentMessage
	for _, m := range msgs {
		if !m.At.Before(since) {
			out = append(out, m)
		}
	}
	return out, nil
}

// ConsumeRecent deletes taps with At <= before. A zero before clears the key.
// It bounds disk even when sweeps skip; processed sweeps pass their start
// watermark, expiry pruning passes now-window.
func (s *Store) ConsumeRecent(key string, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs, ok := s.recent[key]
	if !ok || len(msgs) == 0 {
		return nil
	}
	if before.IsZero() {
		next := make(map[string][]RecentMessage, len(s.recent))
		for k, v := range s.recent {
			if k != key {
				next[k] = v
			}
		}
		if err := s.atomic(s.recentPath(), next); err != nil {
			return err
		}
		s.recent = next
		return nil
	}
	// Copy to avoid mutating the stored slice while filtering.
	kept := append([]RecentMessage(nil), msgs[:0]...)
	for _, m := range msgs {
		if m.At.After(before) {
			kept = append(kept, m)
		}
	}
	next := make(map[string][]RecentMessage, len(s.recent))
	for k, v := range s.recent {
		next[k] = v
	}
	if len(kept) == 0 {
		delete(next, key)
	} else {
		next[key] = kept
	}
	// No-op when nothing expired.
	if len(kept) == len(msgs) {
		return nil
	}
	if err := s.atomic(s.recentPath(), next); err != nil {
		return err
	}
	s.recent = next
	return nil
}

// ClearRecent removes all taps for a chat.
func (s *Store) ClearRecent(key string) error { return s.ConsumeRecent(key, time.Time{}) }
