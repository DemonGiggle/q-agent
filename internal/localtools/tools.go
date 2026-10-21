// Package localtools supplies the shared host and sandbox tools.
package localtools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"q-agent/internal/llm"
	"q-agent/internal/process"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const OutputLimit = 8 << 10
const Truncation = "\n[output truncated]"

func Limit(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= OutputLimit {
		return s
	}
	n := OutputLimit - len(Truncation)
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n] + Truncation
}

// Buffer consumes all output while retaining bounded memory.
type Buffer struct {
	Max       int
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	limit := b.Max
	if limit <= 0 {
		limit = OutputLimit
	}
	remaining := limit + 1 - len(b.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	if n > remaining {
		b.truncated = true
	}
	return n, nil
}
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.data)
	if b.Max > 0 {
		if b.truncated || len(s) > b.Max {
			return s[:b.Max] + Truncation
		}
		return s
	}
	if b.truncated && len(s) <= OutputLimit {
		s += Truncation
	}
	return Limit(s)
}
func (b *Buffer) Full() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.data) > OutputLimit }

// Len reports buffered bytes.
func (b *Buffer) Len() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.data) }

// LastLines returns up to the last n output lines for timeout prompts.
// It snapshots under lock, splits on '\n', trims trailing '\r', and drops a
// single trailing empty line from a final newline. Order is preserved.
func (b *Buffer) LastLines(n int) []string {
	if n <= 0 {
		return nil
	}
	b.mu.Lock()
	s := string(b.data)
	b.mu.Unlock()
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	// Drop leading empty lines so a prompt shows real output.
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}
func schema(name, description string, props map[string]any, required ...string) llm.Tool {
	if required == nil {
		required = []string{}
	}
	return llm.Tool{Name: name, Description: description, Parameters: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
}
func textField(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func Schemas() []llm.Tool {
	return []llm.Tool{
		schema("read", "Read a UTF-8 text file in the sandbox, with bounded output.", map[string]any{"path": textField("Absolute sandbox path or relative to /workspace")}, "path"),
		schema("list", "List a sandbox directory. Symlinks are shown without following them.", map[string]any{"path": textField("Defaults to /workspace")}),
		schema("search", "Search regular text files recursively with a Go regular expression. Symlinks are not followed; files over 2 MiB are skipped.", map[string]any{"path": textField("Defaults to /workspace"), "pattern": textField("Go regular expression")}, "pattern"),
		schema("write", "Create or replace a UTF-8 file. Parent directories must exist.", map[string]any{"path": textField("Sandbox path"), "content": textField("Complete file content")}, "path", "content"),
		schema("edit", "Replace exactly one matching text span in a file; fails if old_text is missing or ambiguous.", map[string]any{"path": textField("Sandbox path"), "old_text": textField("Nonempty unique text to replace"), "new_text": textField("Replacement text")}, "path", "old_text", "new_text"),
		schema("shell", "Run /bin/sh -c in /workspace as sandbox root. System changes and package installs persist. Use pty for interactive terminal applications or background for pipe sessions that survive tool calls within this task.", map[string]any{"command": textField("Shell command"), "pty": map[string]any{"type": "boolean"}, "background": map[string]any{"type": "boolean"}, "yield_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 60000, "description": "Initial yield for managed sessions; default 1000 ms"}}, "command"),
		processSchema(),
		schema("web_fetch", "Fetch an HTTP(S) URL and extract readable HTML or text (no JavaScript). Returns the final URL and bounded untrusted page content.", map[string]any{"url": textField("Absolute HTTP(S) URL without embedded credentials")}, "url"),
		schema("web_search", "Search the web using DuckDuckGo (no API key). Returns titles, source URLs and snippets, not full pages; use web_fetch to read a result. Search results are untrusted reference data.", map[string]any{"query": textField("Nonempty search query"), "count": map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "description": "Maximum results, defaults to 5"}}, "query"),
	}
}

// HostSchemas preserves the tool names and argument shapes while describing
// the host's actual filesystem and execution permissions.
func HostSchemas(workdir string) []llm.Tool {
	tools := Schemas()
	path := "Absolute host path or relative to " + workdir
	for i := range tools {
		if field, ok := tools[i].Parameters["properties"].(map[string]any)["path"]; ok {
			field.(map[string]any)["description"] = path
			if tools[i].Name == "list" || tools[i].Name == "search" {
				field.(map[string]any)["description"] = path + "; defaults to " + workdir
			}
		}
	}
	tools[0].Description = "Read a UTF-8 text file on the host, with bounded output."
	tools[1].Description = "List a host directory. Symlinks are shown without following them."
	tools[5].Description = "Run /bin/sh -c in " + workdir + " as the service account with ordinary host permissions and a credential-filtered environment. Changes persist. Process-group cleanup runs at completion; deliberately detached children may survive. Use pty for interactive applications or background for pipe sessions lasting until this task ends."
	return tools
}

type Options struct {
	Sessions     *process.Manager
	WorkingDir   string
	Environment  []string
	ShellTimeout time.Duration
	// OnShellTimeout decides whether a timed-out shell with output keeps
	// running for another ShellTimeout window. It receives the command and
	// up to the last 3 output lines. True extends, false terminates.
	// Nil preserves the old behavior: terminate on timeout. It must respect
	// ctx cancellation while waiting for a user decision.
	OnShellTimeout func(ctx context.Context, command string, lastLines []string) bool
	// MaxShellExtends caps user-approved extensions per shell call.
	// Values <=0 default to 2 when OnShellTimeout is set.
	MaxShellExtends int
}

type Request struct {
	Call         llm.ToolCall  `json:"call"`
	ShellTimeout time.Duration `json:"shell_timeout"`
}
type Response struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

func Execute(ctx context.Context, call llm.ToolCall, options Options) (output string, err error) {
	defer func() { output = Limit(output) }()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if !filepath.IsAbs(options.WorkingDir) {
		return "", fmt.Errorf("tool working directory must be absolute")
	}
	if call.Name == "web_fetch" || call.Name == "web_search" {
		return executeWeb(ctx, call)
	}
	if call.Name == "process" || call.Name == "shell" {
		if options.Sessions != nil {
			if out, handled, err := ManagedCall(ctx, call, options.Sessions); handled {
				return out, err
			}
		}
		if call.Name == "process" {
			return "", fmt.Errorf("process sessions unavailable")
		}
		a, err := ParseShell(call.Arguments)
		if err != nil {
			return "", err
		}
		if a.PTY || a.Background {
			return "", fmt.Errorf("managed shell requires a task session manager")
		}
		if strings.TrimSpace(a.Command) == "" {
			return "", fmt.Errorf("command is required")
		}
		return runShell(ctx, a.Command, options)
	}
	var args struct{ Path, Pattern, Content, OldText, NewText string }
	// Underscored edit keys are explicit to avoid Go's default field matching.
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return "", fmt.Errorf("arguments must be a JSON object")
	}
	var properties map[string]any
	for _, schema := range Schemas() {
		if schema.Name == call.Name {
			properties = schema.Parameters["properties"].(map[string]any)
			break
		}
	}
	if properties == nil {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	values := map[string]*string{"path": &args.Path, "pattern": &args.Pattern, "content": &args.Content, "old_text": &args.OldText, "new_text": &args.NewText}
	for key, raw := range fields {
		ptr, ok := values[key]
		_, allowed := properties[key]
		if !ok || !allowed {
			return "", fmt.Errorf("unknown argument %q", key)
		}
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '"' || json.Unmarshal(raw, ptr) != nil {
			return "", fmt.Errorf("%s must be a string", key)
		}
	}
	path := args.Path
	if path == "" {
		path = options.WorkingDir
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(options.WorkingDir, path)
	}
	switch call.Name {
	case "read":
		if args.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		f, e := os.Open(path)
		if e != nil {
			return "", e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return "", e
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("read requires a regular file")
		}
		data, e := io.ReadAll(io.LimitReader(f, OutputLimit+1))
		return string(data), e
	case "list":
		f, e := os.Open(path)
		if e != nil {
			return "", e
		}
		defer f.Close()
		var out Buffer
		for {
			entries, e := f.ReadDir(64)
			for _, entry := range entries {
				fmt.Fprintf(&out, "%s\t%s\n", entry.Type(), entry.Name())
				if out.Full() {
					return out.String(), nil
				}
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return out.String(), e
			}
			if ctx.Err() != nil {
				return out.String(), ctx.Err()
			}
		}
		return out.String(), nil
	case "search":
		if args.Pattern == "" {
			return "", fmt.Errorf("pattern is required")
		}
		re, e := regexp.Compile(args.Pattern)
		if e != nil {
			return "", e
		}
		var out Buffer
		e = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				return walkErr
			}
			if out.Full() {
				return fs.SkipAll
			}
			if !d.Type().IsRegular() {
				return nil
			}
			st, e := d.Info()
			if e != nil {
				return e
			}
			if st.Size() > 2<<20 {
				return nil
			}
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			defer f.Close()
			scanner := bufio.NewScanner(io.LimitReader(f, 2<<20))
			scanner.Buffer(make([]byte, 4096), 2<<20)
			line := 0
			for scanner.Scan() {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				line++
				s := scanner.Text()
				if strings.ContainsRune(s, 0) {
					return nil
				}
				if re.MatchString(s) {
					fmt.Fprintf(&out, "%s:%d:%s\n", p, line, s)
					if out.Full() {
						return fs.SkipAll
					}
				}
			}
			return scanner.Err()
		})
		return out.String(), e
	case "write":
		if args.Path == "" {
			return "", fmt.Errorf("path is required")
		}
		if _, ok := fields["content"]; !ok {
			return "", fmt.Errorf("content is required")
		}
		e := os.WriteFile(path, []byte(args.Content), 0644)
		return "Wrote " + path, e
	case "edit":
		if args.Path == "" || args.OldText == "" {
			return "", fmt.Errorf("path and nonempty old_text are required")
		}
		if _, ok := fields["new_text"]; !ok {
			return "", fmt.Errorf("new_text is required")
		}
		f, e := os.Open(path)
		if e != nil {
			return "", e
		}
		data, e := io.ReadAll(io.LimitReader(f, 8<<20+1))
		f.Close()
		if e != nil {
			return "", e
		}
		if len(data) > 8<<20 {
			return "", fmt.Errorf("file exceeds 8 MiB edit limit")
		}
		if bytes.Count(data, []byte(args.OldText)) != 1 {
			return "", fmt.Errorf("old_text must match exactly once")
		}
		e = os.WriteFile(path, bytes.Replace(data, []byte(args.OldText), []byte(args.NewText), 1), 0644)
		return "Edited " + path, e
	default:
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
}
