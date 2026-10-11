package localtools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"q-agent/internal/llm"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFilesAndEditFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	call := func(name string, args map[string]string) (string, error) {
		data, _ := json.Marshal(args)
		return Execute(context.Background(), llm.ToolCall{Name: name, Arguments: data}, Options{WorkingDir: "/workspace"})
	}
	if _, err := call("write", map[string]string{"path": path, "content": "same same"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("edit", map[string]string{"path": path, "old_text": "same", "new_text": "different"}); err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "same same" {
		t.Fatal("failed edit changed file")
	}
	if _, err := call("edit", map[string]string{"path": path, "old_text": "same same", "new_text": "different"}); err != nil {
		t.Fatal(err)
	}
	out, err := call("read", map[string]string{"path": path})
	if err != nil || out != "different" {
		t.Fatalf("%s %v", out, err)
	}
}
func TestNullContentCannotTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	os.WriteFile(path, []byte("keep"), 0600)
	args, _ := json.Marshal(map[string]any{"path": path, "content": nil})
	_, err := Execute(context.Background(), llm.ToolCall{Name: "write", Arguments: args}, Options{WorkingDir: "/workspace"})
	data, _ := os.ReadFile(path)
	if err == nil || string(data) != "keep" {
		t.Fatal("null content changed a file")
	}
}
func TestOutputBoundAndJSON(t *testing.T) {
	var buffer Buffer
	for i := 0; i < 100; i++ {
		buffer.Write([]byte(strings.Repeat("😀", 1000)))
	}
	out := buffer.String()
	if len(out) > OutputLimit || !utf8.ValidString(out) || !strings.HasSuffix(out, Truncation) {
		t.Fatal("invalid truncation")
	}
	for _, args := range []string{`null`, `{"path":9}`, `{"path":null}`, `{"path":"/x","unknown":true}`} {
		_, err := Execute(context.Background(), llm.ToolCall{Name: "read", Arguments: json.RawMessage(args)}, Options{WorkingDir: "/workspace"})
		if err == nil {
			t.Fatal("invalid arguments accepted")
		}
	}
}
