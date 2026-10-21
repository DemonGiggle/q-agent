package sandbox

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestSessionFramesRejectMalformedAndOversizedProtocol(t *testing.T) {
	for _, data := range [][]byte{nil, {0, 0, 0, 0}, {0, 2, 0, 1}, {0, 0, 0, 4, '{', 'b', 'a', 'd'}, {0, 0, 0, 2, '{'}} {
		if _, err := readFrame(bytes.NewReader(data)); err == nil {
			t.Fatal("malformed frame accepted")
		}
	}
	var b bytes.Buffer
	if err := (&frameWriter{w: &b}).send(streamFrame{Type: "output", Data: []byte("\x00{\"type\":\"exit\"}\n😀")}); err != nil {
		t.Fatal(err)
	}
	f, err := readFrame(&b)
	if err != nil || string(f.Data) != "\x00{\"type\":\"exit\"}\n😀" {
		t.Fatalf("application bytes confused framing: %+v %v", f, err)
	}
	for _, payload := range []string{`{"type":"ready","unknown":true}`, `{"type":"ready"}{"type":"exit"}`} {
		b.Reset()
		binary.Write(&b, binary.BigEndian, uint32(len(payload)))
		b.WriteString(payload)
		if _, err = readFrame(&b); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	if err = (&frameWriter{w: io.Discard}).send(streamFrame{Type: "output", Data: []byte(strings.Repeat("x", frameLimit))}); err == nil {
		t.Fatal("oversized output frame accepted")
	}
}
