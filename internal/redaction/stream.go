package redaction

import "strings"

const MaxPendingLine = 64 * 1024

// Stream emits only complete logical lines, retaining PEM state across flushes.
// Once a line overflows, discard it through its terminator instead of emitting
// a prefix that could contain half of a credential.
type Stream struct {
	policy       *Policy
	sink         Sink
	pending      string
	overflow     bool
	overflowTail string
	pem          bool
}

func (p *Policy) Stream(s Sink) *Stream { return &Stream{policy: effective(p), sink: s} }
func (s *Stream) Feed(text string) string {
	if !s.policy.Enabled(s.sink) {
		return text
	}
	var out strings.Builder
	for len(text) > 0 {
		end := strings.IndexByte(text, '\n')
		part := text
		if end >= 0 {
			part = text[:end]
		}
		if !s.overflow {
			if len(s.pending)+len(part) > MaxPendingLine {
				s.trackOverflow(s.pending + part)
				s.pending = ""
				s.overflow = true
			} else {
				s.pending += part
			}
		} else {
			s.trackOverflow(part)
		}
		if end < 0 {
			break
		}
		out.WriteString(s.finish())
		out.WriteByte('\n')
		text = text[end+1:]
	}
	return out.String()
}
func (s *Stream) finish() string {
	line := strings.TrimSuffix(s.pending, "\r")
	s.pending = ""
	if s.overflow {
		s.overflow = false
		s.overflowTail = ""
		return Marker
	}
	var out strings.Builder
	for {
		if s.pem {
			end := pemEnd.FindStringIndex(line)
			out.WriteString(Marker)
			if end == nil {
				return out.String()
			}
			s.pem = false
			line = line[end[1]:]
		} else {
			begin := pemBegin.FindStringIndex(line)
			if begin == nil {
				out.WriteString(s.policy.line(line))
				return out.String()
			}
			out.WriteString(s.policy.line(line[:begin[0]]))
			s.pem = true
			line = line[begin[1]:]
		}
	}
}

func (s *Stream) Flush() string {
	if s.pending == "" && !s.overflow {
		return ""
	}
	return s.finish()
}

// Keep PEM state even when the line containing a delimiter is too long to keep.
func (s *Stream) trackOverflow(part string) {
	text := s.overflowTail + part
	for {
		begin, end := pemBegin.FindStringIndex(text), pemEnd.FindStringIndex(text)
		if begin == nil && end == nil {
			break
		}
		if begin != nil && (end == nil || begin[0] < end[0]) {
			s.pem = true
			text = text[begin[1]:]
		} else {
			s.pem = false
			text = text[end[1]:]
		}
	}
	if len(text) > 128 {
		text = text[len(text)-128:]
	}
	s.overflowTail = text
}
