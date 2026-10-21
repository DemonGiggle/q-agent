package sandbox

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"q-agent/internal/process"
)

const frameLimit = 128 << 10

// Application bytes are base64 JSON data inside length-delimited frames, never
// interpreted as protocol. Control traffic cannot reach application stdin.
type streamFrame struct {
	Type    string        `json:"type"`
	Spec    *process.Spec `json:"spec,omitempty"`
	Data    []byte        `json:"data,omitempty"`
	Rows    int           `json:"rows,omitempty"`
	Columns int           `json:"columns,omitempty"`
	Code    int           `json:"code,omitempty"`
	Error   string        `json:"error,omitempty"`
}

func readFrame(r io.Reader) (streamFrame, error) {
	var f streamFrame
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return f, err
	}
	if size == 0 || size > frameLimit {
		return f, fmt.Errorf("invalid stream frame size")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return f, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return f, fmt.Errorf("invalid stream frame")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return f, fmt.Errorf("invalid trailing stream data")
	}
	return f, nil
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *frameWriter) send(f streamFrame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(data) > frameLimit {
		return fmt.Errorf("stream frame exceeds limit")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err = binary.Write(w.w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	n, err := w.w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
