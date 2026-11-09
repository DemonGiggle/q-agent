package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"q-agent/internal/localtools"
	"q-agent/internal/process"
	"q-agent/internal/redaction"
)

func streamWorker(in io.Reader, out io.Writer) error {
	first, err := readFrame(in)
	if err != nil {
		return err
	}
	if first.Type != "start" || first.Spec == nil {
		return fmt.Errorf("expected session start frame")
	}
	spec := *first.Spec
	if spec.Rows < 1 || spec.Rows > 120 || spec.Columns < 2 || spec.Columns > 240 {
		return fmt.Errorf("invalid terminal dimensions")
	}
	writer := &frameWriter{w: out}
	// Hold output until the ready frame, including commands with immediate output.
	var gate sync.Mutex
	gate.Lock()
	child, err := process.HostLauncher("/workspace", os.Environ())(context.Background(), spec, streamOutput{writer: writer, gate: &gate})
	if err != nil {
		gate.Unlock()
		return writer.send(streamFrame{Type: "ready", Error: err.Error()})
	}
	defer func() { child.Stop(); _ = child.Wait() }()
	if err = writer.send(streamFrame{Type: "ready"}); err != nil {
		gate.Unlock()
		return err
	}
	gate.Unlock()
	exited := make(chan process.Exit, 1)
	go func() { exited <- child.Wait() }()
	requests := make(chan streamFrame)
	readErrors := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	// Return on exit even if the host has not sent another control request.
	go func() {
		for {
			f, err := readFrame(in)
			if err != nil {
				readErrors <- err
				return
			}
			select {
			case requests <- f:
			case <-done:
				return
			}
		}
	}()
	for {
		select {
		case exit := <-exited:
			return writer.send(streamFrame{Type: "exit", Code: exit.Code})
		case err := <-readErrors:
			if err == io.EOF {
				return nil
			}
			return err
		case f := <-requests:
			var appErr error
			switch f.Type {
			case "write":
				if len(f.Data) > 64<<10 {
					return fmt.Errorf("oversized input frame")
				}
				_, appErr = child.Write(f.Data)
			case "resize":
				if !spec.PTY || f.Rows < 1 || f.Rows > 120 || f.Columns < 2 || f.Columns > 240 {
					return fmt.Errorf("invalid resize frame")
				}
				appErr = child.Resize(f.Rows, f.Columns)
			case "terminate":
				child.Stop()
			default:
				return fmt.Errorf("unknown control frame")
			}
			response := streamFrame{Type: "result"}
			if appErr != nil {
				response.Error = appErr.Error()
			}
			if err = writer.send(response); err != nil {
				return err
			}
		}
	}
}

type streamOutput struct {
	writer *frameWriter
	gate   *sync.Mutex
}

func (w streamOutput) Write(p []byte) (int, error) {
	w.gate.Lock()
	defer w.gate.Unlock()
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 16<<10)
		if err := w.writer.send(streamFrame{Type: "output", Data: p[:n]}); err != nil {
			return 0, err
		}
		p = p[n:]
	}
	return total, nil
}

// ---- MCP stdio channel (issue #47) ----
//
// One MCP server's stdin/stdout byte streams cross the sandbox boundary as
// chunked application data inside length-delimited frames; stderr crosses as
// diagnostics only, never as protocol. Application bytes are never
// interpreted as control traffic in either direction.
//
// Frame types on the worker pipes:
//   - host "start" {command, args}: spawn the server (argv used literally,
//     no shell) with Dir /workspace and filtered environment.
//   - worker "ready" {error?}: server started, or the ordinary start failure.
//   - host "stdin" {data}: client-to-server bytes.
//   - host "close-stdin": graceful shutdown signal (close server stdin).
//   - worker "stdout"/"stderr" {data}: server output chunks.
//   - worker "exit" {code}: server terminated; no further frames follow
//     except pipe EOF.
//
// A malformed frame, unknown control type, or worker setup failure is a
// FatalError boundary failure: the host must surface it without falling
// back to host execution. A "ready" error is an ordinary server failure.

// mcpStdioMode selects this channel in the worker argv.
const mcpStdioMode = "mcp-stdio"

// mcpStdioChunk caps application bytes per frame so base64 expansion stays
// well under frameLimit.
const mcpStdioChunk = 32 << 10

// Bounds enforced by the worker (the sandbox boundary is the enforcement
// point); the mcpclient factories mirror them for fail-fast validation.
const (
	maxMCPCommandBytes = 4 << 10
	maxMCPArgs         = 128
	maxMCPArgBytes     = 32 << 10
	maxMCPArgsTotal    = 256 << 10
)

type mcpFrame struct {
	Type    string   `json:"type"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Data    []byte   `json:"data,omitempty"`
	Code    int      `json:"code,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func readMCPFrame(r io.Reader) (mcpFrame, error) {
	var f mcpFrame
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return f, err
	}
	if size == 0 || size > frameLimit {
		return f, fmt.Errorf("invalid mcp stdio frame size")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return f, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return f, fmt.Errorf("invalid mcp stdio frame")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return f, fmt.Errorf("invalid trailing mcp stdio data")
	}
	return f, nil
}

// mcpWriter serializes frames for one direction of the channel.
type mcpWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *mcpWriter) send(f mcpFrame) error {
	if len(f.Data) > mcpStdioChunk {
		return fmt.Errorf("mcp stdio frame exceeds chunk limit")
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(data) > frameLimit {
		return fmt.Errorf("mcp stdio frame exceeds limit")
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

// validateMCPCommand enforces the argv contract at the sandbox boundary:
// the command is an explicit executable path or PATH lookup name with no
// shell interpretation, and arguments are bounded literals.
func validateMCPCommand(command string, args []string) error {
	if len(command) == 0 || len(command) > maxMCPCommandBytes {
		return fmt.Errorf("invalid mcp stdio command")
	}
	if len(args) > maxMCPArgs {
		return fmt.Errorf("mcp stdio args exceed limit")
	}
	total := 0
	for _, arg := range args {
		if len(arg) > maxMCPArgBytes {
			return fmt.Errorf("mcp stdio arg exceeds limit")
		}
		total += len(arg)
	}
	if total > maxMCPArgsTotal {
		return fmt.Errorf("mcp stdio args exceed limit")
	}
	return nil
}

// mcpWorkerCtl abstracts worker teardown so the channel logic stays portable
// and unit-testable; *workerProcess satisfies it on Linux.
type mcpWorkerCtl interface {
	stop()
	wait() error
}

// mcpStdioWorker serves one MCP server inside the namespace: it spawns the
// configured executable with Dir /workspace and a credential-filtered
// environment, then bridges the channel frames to the child pipes.
func mcpStdioWorker(in io.Reader, out io.Writer) error {
	env, _ := redaction.FilterEnvironment(os.Environ())
	return serveMCPStdio("/workspace", env, in, out)
}

// serveMCPStdio runs the channel over in/out with an explicit working
// directory and environment so tests can exercise it without a namespace.
func serveMCPStdio(dir string, env []string, in io.Reader, out io.Writer) error {
	hello, err := readMCPFrame(in)
	if err != nil {
		return err
	}
	if hello.Type != "start" {
		return fmt.Errorf("expected mcp stdio start frame")
	}
	if err := validateMCPCommand(hello.Command, hello.Args); err != nil {
		return err
	}
	cmd := exec.Command(hello.Command, hello.Args...)
	cmd.Dir = dir
	cmd.Env = append([]string(nil), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	send := &mcpWriter{w: out}
	if err := cmd.Start(); err != nil {
		// Ordinary server failure (missing executable, bad directory):
		// report it as a ready error, not a boundary failure.
		_ = send.send(mcpFrame{Type: "ready", Error: localtools.Limit(err.Error())})
		return nil
	}
	if err := send.send(mcpFrame{Type: "ready"}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	exited := make(chan int, 1)
	go func() {
		_ = cmd.Wait()
		code := -1
		if state := cmd.ProcessState; state != nil {
			code = state.ExitCode()
		}
		exited <- code
	}()
	var pumps sync.WaitGroup
	pumps.Add(2)
	pump := func(r io.Reader, frameType string) {
		defer pumps.Done()
		buf := make([]byte, mcpStdioChunk)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if werr := send.send(mcpFrame{Type: frameType, Data: buf[:n]}); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pump(stdout, "stdout")
	go pump(stderr, "stderr")
	type control struct {
		frame mcpFrame
		err   error
	}
	controls := make(chan control)
	go func() {
		for {
			f, err := readMCPFrame(in)
			controls <- control{frame: f, err: err}
			if err != nil {
				return
			}
		}
	}()
	stdinClosed := false
	closeStdin := func() {
		if !stdinClosed {
			stdinClosed = true
			_ = stdin.Close()
		}
	}
	for {
		select {
		case code := <-exited:
			// Drain remaining output before reporting the exit so no
			// diagnostic or protocol byte is lost.
			closeStdin()
			pumps.Wait()
			_ = send.send(mcpFrame{Type: "exit", Code: code})
			return nil
		case c := <-controls:
			if c.err != nil {
				_ = cmd.Process.Kill()
				<-exited
				closeStdin()
				pumps.Wait()
				if c.err == io.EOF || c.err == io.ErrUnexpectedEOF {
					return nil
				}
				return c.err
			}
			switch c.frame.Type {
			case "stdin":
				// A write failure means the child already exited; the
				// exit path reports termination.
				_, _ = stdin.Write(c.frame.Data)
			case "close-stdin":
				closeStdin()
			default:
				_ = cmd.Process.Kill()
				<-exited
				closeStdin()
				pumps.Wait()
				return fmt.Errorf("unknown mcp stdio control frame")
			}
		}
	}
}

// mcpStderr keeps the tail of stderr diagnostics under a bound.
type mcpStderr struct {
	mu      sync.Mutex
	data    []byte
	max     int
	dropped bool
}

func newMCPStderr(max int) *mcpStderr {
	if max <= 0 {
		max = 64 << 10
	}
	return &mcpStderr{max: max}
}

func (b *mcpStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.dropped = true
		b.data = append([]byte(nil), b.data[len(b.data)-b.max:]...)
	}
	return len(p), nil
}

func (b *mcpStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// MCPStdioProcess is one sandboxed MCP server's host-side endpoint. The
// child lives inside the sandbox namespace; all output arrives as channel
// frames. The zero value is unusable; create via StartMCPStdio.
type MCPStdioProcess struct {
	worker mcpWorkerCtl
	send   *mcpWriter
	input  *os.File
	output *os.File

	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderr  *mcpStderr

	mu       sync.Mutex
	exited   bool
	code     int
	closing  bool
	protoErr error

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// mcpStdioReadyTimeout bounds the worker handshake.
const mcpStdioReadyTimeout = 30 * time.Second

// mcpStdioGrace bounds graceful shutdown after stdin close before the
// namespace is terminated.
const mcpStdioGrace = 5 * time.Second

var errMCPStdioClosed = fmt.Errorf("mcp stdio process is closed")

// openMCPStdio performs the worker handshake over established pipes and
// starts the output pump. worker teardown on failure is the caller's.
func openMCPStdio(ctx context.Context, worker mcpWorkerCtl, input, output *os.File, command string, args []string) (*MCPStdioProcess, error) {
	if err := validateMCPCommand(command, args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stdoutR, stdoutW := io.Pipe()
	p := &MCPStdioProcess{
		worker:  worker,
		send:    &mcpWriter{w: input},
		input:   input,
		output:  output,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stderr:  newMCPStderr(64 << 10),
		done:    make(chan struct{}),
	}
	if err := p.send.send(mcpFrame{Type: "start", Command: command, Args: append([]string(nil), args...)}); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, FatalError{err}
	}
	type ready struct {
		frame mcpFrame
		err   error
	}
	readyCh := make(chan ready, 1)
	go func() { f, err := readMCPFrame(output); readyCh <- ready{frame: f, err: err} }()
	timeout := time.NewTimer(mcpStdioReadyTimeout)
	defer timeout.Stop()
	select {
	case r := <-readyCh:
		if r.err != nil {
			stdoutR.Close()
			stdoutW.Close()
			return nil, FatalError{fmt.Errorf("mcp stdio handshake: %w", r.err)}
		}
		if r.frame.Type != "ready" {
			stdoutR.Close()
			stdoutW.Close()
			return nil, FatalError{fmt.Errorf("invalid mcp stdio handshake")}
		}
		if r.frame.Error != "" {
			stdoutR.Close()
			stdoutW.Close()
			// Ordinary server start failure, not a boundary failure.
			return nil, fmt.Errorf("mcp stdio server failed to start: %s", r.frame.Error)
		}
	case <-timeout.C:
		stdoutR.Close()
		stdoutW.Close()
		return nil, FatalError{fmt.Errorf("mcp stdio handshake timed out")}
	case <-ctx.Done():
		stdoutR.Close()
		stdoutW.Close()
		return nil, ctx.Err()
	}
	go p.pump()
	return p, nil
}

// pump reassembles child stdout into the pipe reader and captures stderr
// diagnostics. It ends on the worker's exit teardown (EOF after a clean
// exit) or on a protocol violation, which is recorded as a fatal boundary
// error.
func (p *MCPStdioProcess) pump() {
	defer close(p.done)
	defer p.stdoutW.Close()
	for {
		f, err := readMCPFrame(p.output)
		if err != nil {
			p.mu.Lock()
			quiet := p.exited || p.closing
			if !quiet {
				p.protoErr = FatalError{fmt.Errorf("mcp stdio protocol: %w", err)}
			}
			p.mu.Unlock()
			if !quiet {
				p.stdoutW.CloseWithError(p.protocolError())
				p.worker.stop()
			}
			return
		}
		switch f.Type {
		case "stdout":
			// The reader may be gone during teardown; keep draining so
			// the exit frame is still observed.
			_, _ = p.stdoutW.Write(f.Data)
		case "stderr":
			_, _ = p.stderr.Write(f.Data)
		case "exit":
			p.mu.Lock()
			p.exited = true
			p.code = f.Code
			p.mu.Unlock()
		default:
			p.mu.Lock()
			p.protoErr = FatalError{fmt.Errorf("mcp stdio protocol: unknown frame %q", f.Type)}
			p.mu.Unlock()
			p.stdoutW.CloseWithError(p.protocolError())
			p.worker.stop()
			return
		}
	}
}

func (p *MCPStdioProcess) protocolError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.protoErr != nil {
		return p.protoErr
	}
	return errMCPStdioClosed
}

// Reader returns the child's reassembled stdout byte stream. It carries
// MCP protocol bytes only; stderr arrives via Diagnostic.
func (p *MCPStdioProcess) Reader() io.ReadCloser { return p.stdoutR }

// Writer returns the child's stdin. Close sends the graceful-shutdown
// signal (close server stdin); it does not reap the namespace, use Close
// for full teardown.
func (p *MCPStdioProcess) Writer() io.WriteCloser { return &mcpStdinWriter{p: p} }

// mcpStdinWriter chunks stdin bytes into channel frames.
type mcpStdinWriter struct {
	p      *MCPStdioProcess
	mu     sync.Mutex
	closed bool
}

func (w *mcpStdinWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errMCPStdioClosed
	}
	total := len(data)
	for len(data) > 0 {
		n := min(len(data), mcpStdioChunk)
		if err := w.p.send.send(mcpFrame{Type: "stdin", Data: data[:n]}); err != nil {
			return total - len(data), err
		}
		data = data[n:]
	}
	return total, nil
}

func (w *mcpStdinWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.p.send.send(mcpFrame{Type: "close-stdin"})
}

// Diagnostic returns the bounded stderr text with credential-shaped values
// redacted. Stderr is diagnostic, never protocol.
func (p *MCPStdioProcess) Diagnostic(policy *redaction.Policy) string {
	return policy.Redact(p.stderr.String())
}

// Exited reports the server exit code once the exit frame arrived.
func (p *MCPStdioProcess) Exited() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.exited
}

// Close performs graceful shutdown: close server stdin, wait a bounded
// period for voluntary exit, then terminate the namespace and reap it. It
// is idempotent and safe to call concurrently with the pump.
func (p *MCPStdioProcess) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		_ = p.send.send(mcpFrame{Type: "close-stdin"})
		select {
		case <-p.done:
		case <-time.After(mcpStdioGrace):
			p.worker.stop()
		}
		werr := p.worker.wait()
		_ = p.input.Close()
		_ = p.output.Close()
		p.stdoutW.CloseWithError(errMCPStdioClosed)
		<-p.done
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case p.protoErr != nil:
			p.closeErr = p.protoErr
		case !p.exited && werr != nil && !isContextError(werr):
			p.closeErr = FatalError{werr}
		}
	})
	return p.closeErr
}

func isContextError(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
