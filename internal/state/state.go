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
	"q-agent/internal/trust"
	"strings"
	"sync"
)

const MaxPairs = 12
const MaxHistoryBytes = 32 << 10

type Pair struct {
	Interrupted bool     `json:"interrupted,omitempty"`
	Progress    string   `json:"progress,omitempty"`
	User        string   `json:"user"`
	Answer      string   `json:"answer"`
	Steers      []string `json:"steers,omitempty"`
}
type Conversation struct {
	// Resolved when loading a task; not part of persisted topic history.
	MaxSteps       int    `json:"-"`
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
		if p.Interrupted && p.Progress != "" {
			messages = append(messages, llm.Message{Role: "user", Content: trust.Wrap("interrupted_task_tool_observations", p.Progress), Origin: "interrupted_task_tool_observations", Untrusted: true})
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: p.Answer})
	}
	return messages
}
func (c *Conversation) Bound() {
	for i := range c.Pairs {
		if c.Pairs[i].Interrupted {
			c.Pairs[i] = boundInterrupted(c.Pairs[i])
		}
	}
	for {
		data, _ := json.Marshal(c)
		if len(c.Pairs) == 0 || len(c.Pairs) <= MaxPairs && len(data) <= MaxHistoryBytes {
			return
		}
		c.Pairs = c.Pairs[1:]
	}
}

type PhotoAttachment struct {
	LocalPath string `json:"local_path,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	Caption   string `json:"caption,omitempty"`
	Fallback  string `json:"fallback,omitempty"`
}

type Reply struct {
	Keyboard *platform.InlineKeyboard `json:"keyboard,omitempty"`
	ID       string                   `json:"id"`
	Route    platform.Message         `json:"route"`
	Parts    []string                 `json:"parts"`
	Photos   []PhotoAttachment        `json:"photos,omitempty"`
	Next     int                      `json:"next"`
}

// Total counts deliverable items: photos first, then text parts.
func (r Reply) Total() int { return len(r.Photos) + len(r.Parts) }
type Store struct {
	mu              sync.Mutex
	dir             string
	cursor          int64
	policy          *redaction.Policy
	outbox          []Reply
	schedules       scheduleState
	preferSchedules bool
	memories        map[string]ChatMemory
	memoryLimits    MemoryLimits
	memoryLastKey   string
	atomic          func(string, any) error
}

func Open(dir string, policy *redaction.Policy, limits ...MemoryLimits) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, policy: policy, cursor: -1, memories: map[string]ChatMemory{}, atomic: Atomic, memoryLimits: MemoryLimits{MaxBytes: 8192, MaxPendingBytes: DefaultMaxPendingBytes}}
	if len(limits) > 0 {
		if limits[0].MaxBytes > 0 {
			s.memoryLimits.MaxBytes = limits[0].MaxBytes
		}
		if limits[0].MaxPendingBytes > 0 {
			s.memoryLimits.MaxPendingBytes = limits[0].MaxPendingBytes
		}
	}
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
		total := len(r.Photos) + len(r.Parts)
		if total == 0 || r.Next < 0 || r.Next >= total {
			return nil, fmt.Errorf("invalid outbox cursor")
		}
		if len(r.Photos) > 10 {
			return nil, fmt.Errorf("invalid outbox photos")
		}
		for _, p := range r.Photos {
			if (p.LocalPath == "") == (p.RemoteURL == "") {
				return nil, fmt.Errorf("invalid outbox photo")
			}
		}
	}
	if err := read(filepath.Join(dir, "schedules.json"), &s.schedules); err != nil {
		return nil, err
	}
	if err := s.validateSchedules(); err != nil {
		return nil, fmt.Errorf("invalid schedules: %w", err)
	}
	if err := s.recoverMemory(); err != nil {
		return nil, err
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
	if err := s.recoverHistory(); err != nil {
		return Conversation{}, err
	}
	var c Conversation
	err := read(s.path(key), &c)
	c.Bound()
	return c, err
}
func (s *Store) Save(key string, c Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverHistory(); err != nil {
		return err
	}
	c = s.cleanConversation(c)
	return s.atomic(s.path(key), c)
}
func (s *Store) Reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recoverHistory(); err != nil {
		return err
	}
	return s.atomic(s.path(key), Conversation{})
}
func (s *Store) Queue(route platform.Message, parts []string) error {
	return s.QueueKeyboard(route, parts, nil)
}
func (s *Store) QueueKeyboard(route platform.Message, parts []string, keyboard *platform.InlineKeyboard) error {
	return s.queue(route, parts, keyboard, "")
}

// QueueWithPhotos queues text parts plus native photo attachments (photos
// first, then text). Parts may be empty when photos are present.
func (s *Store) QueueWithPhotos(route platform.Message, parts []string, photos []PhotoAttachment, keyboard *platform.InlineKeyboard) error {
	return s.queueWithPhotos(route, parts, photos, keyboard, "")
}

// QueueNotice keeps an acknowledgement distinct from a later final answer
// to the same update, including input arriving as the previous run finishes.
func (s *Store) QueueNotice(route platform.Message, parts []string) error {
	return s.queue(route, parts, nil, ":notice")
}

func (s *Store) queue(route platform.Message, parts []string, keyboard *platform.InlineKeyboard, suffix string) error {
	return s.queueWithPhotos(route, parts, nil, keyboard, suffix)
}

func (s *Store) queueWithPhotos(route platform.Message, parts []string, photos []PhotoAttachment, keyboard *platform.InlineKeyboard, suffix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(parts) == 0 && len(photos) == 0 {
		return fmt.Errorf("reply must have at least one part")
	}
	if len(photos) > 10 {
		return fmt.Errorf("at most 10 photos per reply")
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
	copyPhotos := append([]PhotoAttachment(nil), photos...)
	for i := range copyPhotos {
		if copyPhotos[i].LocalPath == "" && copyPhotos[i].RemoteURL == "" {
			return fmt.Errorf("photo must have a local path or remote URL")
		}
		if copyPhotos[i].LocalPath != "" && copyPhotos[i].RemoteURL != "" {
			return fmt.Errorf("photo must have either a local path or remote URL")
		}
		copyPhotos[i].Caption = s.policy.Text(redaction.Remote, copyPhotos[i].Caption)
		copyPhotos[i].Fallback = s.policy.Text(redaction.Remote, copyPhotos[i].Fallback)
	}
	next := append(append([]Reply(nil), s.outbox...), Reply{ID: id, Route: route, Parts: copyParts, Photos: copyPhotos, Keyboard: keyboard})
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
		if reply, ok := s.pendingBackgroundReply(); ok {
			return reply, true
		}
	}
	if len(s.outbox) == 0 {
		return s.pendingBackgroundReply()
	}
	r := s.outbox[0]
	r.Parts = append([]string(nil), r.Parts...)
	r.Photos = append([]PhotoAttachment(nil), r.Photos...)
	return r, true
}
func (s *Store) Delivered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(id, "memory:") {
		err := s.updateMemoryReply(id, true)
		if err == nil {
			s.preferSchedules = false
		}
		return err
	}
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
	if next[0].Next >= next[0].Total() {
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
	if strings.HasPrefix(id, "memory:") {
		err := s.updateMemoryReply(id, false)
		if err == nil {
			s.preferSchedules = false
		}
		return err
	}
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

func (s *Store) pendingBackgroundReply() (Reply, bool) {
	if reply, ok := s.pendingScheduleReply(); ok {
		return reply, true
	}
	return s.pendingMemoryReply()
}
