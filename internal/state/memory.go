package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

// ChatRecord tracks an authorized chat/group seen by the runtime so the
// nightly digest sweep can enumerate workspaces without reversing the
// one-way workspace hash. Topics accumulate per workspace; memory stays
// workspace-scoped and shared by all of the chat's topics.
type ChatRecord struct {
	Platform string    `json:"platform"`
	ChatID   int64     `json:"chat_id"`
	Owner    int64     `json:"owner_user_id"`
	Topics   []int64   `json:"topics,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

// MemoryEntry is one dated digest note for a chat/group.
type MemoryEntry struct {
	Date string `json:"date"`
	Text string `json:"text"`
}

const MemoryVersion = 1
const DefaultMaxPendingBytes = 4 << 20
const MaxSourceBytes = 256 << 10

// MaxMemoryNotices bounds the durable per-chat delivery outbox. Digests can
// succeed while Telegram delivery stalls; keep newest notices so disk stays
// bounded and the latest summary still arrives.
const MaxMemoryNotices = 16

type MemoryLimits struct {
	MaxBytes        int
	MaxPendingBytes int
}

// SourceEvent is independent of the bounded prompt history. Transcript contains
// only ordered user, steering, and final assistant text, never tools/reasoning.
type SourceEvent struct {
	Sequence   uint64    `json:"sequence"`
	ID         string    `json:"id"`
	Topic      int64     `json:"topic"`
	At         time.Time `json:"at"`
	Transcript string    `json:"transcript"`
}

type ExchangeIntent struct {
	Key          string       `json:"key"`
	Conversation Conversation `json:"conversation"`
}

type MemoryLoss struct {
	LostEvents       uint64 `json:"lost_events"`
	LostBytes        uint64 `json:"lost_bytes"`
	TruncatedEvents  uint64 `json:"truncated_events"`
	TruncatedBytes   uint64 `json:"truncated_bytes"`
	DroppedNotes     uint64 `json:"dropped_notes"`
	DroppedNoteBytes uint64 `json:"dropped_note_bytes"`
}

type MemoryRetry struct {
	Work     bool      `json:"work,omitempty"`
	Deferred bool      `json:"deferred,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	NextAt   time.Time `json:"next_at,omitempty"`
}

// ChatMemory is also the transaction boundary for digest acknowledgement and
// delivery. Revision changes only when summaries/source eligibility change;
// concurrent appends and outbox delivery do not invalidate a digest snapshot.
type ChatMemory struct {
	Version           int              `json:"version"`
	Epoch             uint64           `json:"epoch"`
	Revision          uint64           `json:"revision"`
	NextSequence      uint64           `json:"next_sequence"`
	Cursor            uint64           `json:"cursor"`
	Entries           []MemoryEntry    `json:"entries,omitempty"`
	Pending           []SourceEvent    `json:"pending,omitempty"`
	Intent            *ExchangeIntent  `json:"exchange_intent,omitempty"`
	LastSourceID      string           `json:"last_source_id,omitempty"`
	LastDigestAt      time.Time        `json:"last_digest_at,omitempty"`
	Loss              MemoryLoss       `json:"loss"`
	Overflow          bool             `json:"overflow,omitempty"`
	Retry             MemoryRetry      `json:"retry"`
	Notices           []Reply          `json:"notices,omitempty"`
	LegacyBaseline    bool             `json:"legacy_baseline,omitempty"`
	MigrationReported bool             `json:"migration_reported,omitempty"`
	Route             platform.Message `json:"route"`
}

// PendingBytes measures encoded journal bytes, including labels and JSON escapes.
func (m ChatMemory) PendingBytes() int {
	if len(m.Pending) == 0 {
		return 0
	}
	data, _ := json.Marshal(m.Pending)
	return len(data)
}

// UTF8Prefix limits bytes without cutting a code point.
func UTF8Prefix(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func (m ChatMemory) bytes() int {
	n := 0
	for _, e := range m.Entries {
		n += len(e.Text)
	}
	return n
}

// Bytes reports the stored memory size callers use for cap management.
func (m ChatMemory) Bytes() int { return m.bytes() }

func validRecord(r ChatRecord) bool {
	return r.Platform != "" && r.ChatID != 0
}

func (s *Store) chatsPath() string { return filepath.Join(s.dir, "chats.json") }
func (s *Store) memoryPath(key string) string {
	return filepath.Join(s.dir, "memory", key+".json")
}

func readChats(path string) (map[string]ChatRecord, error) {
	var records map[string]ChatRecord
	if err := read(path, &records); err != nil {
		return nil, err
	}
	if records == nil {
		return map[string]ChatRecord{}, nil
	}
	for key, r := range records {
		if !validRecord(r) {
			delete(records, key)
		}
	}
	return records, nil
}

// RecordChat upserts the workspace registry entry for an authorized route.
// It never fails the chat path: persistence errors are returned for logging.
func (s *Store) RecordChat(route platform.Message, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := readChats(s.chatsPath())
	if err != nil {
		return err
	}
	key := route.WorkspaceKey()
	rec := records[key]
	rec.Platform = route.Platform
	rec.ChatID = route.ChatID
	if route.UserID != 0 {
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
	rec.LastSeen = now.UTC()
	records[key] = rec
	return Atomic(s.chatsPath(), records)
}

// ListChats returns registered workspaces ordered by key for a stable sweep.
func (s *Store) ListChats() ([]ChatRecordWithKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := readChats(s.chatsPath())
	if err != nil {
		return nil, err
	}
	for key, mem := range s.memories {
		if _, ok := records[key]; !ok && mem.Route.ChatID != 0 {
			records[key] = ChatRecord{Platform: mem.Route.Platform, ChatID: mem.Route.ChatID, Owner: mem.Route.UserID}
		}
	}
	out := make([]ChatRecordWithKey, 0, len(records))
	for key, r := range records {
		out = append(out, ChatRecordWithKey{Key: key, Record: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ChatRecordWithKey pairs a registry key with its record.
type ChatRecordWithKey struct {
	Key    string
	Record ChatRecord
}

// loadMemory migrates legacy summaries exactly once. Retained conversation
// history is deliberately outside the new source baseline.
func (s *Store) loadMemory(key string) (ChatMemory, error) {
	var mem ChatMemory
	data, err := os.ReadFile(s.memoryPath(key))
	if err != nil && !os.IsNotExist(err) {
		return mem, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &mem); err != nil {
			return mem, err
		}
	}
	if mem.Version > MemoryVersion {
		return mem, fmt.Errorf("unsupported memory version %d", mem.Version)
	}
	if mem.Version == 0 {
		mem.Version, mem.Epoch = MemoryVersion, 1
		mem.LegacyBaseline = len(data) > 0
		mem.Pending, mem.Intent = nil, nil
		mem.Cursor, mem.NextSequence = 0, 0
		s.boundMemory(&mem, s.memoryLimits.MaxBytes)
		if len(data) > 0 {
			if err := s.writeMemory(key, mem); err != nil {
				return mem, err
			}
		}
	}
	s.memories[key] = mem
	return mem, nil
}

func (s *Store) writeMemory(key string, mem ChatMemory) error {
	if err := s.atomic(s.memoryPath(key), mem); err != nil {
		// Rename may have succeeded before a directory-sync error. Refresh the
		// recovery cache so a later history read sees the durable intent.
		var saved ChatMemory
		if read(s.memoryPath(key), &saved) == nil && saved.Version == MemoryVersion {
			s.memories[key] = saved
		}
		return err
	}
	s.memories[key] = mem
	return nil
}

// LoadMemory returns an independent snapshot, suitable for work outside locks.
func (s *Store) LoadMemory(key string) (ChatMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadMemory(key)
}

// RedactMemoryText uses the immutable persistence policy before measurement.
func (s *Store) RedactMemoryText(text string) string {
	return s.policy.Text(redaction.Persistence, text)
}

// PrepareMemory redacts and enforces the hard persisted-text budget.
func (s *Store) PrepareMemory(mem ChatMemory, maxBytes int) ChatMemory {
	mem.Entries = append([]MemoryEntry(nil), mem.Entries...)
	s.boundMemory(&mem, maxBytes)
	return mem
}

func (s *Store) boundMemory(mem *ChatMemory, maxBytes int) {
	for i := range mem.Entries {
		mem.Entries[i].Text = strings.ToValidUTF8(s.policy.Text(redaction.Persistence, mem.Entries[i].Text), "�")
	}
	if maxBytes <= 0 {
		maxBytes = s.memoryLimits.MaxBytes
	}
	for mem.Bytes() > maxBytes && len(mem.Entries) > 1 {
		mem.Loss.DroppedNotes++
		mem.Loss.DroppedNoteBytes += uint64(len(mem.Entries[0].Text))
		mem.Entries = mem.Entries[1:]
	}
	if mem.Bytes() > maxBytes && len(mem.Entries) == 1 {
		old := len(mem.Entries[0].Text)
		mem.Entries[0].Text = UTF8Prefix(mem.Entries[0].Text, maxBytes)
		mem.Loss.DroppedNoteBytes += uint64(old - len(mem.Entries[0].Text))
	}
	if len(mem.Entries) == 1 && mem.Entries[0].Text == "" {
		mem.Entries = nil
	}
}

// ClearMemory advances the epoch, invalidating model work already in flight.
// Recover history first: forgetting memory must never discard prompt history.
func (s *Store) ClearMemory(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mem, err := s.loadMemory(key)
	if err != nil {
		return err
	}
	if err := s.recoverExchange(key, &mem); err != nil {
		return err
	}
	mem.Epoch++
	mem.Revision++
	mem.Entries, mem.Pending, mem.Notices = nil, nil, nil
	mem.Cursor = mem.NextSequence
	mem.Retry, mem.Loss, mem.Overflow = MemoryRetry{}, MemoryLoss{}, false
	return s.writeMemory(key, mem)
}

// AppendMemory is a local administrative/test helper; digest workers use the
// conditional CommitDigest transaction instead.
func (s *Store) AppendMemory(key, date, text string, maxBytes int) (ChatMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mem, err := s.loadMemory(key)
	if err != nil {
		return mem, err
	}
	mem.Entries = append(mem.Entries, MemoryEntry{Date: date, Text: text})
	s.boundMemory(&mem, maxBytes)
	mem.Revision++
	err = s.writeMemory(key, mem)
	return mem, err
}

// WorkspaceRoute rebuilds a sendable route for a registered workspace. Topic
// callers override TopicID (digests notify at topic 0).
func (r ChatRecordWithKey) WorkspaceRoute() platform.Message {
	return platform.Message{
		Platform: r.Record.Platform,
		ChatID:   r.Record.ChatID,
		UserID:   r.Record.Owner,
	}
}

// ReportMemoryMigration queues the baseline notice once, including for legacy
// chats with no journal work. Delivery retries use the normal durable outbox.
func (s *Store) ReportMemoryMigration(key string, route platform.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mem, err := s.loadMemory(key)
	if err != nil {
		return err
	}
	if !mem.LegacyBaseline || mem.MigrationReported {
		return nil
	}
	s.addMemoryNotice(&mem, route, "memory:"+key+":migration", []string{"Memory storage upgraded. Existing summaries were retained within budget. Pre-upgrade pending history is outside the new baseline and was not imported."})
	mem.MigrationReported = true
	return s.writeMemory(key, mem)
}
