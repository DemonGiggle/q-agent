package state

import (
	"path/filepath"
	"sort"
	"time"

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

// ChatMemory is the workspace-scoped long-term memory file. Digested
// counts per conversation key watermark which history pairs the digest
// already consumed, so only new pairs are distilled each night.
type ChatMemory struct {
	Entries      []MemoryEntry  `json:"entries,omitempty"`
	Digested     map[string]int `json:"digested,omitempty"`
	LastDigestAt time.Time      `json:"last_digest_at,omitempty"`
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

// LoadMemory reads the workspace memory file; missing files yield empty memory.
func (s *Store) LoadMemory(key string) (ChatMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mem ChatMemory
	if err := read(s.memoryPath(key), &mem); err != nil {
		return ChatMemory{}, err
	}
	if mem.Digested == nil {
		mem.Digested = map[string]int{}
	}
	s.policyBounds(&mem)
	return mem, nil
}

// policyBounds clamps absurd in-memory growth from a corrupt file before use.
func (s *Store) policyBounds(mem *ChatMemory) {
	const maxEntries = 1024
	if len(mem.Entries) > maxEntries {
		mem.Entries = mem.Entries[len(mem.Entries)-maxEntries:]
	}
}

// SaveMemory persists the workspace memory file with persistence redaction.
func (s *Store) SaveMemory(key string, mem ChatMemory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range mem.Entries {
		mem.Entries[i].Text = s.policy.Text(redaction.Persistence, mem.Entries[i].Text)
	}
	if mem.Digested == nil {
		mem.Digested = map[string]int{}
	}
	return Atomic(s.memoryPath(key), mem)
}

// ClearMemory drops all long-term entries but keeps digest watermarks, so a
// cleared chat does not re-digest its whole history the next night.
func (s *Store) ClearMemory(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mem ChatMemory
	_ = read(s.memoryPath(key), &mem)
	if mem.Digested == nil {
		mem.Digested = map[string]int{}
	}
	mem.Entries = nil
	return Atomic(s.memoryPath(key), mem)
}

// AppendMemory adds one dated entry and drops oldest entries while over
// maxBytes (maxBytes <= 0 leaves the entry uncapped). It returns the saved
// memory for the caller to report usage.
func (s *Store) AppendMemory(key, date, text string, maxBytes int) (ChatMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mem ChatMemory
	if err := read(s.memoryPath(key), &mem); err != nil {
		return ChatMemory{}, err
	}
	if mem.Digested == nil {
		mem.Digested = map[string]int{}
	}
	text = s.policy.Text(redaction.Persistence, text)
	mem.Entries = append(mem.Entries, MemoryEntry{Date: date, Text: text})
	for maxBytes > 0 && mem.bytes() > maxBytes && len(mem.Entries) > 1 {
		mem.Entries = mem.Entries[1:]
	}
	if err := Atomic(s.memoryPath(key), mem); err != nil {
		return ChatMemory{}, err
	}
	return mem, nil
}

// MarkDigested records how many pairs of each conversation the digest has
// consumed and stamps the sweep time.
func (s *Store) MarkDigested(key string, counts map[string]int, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mem ChatMemory
	if err := read(s.memoryPath(key), &mem); err != nil {
		return err
	}
	if mem.Digested == nil {
		mem.Digested = map[string]int{}
	}
	for k, n := range counts {
		if n < 0 {
			continue
		}
		if n > mem.Digested[k] {
			mem.Digested[k] = n
		}
	}
	mem.LastDigestAt = at.UTC()
	return Atomic(s.memoryPath(key), mem)
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
