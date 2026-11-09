package sandbox

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"q-agent/internal/redaction"
)

// mcpServe wires serveMCPStdio to in-memory pipes: the test speaks the
// host side of the channel (frames), the child is a real executable.
func mcpServe(t *testing.T, dir string, command string, args ...string) (write func(mcpFrame), read func() mcpFrame, wait func() error) {
	t.Helper()
	return mcpServeWithEnv(t, dir, nil, command, args...)
}

func mcpServeWithEnv(t *testing.T, dir string, env []string, command string, args ...string) (write func(mcpFrame), read func() mcpFrame, wait func() error) {
	t.Helper()
	clientToWorkerR, clientToWorkerW := io.Pipe()
	workerToClientR, workerToClientW := io.Pipe()
	writer := &mcpWriter{w: clientToWorkerW}
	write = func(f mcpFrame) {
		t.Helper()
		if len(f.Data) > 0 {
			chunks := f
			for len(chunks.Data) > 0 {
				n := min(len(chunks.Data), mcpStdioChunk)
				if err := writer.send(mcpFrame{Type: f.Type, Data: chunks.Data[:n]}); err != nil {
					t.Fatalf("write frame: %v", err)
				}
				chunks.Data = chunks.Data[n:]
			}
			return
		}
		if err := writer.send(f); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	read = func() mcpFrame {
		t.Helper()
		done := make(chan mcpFrame, 1)
		errCh := make(chan error, 1)
		go func() {
			f, err := readMCPFrame(workerToClientR)
			if err != nil {
				errCh <- err
				return
			}
			done <- f
		}()
		select {
		case f := <-done:
			return f
		case err := <-errCh:
			t.Fatalf("read frame: %v", err)
			return mcpFrame{}
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for worker frame")
			return mcpFrame{}
		}
	}
	done := make(chan error, 1)
	go func() { done <- serveMCPStdio(dir, env, clientToWorkerR, workerToClientW) }()
	t.Cleanup(func() {
		clientToWorkerW.Close()
		workerToClientR.Close()
	})
	wait = func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for worker exit")
			return nil
		}
	}
	write(mcpFrame{Type: "start", Command: command, Args: args})
	return write, read, wait
}

func mcpShell(t *testing.T) string {
	t.Helper()
	for _, sh := range []string{"/bin/sh", "/usr/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	t.Skip("no POSIX shell available")
	return ""
}

func TestMCPFrameCodecRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := &mcpWriter{w: &buf}
	in := mcpFrame{Type: "stdin", Data: []byte("hello \x00 binary \xff")}
	if err := w.send(in); err != nil {
		t.Fatalf("send: %v", err)
	}
	out, err := readMCPFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Type != in.Type || !bytes.Equal(out.Data, in.Data) {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestMCPFrameCodecRejectsMalformed(t *testing.T) {
	payload, _ := json.Marshal(mcpFrame{Type: "stdout", Data: []byte("x")})
	framed := func(data []byte) io.Reader {
		var buf bytes.Buffer
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(data)))
		buf.Write(data)
		return &buf
	}
	for _, tc := range []struct {
		name string
		r    io.Reader
	}{
		{"empty", bytes.NewReader(nil)},
		{"zero size", bytes.NewReader([]byte{0, 0, 0, 0})},
		{"oversize", func() io.Reader {
			var buf bytes.Buffer
			_ = binary.Write(&buf, binary.BigEndian, uint32(frameLimit+1))
			return &buf
		}()},
		{"bad json", framed([]byte("{bad"))},
		{"unknown field", framed([]byte(`{"type":"stdout","nope":1}`))},
		{"trailing data", framed(append(payload, 'x'))},
		{"short payload", func() io.Reader {
			var buf bytes.Buffer
			_ = binary.Write(&buf, binary.BigEndian, uint32(100))
			buf.Write([]byte("{}"))
			return &buf
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readMCPFrame(tc.r); err == nil {
				t.Fatal("expected frame error")
			}
		})
	}
}

func TestMCPServeEchoSeparatesStreams(t *testing.T) {
	sh := mcpShell(t)
	dir := t.TempDir()
	write, read, wait := mcpServe(t, dir, sh, "-c", "echo out-line; echo err-line >&2; cat")
	if ready := read(); ready.Type != "ready" || ready.Error != "" {
		t.Fatalf("ready: %+v", ready)
	}
	write(mcpFrame{Type: "stdin", Data: []byte("through\n")})
	var sawStdout, sawStderr bool
	for i := 0; i < 20 && !(sawStdout && sawStderr); i++ {
		f := read()
		switch f.Type {
		case "stdout":
			if strings.Contains(string(f.Data), "out-line") || strings.Contains(string(f.Data), "through") {
				sawStdout = true
			}
			if strings.Contains(string(f.Data), "err-line") {
				t.Fatalf("stderr leaked into stdout: %q", f.Data)
			}
		case "stderr":
			if strings.Contains(string(f.Data), "err-line") {
				sawStderr = true
			}
			if strings.Contains(string(f.Data), "out-line") || strings.Contains(string(f.Data), "through") {
				t.Fatalf("stdout leaked into stderr: %q", f.Data)
			}
		default:
			t.Fatalf("unexpected frame before exit: %+v", f)
		}
	}
	if !sawStdout || !sawStderr {
		t.Fatalf("missing split output: stdout=%v stderr=%v", sawStdout, sawStderr)
	}
	write(mcpFrame{Type: "close-stdin"})
	for i := 0; i < 20; i++ {
		// Late stdout chunks may precede the exit frame; only the exit
		// code matters here.
		if exit := read(); exit.Type == "exit" {
			if exit.Code != 0 {
				t.Fatalf("exit: %+v", exit)
			}
			break
		} else if i == 19 {
			t.Fatalf("no exit frame, last: %+v", exit)
		}
	}
	if err := wait(); err != nil {
		t.Fatalf("worker: %v", err)
	}
}

func TestMCPServeExitCode(t *testing.T) {
	sh := mcpShell(t)
	_, read, wait := mcpServe(t, t.TempDir(), sh, "-c", "exit 7")
	if ready := read(); ready.Type != "ready" {
		t.Fatalf("ready: %+v", ready)
	}
	if exit := read(); exit.Type != "exit" || exit.Code != 7 {
		t.Fatalf("exit: %+v", exit)
	}
	if err := wait(); err != nil {
		t.Fatalf("worker: %v", err)
	}
}

func TestMCPServeStartFailureIsOrdinary(t *testing.T) {
	_, read, wait := mcpServe(t, t.TempDir(), "/nonexistent-mcp-server-binary")
	ready := read()
	if ready.Type != "ready" || ready.Error == "" {
		t.Fatalf("expected ready error, got %+v", ready)
	}
	if err := wait(); err != nil {
		t.Fatalf("start failure must not be a boundary error: %v", err)
	}
}

func TestMCPServeRejectsBadHello(t *testing.T) {
	clientR, clientW := io.Pipe()
	workerR, workerW := io.Pipe()
	defer clientW.Close()
	defer workerR.Close()
	done := make(chan error, 1)
	go func() { done <- serveMCPStdio(t.TempDir(), nil, clientR, workerW) }()
	w := &mcpWriter{w: clientW}
	if err := w.send(mcpFrame{Type: "stdin", Data: []byte("x")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected hello error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not reject bad hello")
	}
}

func TestMCPServeValidationBounds(t *testing.T) {
	if err := validateMCPCommand("", nil); err == nil {
		t.Fatal("expected empty command error")
	}
	if err := validateMCPCommand("/bin/true", make([]string, maxMCPArgs+1)); err == nil {
		t.Fatal("expected arg count error")
	}
	if err := validateMCPCommand("/bin/true", []string{strings.Repeat("x", maxMCPArgBytes+1)}); err == nil {
		t.Fatal("expected arg size error")
	}
	if err := validateMCPCommand(strings.Repeat("x", maxMCPCommandBytes+1), nil); err == nil {
		t.Fatal("expected command size error")
	}
	if err := validateMCPCommand("/bin/true", []string{"a", "b"}); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}
}

func TestMCPServeUnknownControlKillsChild(t *testing.T) {
	sh := mcpShell(t)
	write, read, wait := mcpServe(t, t.TempDir(), sh, "-c", "exec sleep 30")
	if ready := read(); ready.Type != "ready" {
		t.Fatalf("ready: %+v", ready)
	}
	write(mcpFrame{Type: "bogus-control"})
	if err := wait(); err == nil {
		t.Fatal("expected control error")
	}
}

func TestMCPStderrKeepsTail(t *testing.T) {
	buf := newMCPStderr(16)
	if _, err := buf.Write([]byte("0123456789abcdefFINAL")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buf.String(); got != "56789abcdefFINAL" {
		t.Fatalf("tail = %q", got)
	}
}

func TestMCPServeFiltersEnvironment(t *testing.T) {
	sh := mcpShell(t)
	dir := t.TempDir()
	// A credential-named variable must not reach the child even when the
	// worker environment carries it.
	t.Setenv("Q_AGENT_MCP_TEST_LEAK_TOKEN", "leak-value")
	env, _ := redaction.FilterEnvironment(os.Environ())
	write, read, wait := mcpServeWithEnv(t, dir, env, sh, "-c", "echo \"token=[${Q_AGENT_MCP_TEST_LEAK_TOKEN-unset}]\"; echo done")
	if ready := read(); ready.Type != "ready" {
		t.Fatalf("ready: %+v", ready)
	}
	var out []byte
	for i := 0; i < 5; i++ {
		f := read()
		if f.Type == "stdout" {
			out = append(out, f.Data...)
			if strings.Contains(string(out), "done") {
				break
			}
		}
	}
	if !strings.Contains(string(out), "token=[unset]") {
		t.Fatalf("credential env reached child: %q", out)
	}
	write(mcpFrame{Type: "close-stdin"})
	if exit := read(); exit.Type != "exit" {
		t.Fatalf("exit: %+v", exit)
	}
	if err := wait(); err != nil {
		t.Fatalf("worker: %v", err)
	}
}
