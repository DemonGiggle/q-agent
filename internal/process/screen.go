// Package process owns bounded, task-scoped interactive process sessions.
package process

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/muesli/termenv"
	"github.com/vito/midterm"
	"q-agent/internal/redaction"
)

// screen isolates the emulator from application IO and bounds partial escape
// sequences. Use Decode/Process so malformed input cannot trigger midterm's
// panic logger (which includes raw application data).
type screen struct {
	term      *midterm.Terminal
	pending   []byte
	discard   byte
	escaped   bool
	appCursor bool
	replies   responseBuffer
}

func newScreen(rows, cols int) *screen {
	s := &screen{term: midterm.NewTerminal(rows, cols)}
	s.term.ForwardResponses = &s.replies
	return s
}
func (s *screen) feed(p []byte) {
	data := append(s.pending, p...)
	s.pending = nil
	if s.discard != 0 {
		end := 0
		for end < len(data) {
			b := data[end]
			end++
			if s.discard == ']' {
				if b == 7 || s.escaped && b == '\\' {
					s.discard = 0
					s.escaped = false
					break
				}
				s.escaped = b == 27
			} else if b >= 0x40 && b <= 0x7e {
				s.discard = 0
				break
			}
		}
		data = data[end:]
		if s.discard != 0 {
			return
		}
	}
	data = bytes.ReplaceAll(data, []byte{0xc2, 0x9b}, []byte("\x1b["))
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			s.pending = append([]byte(nil), data...)
			return
		}
		if data[0] != 27 && data[0] != 0xc2 && !utf8.Valid(data[:1]) {
			_, size := utf8.DecodeRune(data)
			if size == 1 {
				data = data[1:]
				continue
			}
		}
		b := bytes.NewBuffer(data)
		cmd, _, err := midterm.Decode(b)
		if err != nil {
			if len(data) <= 4096 {
				s.pending = append([]byte(nil), data...)
			} else if len(data) > 1 && data[0] == 27 {
				if data[1] == ']' || data[1] == 'P' || data[1] == '^' || data[1] == '_' {
					s.discard = ']'
				} else {
					s.discard = '['
				}
			}
			return
		}
		sequence := data[:len(data)-b.Len()]
		data = b.Bytes()
		// OSC/DCS payloads and oversized CSI parameters are not needed for a
		// screen snapshot, and must never reach emulator diagnostics/allocations.
		if len(sequence) > 1 && sequence[0] == 27 && (sequence[1] == ']' || sequence[1] == 'P' || sequence[1] == '^' || sequence[1] == '_') {
			continue
		}
		if !safeSequence(sequence, max(s.term.Height, s.term.Width)) {
			continue
		}
		if bytes.Equal(sequence, []byte("\x1b[?1h")) {
			s.appCursor = true
		}
		if bytes.Equal(sequence, []byte("\x1b[?1l")) {
			s.appCursor = false
		}
		s.apply(cmd)
	}
}
func (s *screen) apply(cmd midterm.Command) {
	defer func() {
		if recover() != nil {
			s.term = midterm.NewTerminal(s.term.Height, s.term.Width)
			s.term.ForwardResponses = &s.replies
		}
	}()
	t := s.term
	// Process does not perform Write's pre-command scroll. Keep that behavior
	// here, with all raw data kept away from the library's diagnostic logger.
	if t.Cursor.Y >= t.Height {
		scroll, _, _ := midterm.Decode(bytes.NewBufferString("\x1b[S"))
		_ = t.Process(scroll)
		t.Cursor.Y = t.Height - 1
	}
	t.Cursor.Y = max(0, min(t.Cursor.Y, t.Height-1))
	t.Cursor.X = max(0, min(t.Cursor.X, t.Width-1))
	_ = t.Process(cmd)
}
func (s *screen) resize(rows, cols int) {
	s.term.Resize(rows, cols)
	s.term.Cursor.Y = min(s.term.Cursor.Y, rows-1)
}

type Emphasis struct {
	Row        int    `json:"row"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Bold       bool   `json:"bold,omitempty"`
	Reverse    bool   `json:"reverse,omitempty"`
	Underline  bool   `json:"underline,omitempty"`
	Foreground string `json:"foreground,omitempty"`
	Background string `json:"background,omitempty"`
}
type Screen struct {
	Columns       int        `json:"columns"`
	Rows          int        `json:"rows"`
	CursorRow     int        `json:"cursor_row"`
	CursorColumn  int        `json:"cursor_column"`
	CursorVisible bool       `json:"cursor_visible"`
	Alternate     bool       `json:"alternate"`
	Lines         []string   `json:"lines"`
	Emphasis      []Emphasis `json:"emphasis,omitempty"`
	Offset        int        `json:"offset"`
	NextOffset    int        `json:"next_offset"`
}

func (s *screen) snapshot(policy *redaction.Policy) *Screen {
	t := s.term
	out := &Screen{Columns: t.Width, Rows: t.Height, CursorRow: min(t.Cursor.Y, t.Height-1), CursorColumn: t.Cursor.X, CursorVisible: t.CursorVisible, Alternate: t.IsAlt, NextOffset: -1}
	lines := make([]string, t.Height)
	for y, row := range t.Content {
		lines[y] = strings.TrimRight(string(row), " \x00")
		for x := 0; x < len(row); {
			f := t.Format[y][x]
			end := x + 1
			for end < len(row) && t.Format[y][end] == f {
				end++
			}
			e := Emphasis{Row: y, Start: x, End: end, Bold: f.IsBold(), Reverse: f.IsReverse(), Underline: f.IsUnderline()}
			if f.Fg != nil {
				e.Foreground = termenv.ConvertToRGB(f.Fg).Hex()
			}
			if f.Bg != nil {
				e.Background = termenv.ConvertToRGB(f.Bg).Hex()
			}
			if e.Bold || e.Reverse || e.Underline || e.Foreground != "" || e.Background != "" {
				out.Emphasis = append(out.Emphasis, e)
			}
			if f.IsConceal() {
				r := []rune(lines[y])
				for i := x; i < min(end, len(r)); i++ {
					r[i] = '*'
				}
				lines[y] = string(r)
			}
			x = end
		}
	}
	// Detection spans adjacent rows; masking preserves positions and geometry.
	raw := strings.Join(lines, "\n")
	masked := policy.InteractiveMask(raw)
	var display strings.Builder
	for i, r := range raw {
		size := utf8.RuneLen(r)
		if raw[i:i+size] != masked[i:i+size] {
			display.WriteByte('*')
		} else {
			display.WriteRune(r)
		}
	}
	out.Lines = strings.Split(display.String(), "\n")
	return out
}

// plain removes ANSI decorations incrementally for cursor-based history.
// Screen rendering is independent and retains cursor movement and redraws.
type plain struct {
	state   byte
	pending []byte
}

func (p *plain) feed(data []byte) []byte {
	data = append(p.pending, data...)
	p.pending = nil
	if len(data) > 0 {
		start := len(data) - 1
		for start > 0 && !utf8.RuneStart(data[start]) {
			start--
		}
		if !utf8.FullRune(data[start:]) {
			p.pending = append([]byte(nil), data[start:]...)
			data = data[:start]
		}
	}
	data = bytes.ReplaceAll(data, []byte{0xc2, 0x9b}, []byte("\x1b["))
	var out []byte
	for _, b := range data {
		switch p.state {
		case 1:
			if b == '[' {
				p.state = 2
			} else if b == ']' || b == 'P' || b == '^' || b == '_' {
				p.state = 3
			} else {
				p.state = 0
			}
		case 2:
			if b >= 0x40 && b <= 0x7e {
				p.state = 0
			}
		case 3:
			if b == 7 {
				p.state = 0
			} else if b == 27 {
				p.state = 4
			}
		case 4:
			if b == '\\' {
				p.state = 0
			} else {
				p.state = 3
			}
		default:
			if b == 27 {
				p.state = 1
			} else if b >= 32 || b == '\n' || b == '\r' || b == '\t' {
				out = append(out, b)
			}
		}
	}
	out = []byte(strings.ToValidUTF8(string(out), "�"))
	return out
}

func safeSequence(seq []byte, limit int) bool {
	if len(seq) < 3 || seq[0] != 27 || seq[1] != '[' {
		return true
	}
	last := seq[len(seq)-1]
	if last == 'm' {
		limit = 255
	}
	if last == 'h' || last == 'l' {
		limit = 65535
	}
	digits := ""
	for _, b := range seq[2:] {
		if b == '-' {
			return false
		}
		if b >= '0' && b <= '9' {
			digits += string(b)
			if len(digits) > 6 {
				return false
			}
		} else if digits != "" {
			n, err := strconv.Atoi(digits)
			if err != nil || n > limit {
				return false
			}
			digits = ""
		}
	}
	return true
}

// Terminal query responses are bounded and forwarded asynchronously to stdin.
// Applications such as gh/survey wait for cursor reports even while reading
// their prompt; interpreting screen output alone cannot unblock those reads.
type responseBuffer struct{ data []byte }

func (b *responseBuffer) Write(p []byte) (int, error) {
	n := len(p)
	p = p[:min(len(p), 4096-len(b.data))]
	b.data = append(b.data, p...)
	return n, nil
}
func (b *responseBuffer) take() []byte { data := b.data; b.data = nil; return data }
