// Package state stores host-only conversations, scheduled work, and reply delivery.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"q-agent/internal/llm"
	"q-agent/internal/platform"
	"q-agent/internal/redaction"
	"strings"
	"sync"
)

const MaxPairs = 12
const MaxHistoryBytes = 32 << 10

type Pair struct {
	User   string   `json:"user"`
	Answer string   `json:"answer"`
	Steers []string `json:"steers,omitempty"`
}
type Conversation struct {
	Thinking       string `json:"thinking,omitempty"`
	ModelSelection string `json:"model_selection,omitempty"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Identity       string `json:"identity,omitempty"`
	Pairs          []Pair `json:"pairs"`
}

func (c Conversation) Messages() []llm.Message {
	var messages []llm.Message
	for _, p := range c.Pairs {
		messages = append(messages, llm.Message{Role: "user", Content: p.User})
		for _, steer := range p.Steers {
			messages = append(messages, llm.Message{Role: "user", Content: steer})
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: p.Answer})
	}
	return messages
}
func (c *Conversation) Bound() {
	for {
		data, _ := json.Marshal(c)
		if len(c.Pairs) == 0 || len(c.Pairs) <= MaxPairs && len(data) <= MaxHistoryBytes {
			return
		}
		c.Pairs = c.Pairs[1:]
	}
}

type Reply struct {
	Keyboard *platform.InlineKeyboard `json:"keyboard,omitempty"`
	ID       string                   `json:"id"`
	Route    platform.Message         `json:"route"`
	Parts    []string                 `json:"parts"`
	Next     int                      `json:"next"`
}
type Store struct {
	mu              sync.Mutex
	dir             string
	cursor          int64
	policy          *redaction.Policy
	outbox          []Reply
	schedules       scheduleState
	preferSchedules bool
}

func Open(dir string, policy *redaction.Policy) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, policy: policy, cursor: -1}
	var cursor struct {
		UpdateID int64 `json:"update_id"`
	}
	cursor.UpdateID = -1
	if err := read(filepath.Join(dir, "cursor.json"), &cursor); err != nil {
		return nil, err
	}
	s.cursor = cursor.UpdateID
	if err := read(filepath.Join(dir, "outbox.json"), &s.outbox); err != nil {
		return nil, err
	}
	for _, r := range s.outbox {
		if r.Next < 0 || r.Next >= len(r.Parts) || len(r.Parts) == 0 {
			return nil, fmt.Errorf("invalid outbox cursor")
		}
	}
	if err := read(filepath.Join(dir, "schedules.json"), &s.schedules); err != nil {
		return nil, err
	}
	if err := s.validateSchedules(); err != nil {
		return nil, fmt.Errorf("invalid schedules: %w", err)
	}
	return s, nil
}
func read(path string, target any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// Atomic writes sync data and directory before returning. Accepted update IDs
// are committed before provisioning or model execution can begin.
func Atomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".atomic-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) Cursor() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.cursor }
func (s *Store) Accept(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id <= s.cursor {
		return false, nil
	}
	if err := Atomic(filepath.Join(s.dir, "cursor.json"), map[string]int64{"update_id": id}); err != nil {
		return false, err
	}
	s.cursor = id
	return true, nil
}
func (s *Store) path(key string) string {
	return filepath.Join(s.dir, "conversations", platform.WorkspaceKey(key, 0)+".json")
}
func (s *Store) Load(key string) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	err := read(s.path(key), &c)
	c.Bound()
	return c, err
}
func (s *Store) Save(key string, c Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range c.Pairs {
		c.Pairs[i].User = s.policy.Text(redaction.Persistence, c.Pairs[i].User)
		c.Pairs[i].Answer = s.policy.Text(redaction.Persistence, c.Pairs[i].Answer)
		c.Pairs[i].Steers = append([]string(nil), c.Pairs[i].Steers...)
		for j, steer := range c.Pairs[i].Steers {
			c.Pairs[i].Steers[j] = s.policy.Text(redaction.Persistence, steer)
		}
	}
	c.Bound()
	return Atomic(s.path(key), c)
}
func (s *Store) Reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Atomic(s.path(key), Conversation{})
}
func (s *Store) Queue(route platform.Message, parts []string) error {
	return s.QueueKeyboard(route, parts, nil)
}
func (s *Store) QueueKeyboard(route platform.Message, parts []string, keyboard *platform.InlineKeyboard) error {
	return s.queue(route, parts, keyboard, "")
}

// QueueNotice keeps an acknowledgement distinct from a later final answer
// to the same update, including input arriving as the previous run finishes.
func (s *Store) QueueNotice(route platform.Message, parts []string) error {
	return s.queue(route, parts, nil, ":notice")
}

func (s *Store) queue(route platform.Message, parts []string, keyboard *platform.InlineKeyboard, suffix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(parts) == 0 {
		return fmt.Errorf("reply must have at least one part")
	}
	route.Text = ""
	id := fmt.Sprintf("%s:%d%s", route.Platform, route.UpdateID, suffix)
	for _, r := range s.outbox {
		if r.ID == id {
			return nil
		}
	}
	copyParts := append([]string(nil), parts...)
	for i := range copyParts {
		copyParts[i] = s.policy.Text(redaction.Remote, copyParts[i])
	}
	next := append(append([]Reply(nil), s.outbox...), Reply{ID: id, Route: route, Parts: copyParts, Keyboard: keyboard})
	if err := Atomic(filepath.Join(s.dir, "outbox.json"), next); err != nil {
		return err
	}
	s.outbox = next
	return nil
}
func (s *Store) Pending() (Reply, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preferSchedules {
		if reply, ok := s.pendingScheduleReply(); ok {
			return reply, true
		}
	}
	if len(s.outbox) == 0 {
		return s.pendingScheduleReply()
	}
	r := s.outbox[0]
	r.Parts = append([]string(nil), r.Parts...)
	return r, true
}
func (s *Store) Delivered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(id, "schedule:") {
		err := s.deliveredScheduleReply(id)
		if err == nil {
			s.preferSchedules = false
		}
		return err
	}
	if len(s.outbox) == 0 || s.outbox[0].ID != id {
		return fmt.Errorf("outbox changed")
	}
	next := append([]Reply(nil), s.outbox...)
	next[0].Next++
	if next[0].Next >= len(next[0].Parts) {
		next = next[1:]
	}
	if err := Atomic(filepath.Join(s.dir, "outbox.json"), next); err != nil {
		return err
	}
	s.outbox = next
	s.preferSchedules = true
	return nil
}

func (s *Store) DeferReply(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(id, "schedule:") {
		err := s.deferScheduleReply(id)
		if err == nil {
			s.preferSchedules = false
		}
		return err
	}
	if len(s.outbox) == 0 || s.outbox[0].ID != id {
		return fmt.Errorf("outbox changed")
	}
	if len(s.outbox) == 1 {
		s.preferSchedules = true
		return nil
	}
	next := append(append([]Reply(nil), s.outbox[1:]...), s.outbox[0])
	if err := Atomic(filepath.Join(s.dir, "outbox.json"), next); err != nil {
		return err
	}
	s.outbox = next
	s.preferSchedules = true
	return nil
}
