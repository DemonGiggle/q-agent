package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"q-agent/internal/redaction"
)

const HistoryLimit = 256 << 10
const ResultLimit = 8 << 10
const SessionLimit = 2

type Spec struct {
	Command string `json:"command"`
	PTY     bool   `json:"pty"`
	Columns int    `json:"columns"`
	Rows    int    `json:"rows"`
}
type Exit struct {
	Code int
	Err  error
}
type Child interface {
	io.Writer
	Resize(rows, columns int) error
	Stop()
	Wait() Exit
}
type Launcher func(context.Context, Spec, io.Writer) (Child, error)
type Hooks struct {
	OnStart        func()
	Wake           <-chan struct{}
	HasInput       func() bool
	Interim        func(string) error
	HumanAvailable bool
}
type Manager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	launcher Launcher
	policy   *redaction.Policy
	hooks    Hooks
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	wg       sync.WaitGroup
}
type session struct {
	mu      sync.Mutex
	id      string
	spec    Spec
	child   Child
	history []byte
	base    int64
	screen  *screen
	plain   plain
	done    chan struct{}
	changed chan struct{}
	replies chan []byte
	exit    *Exit
}
type Result struct {
	ID           string  `json:"session_id,omitempty"`
	Status       string  `json:"status"`
	PTY          bool    `json:"pty"`
	Output       string  `json:"output,omitempty"`
	Cursor       int64   `json:"cursor"`
	NextCursor   int64   `json:"next_cursor"`
	EndCursor    int64   `json:"end_cursor"`
	HistoryStart int64   `json:"history_start"`
	Truncated    bool    `json:"truncated"`
	ExitCode     *int    `json:"exit_code,omitempty"`
	Screen       *Screen `json:"screen,omitempty"`
	Reason       string  `json:"reason,omitempty"`
}

func New(ctx context.Context, launcher Launcher, policy *redaction.Policy, hooks Hooks) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: ctx, cancel: cancel, launcher: launcher, policy: policy, hooks: hooks, sessions: map[string]*session{}}
	go func() { <-ctx.Done(); m.Close() }()
	return m
}
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wg.Wait()
		return
	}
	m.closed = true
	m.cancel()
	for _, s := range m.sessions {
		s.child.Stop()
	}
	m.mu.Unlock()
	m.wg.Wait()
}
func (s *session) signal() { close(s.changed); s.changed = make(chan struct{}) }
func (m *Manager) Start(ctx context.Context, spec Spec, yield time.Duration) (string, error) {
	if len(spec.Command) > 64<<10 {
		return "", fmt.Errorf("command exceeds 64 KiB")
	}
	if strings.TrimSpace(spec.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	if spec.Rows == 0 {
		spec.Rows = 24
	}
	if spec.Columns == 0 {
		spec.Columns = 80
	}
	if err := dimensions(spec.Rows, spec.Columns); err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return "", context.Canceled
	}
	active := 0
	for _, s := range m.sessions {
		s.mu.Lock()
		running := s.exit == nil
		s.mu.Unlock()
		if running {
			active++
			if s.spec.Command == spec.Command {
				m.mu.Unlock()
				return "", fmt.Errorf("command already running in session %s", s.id)
			}
		}
	}
	if active >= SessionLimit {
		m.mu.Unlock()
		return "", fmt.Errorf("task session limit is %d; terminate a session first", SessionLimit)
	}
	// Retain no more than two session records, including completed sessions.
	if len(m.sessions) >= SessionLimit {
		for id, s := range m.sessions {
			s.mu.Lock()
			exited := s.exit != nil
			s.mu.Unlock()
			if exited {
				delete(m.sessions, id)
				break
			}
		}
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		m.mu.Unlock()
		return "", err
	}
	s := &session{id: hex.EncodeToString(idBytes), spec: spec, done: make(chan struct{}), changed: make(chan struct{})}
	if spec.PTY {
		s.screen = newScreen(spec.Rows, spec.Columns)
		s.replies = make(chan []byte, 16)
	}
	if m.hooks.OnStart != nil {
		m.hooks.OnStart()
	}
	child, err := m.launcher(m.ctx, spec, writerFunc(func(p []byte) (int, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.screen != nil {
			s.screen.feed(p)
			if replies := s.screen.replies.take(); len(replies) > 0 {
				select {
				case s.replies <- replies:
				default:
				}
			}
		}
		s.history = append(s.history, s.plain.feed(p)...)
		if len(s.history) > HistoryLimit {
			// Mask before discarding context so a cut cannot reveal a token suffix.
			s.history = []byte(m.policy.InteractiveMask(string(s.history)))
			n := len(s.history) - HistoryLimit
			for n < len(s.history) && !utf8.RuneStart(s.history[n]) {
				n++
			}
			s.base += int64(n)
			s.history = append([]byte(nil), s.history[n:]...)
		}
		s.signal()
		return len(p), nil
	}))
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	s.child = child
	m.sessions[s.id] = s
	if s.replies != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for {
				select {
				case <-m.ctx.Done():
					return
				case <-s.done:
					return
				case data := <-s.replies:
					if _, err := child.Write(data); err != nil {
						return
					}
				}
			}
		}()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		exit := child.Wait()
		s.mu.Lock()
		s.exit = &exit
		s.signal()
		close(s.done)
		s.mu.Unlock()
	}()
	m.mu.Unlock()
	// Initial launch yields for output without ending the owned process.
	if yield > 0 {
		timer := time.NewTimer(yield)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.done:
		case <-ctx.Done():
			return "", ctx.Err()
		case <-m.hooks.Wake:
		}
	}
	return m.read(s, 0, "history", 0, "")
}
func (m *Manager) get(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return nil, fmt.Errorf("unknown session in current task")
	}
	return s, nil
}
func (m *Manager) read(s *session, cursor int64, view string, offset int, reason string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exit != nil && s.exit.Err != nil {
		var fatal interface{ Fatal() bool }
		if errors.As(s.exit.Err, &fatal) && fatal.Fatal() {
			return "", s.exit.Err
		}
	}
	r := Result{ID: s.id, Status: "running", PTY: s.spec.PTY, Cursor: cursor, HistoryStart: s.base, EndCursor: s.base + int64(len(s.history)), Reason: reason}
	if s.exit != nil {
		r.Status = "exited"
		r.ExitCode = &s.exit.Code
	}
	if cursor < 0 || cursor > r.EndCursor {
		return "", fmt.Errorf("cursor outside history")
	}
	if cursor < s.base {
		cursor = s.base
		r.Truncated = true
	}
	r.Cursor = cursor
	r.NextCursor = cursor
	if view == "screen" {
		if s.screen == nil {
			return "", fmt.Errorf("screen view requires a PTY session")
		}
		full := s.screen.snapshot(m.policy)
		if offset < 0 || offset >= full.Rows {
			return "", fmt.Errorf("screen offset outside terminal")
		}
		full.Offset = offset
		full.Lines = full.Lines[offset:]
		r.Screen = full
		allEmphasis := full.Emphasis
		// Paginate complete rows, including their styles, after redaction.
		for count := len(full.Lines); count >= 1; count-- {
			full.Lines = full.Lines[:count]
			full.Emphasis = nil
			for _, e := range allEmphasis {
				if e.Row >= offset && e.Row < offset+count {
					full.Emphasis = append(full.Emphasis, e)
				}
			}
			full.NextOffset = -1
			if offset+count < full.Rows {
				full.NextOffset = offset + count
			}
			r.Truncated = full.NextOffset >= 0
			b, _ := json.Marshal(r)
			if len(b) <= ResultLimit {
				return string(b), nil
			}
		}
		// An unusually styled row still returns text and explicit truncation.
		full.Emphasis = nil
		r.Truncated = true
		return encode(r)
	}
	if view != "" && view != "history" {
		return "", fmt.Errorf("view must be history or screen")
	}
	history := string(s.history)
	s.mu.Unlock()
	safe := m.policy.InteractiveMask(history)
	s.mu.Lock()
	data := safe[cursor-r.HistoryStart:]
	if len(data) > 0 && !utf8.RuneStart(data[0]) {
		return "", fmt.Errorf("cursor must point to a UTF-8 boundary")
	}
	n := min(len(data), ResultLimit-1024)
	for {
		for n > 0 && !utf8.ValidString(data[:n]) {
			n--
		}
		r.Output = data[:n]
		r.NextCursor = cursor + int64(n)
		r.Truncated = r.Truncated || r.NextCursor < r.EndCursor
		b, _ := json.Marshal(r)
		if len(b) <= ResultLimit {
			return string(b), nil
		}
		n = n * 3 / 4
	}
}
func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	if len(b) > ResultLimit {
		return "", fmt.Errorf("encoded process result exceeds limit")
	}
	return string(b), err
}
func (m *Manager) List() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := []Result{}
	for _, id := range ids {
		s := m.sessions[id]
		s.mu.Lock()
		r := Result{ID: id, Status: "running", PTY: s.spec.PTY, HistoryStart: s.base, EndCursor: s.base + int64(len(s.history))}
		if s.exit != nil {
			r.Status = "exited"
			r.ExitCode = &s.exit.Code
		}
		s.mu.Unlock()
		list = append(list, r)
	}
	return encode(map[string]any{"sessions": list})
}

type Request struct {
	Action    string   `json:"action"`
	ID        string   `json:"session_id"`
	Cursor    int64    `json:"cursor"`
	View      string   `json:"view"`
	Offset    int      `json:"offset"`
	Data      string   `json:"data"`
	Keys      []string `json:"keys"`
	Rows      int      `json:"rows"`
	Columns   int      `json:"columns"`
	TimeoutMS *int     `json:"timeout_ms"`
	Human     bool     `json:"human_input"`
	Message   string   `json:"message"`
}

func (m *Manager) Execute(ctx context.Context, a Request) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := m.Err(); err != nil {
		return "", err
	}
	if a.Action == "list" {
		return m.List()
	}
	if a.Human && !m.hooks.HumanAvailable {
		return "", fmt.Errorf("human input unavailable for this task")
	}
	s, err := m.get(a.ID)
	if err != nil {
		return "", err
	}
	switch a.Action {
	case "read":
	case "write", "keys":
		data := a.Data
		if a.Action == "keys" {
			data, err = Keys(a.Keys)
			s.mu.Lock()
			application := s.screen != nil && s.screen.appCursor
			s.mu.Unlock()
			if application {
				data = strings.NewReplacer("\x1b[A", "\x1bOA", "\x1b[B", "\x1bOB", "\x1b[C", "\x1bOC", "\x1b[D", "\x1bOD").Replace(data)
			}
			if err != nil {
				return "", err
			}
		}
		if len(data) > 64<<10 {
			return "", fmt.Errorf("input exceeds 64 KiB")
		}
		s.mu.Lock()
		exited := s.exit != nil
		s.mu.Unlock()
		if exited {
			return "", fmt.Errorf("session has exited")
		}
		if _, err = s.child.Write([]byte(data)); err != nil {
			return "", err
		}
	case "resize":
		if !s.spec.PTY {
			return "", fmt.Errorf("resize requires a PTY session")
		}
		if err = dimensions(a.Rows, a.Columns); err != nil {
			return "", err
		}
		if err = s.child.Resize(a.Rows, a.Columns); err != nil {
			return "", err
		}
		s.mu.Lock()
		s.screen.resize(a.Rows, a.Columns)
		s.signal()
		s.mu.Unlock()
	case "terminate":
		s.child.Stop()
		select {
		case <-s.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	case "wait":
		duration := 30 * time.Second
		if a.TimeoutMS != nil {
			if *a.TimeoutMS < 0 || *a.TimeoutMS > 60000 {
				return "", fmt.Errorf("timeout_ms must be between 0 and 60000")
			}
			duration = time.Duration(*a.TimeoutMS) * time.Millisecond
		}
		if len(a.Message) > ResultLimit {
			return "", fmt.Errorf("interim message exceeds 8 KiB")
		}
		if a.Message != "" && m.hooks.Interim != nil {
			if err = m.hooks.Interim(m.policy.Text(redaction.Remote, a.Message)); err != nil {
				return "", err
			}
		}
		timer := time.NewTimer(duration)
		defer timer.Stop()
		reason := "output"
		s.mu.Lock()
		changed := s.changed
		ready := s.exit != nil || s.base+int64(len(s.history)) > a.Cursor
		s.mu.Unlock()
		if m.hooks.HasInput != nil && m.hooks.HasInput() {
			reason = "user_input"
		} else if !ready {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-m.ctx.Done():
				return "", m.ctx.Err()
			case <-s.done:
				reason = "exit"
			case <-changed:
			case <-m.hooks.Wake:
				reason = "user_input"
			case <-timer.C:
				reason = "timeout"
			}
		}
		return m.read(s, a.Cursor, a.View, a.Offset, reason)
	default:
		return "", fmt.Errorf("unknown process action")
	}
	return m.read(s, a.Cursor, a.View, a.Offset, "")
}
func dimensions(rows, cols int) error {
	if rows < 1 || rows > 120 || cols < 2 || cols > 240 {
		return fmt.Errorf("terminal dimensions must be 2..240 columns and 1..120 rows")
	}
	return nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Err propagates a failed execution boundary even when the next operation is
// unrelated to the session. Application exit codes remain ordinary observations.
func (m *Manager) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		var err error
		if s.exit != nil {
			err = s.exit.Err
		}
		s.mu.Unlock()
		var fatal interface{ Fatal() bool }
		if errors.As(err, &fatal) && fatal.Fatal() {
			return err
		}
	}
	return nil
}
