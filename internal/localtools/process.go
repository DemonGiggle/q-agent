package localtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"q-agent/internal/llm"
	"q-agent/internal/process"
)

type ShellArgs struct {
	Command    string `json:"command"`
	PTY        bool   `json:"pty"`
	Background bool   `json:"background"`
	YieldMS    *int   `json:"yield_ms"`
}

func decodeObject(data []byte, target any) error {
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return fmt.Errorf("arguments must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func ParseShell(data []byte) (ShellArgs, error) {
	var a ShellArgs
	if err := decodeObject(data, &a); err != nil {
		return a, err
	}
	// Preserve strict rejection of null and wrong types for every supplied key.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	for name, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return a, fmt.Errorf("%s cannot be null", name)
		}
	}
	if a.YieldMS != nil && (*a.YieldMS < 0 || *a.YieldMS > 60000) {
		return a, fmt.Errorf("yield_ms must be between 0 and 60000")
	}
	return a, nil
}

// ManagedCall is shared by host and sandbox runners. Ordinary calls keep their
// synchronous behavior and never create a session manager in a worker.
func ManagedCall(ctx context.Context, call llm.ToolCall, m *process.Manager) (string, bool, error) {
	if call.Name == "process" {
		var a process.Request
		if err := decodeObject(call.Arguments, &a); err != nil {
			return "", true, err
		}
		out, err := m.Execute(ctx, a)
		return out, true, err
	}
	if call.Name == "shell" {
		a, err := ParseShell(call.Arguments)
		if err != nil {
			return "", true, err
		}
		if a.PTY || a.Background {
			yield := time.Second
			if a.YieldMS != nil {
				yield = time.Duration(*a.YieldMS) * time.Millisecond
			}
			out, err := m.Start(ctx, process.Spec{Command: a.Command, PTY: a.PTY}, yield)
			return out, true, err
		}
	}
	return "", false, nil
}
func processSchema() llm.Tool {
	return schema("process", "Manage current-task processes: list, read history or terminal screen, write stdin, send named keys, resize, wait, terminate. IDs expire when the task ends. Results are bounded JSON with continuation cursors. Screen rows and style positions are zero-based.", map[string]any{
		"action":      map[string]any{"type": "string", "enum": []string{"list", "read", "write", "keys", "resize", "wait", "terminate"}},
		"session_id":  textField("ID from shell or process.list; current task only"),
		"cursor":      map[string]any{"type": "integer", "minimum": 0, "description": "History byte cursor; use next_cursor to continue (default 0)"},
		"view":        map[string]any{"type": "string", "enum": []string{"history", "screen"}},
		"offset":      map[string]any{"type": "integer", "minimum": 0, "description": "Screen starting row; continue at screen.next_offset"},
		"data":        textField("Exact stdin text for write; include newline for pipe input"),
		"keys":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Named terminal keys, e.g. Enter, Up, Down, Escape, Ctrl-C, Ctrl-D"},
		"rows":        map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
		"columns":     map[string]any{"type": "integer", "minimum": 2, "maximum": 240},
		"timeout_ms":  map[string]any{"type": "integer", "minimum": 0, "maximum": 60000, "description": "Wait for output, exit, or user steering; default 30000"},
		"human_input": map[string]any{"type": "boolean", "description": "Wait requests a human reply; unavailable in scheduled tasks"},
		"message":     textField("Optional interim user message before wait, e.g. browser authorization URL and code; keeps the task open"),
	}, "action")
}

// UsesSessions leaves ordinary synchronous tools independent of session state.
func UsesSessions(call llm.ToolCall) (bool, error) {
	if call.Name == "process" {
		return true, nil
	}
	if call.Name != "shell" {
		return false, nil
	}
	a, err := ParseShell(call.Arguments)
	return a.PTY || a.Background, err
}
