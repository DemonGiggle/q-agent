// Stdio transport for MCP servers (issue #47): launch configured
// executables with explicit argument arrays in the chat working directory,
// either as direct host children or inside the existing sandbox namespace,
// and expose independent stdin/stdout/stderr streams to the SDK.
//
// The existing combined-output launcher path cannot carry MCP traffic: the
// protocol requires stdout to carry newline-delimited JSON-RPC only, with
// stderr reserved for diagnostics. Both transports below keep those streams
// separate end to end.
//
// Isolation and credential policy:
//   - Host mode spawns argv literally (no shell) with Dir set to the chat
//     working directory. Sandbox mode spawns inside the chat rootfs with
//     Dir /workspace (see internal/sandbox StartMCPStdio).
//   - The child inherits only a credential-filtered environment via
//     redaction.FilterEnvironment; no env-secret injection occurs.
//   - A sandbox setup or boundary failure never falls back to host
//     execution: it returns a FatalError failure.
//   - Protocol frames are bounded (MaxStdioFrameBytes) and stderr
//     diagnostics are bounded (maxStdioStderrBytes) and redacted before
//     they reach errors or status records.
//
// Lifecycle: Connect closes over a spawned child; Close closes stdin for
// graceful shutdown, waits a bounded period, then terminates and reaps the
// child. Close is idempotent. Cancellation before or during startup kills
// the child instead of leaking it.
package mcpclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/config"
	"q-agent/internal/redaction"
	"q-agent/internal/sandbox"
)

const (
	// MaxStdioFrameBytes bounds one inbound JSON-RPC frame on stdio
	// connections. Servers emitting larger lines fail the connection
	// instead of exhausting memory.
	MaxStdioFrameBytes = 4 << 20
	// maxStdioStderrBytes bounds captured stderr diagnostics per server,
	// keeping the tail (final errors are usually the most relevant).
	maxStdioStderrBytes = 64 << 10
	// stdioShutdownGrace bounds voluntary server exit after stdin close
	// before SIGTERM.
	stdioShutdownGrace = 5 * time.Second
	// stdioTerminateGrace bounds voluntary server exit after SIGTERM
	// before SIGKILL.
	stdioTerminateGrace = 5 * time.Second
	// maxStdioArgs caps the argument count; maxStdioArgBytes caps one
	// argument; maxStdioArgsTotal caps their sum.
	maxStdioArgs      = 128
	maxStdioArgBytes  = 32 << 10
	maxStdioArgsTotal = 256 << 10
)

// checkStdioServer validates one server entry for stdio dialing without
// echoing host details. Load-time validation already enforces this shape;
// the checks keep direct struct literals usable and failures definitive.
func checkStdioServer(server config.MCPServer) error {
	if server.Transport != "stdio" {
		return fmt.Errorf("mcp: stdio transport cannot dial %q transport", server.Transport)
	}
	if strings.TrimSpace(server.Command) == "" {
		return fmt.Errorf("mcp: stdio server command is required")
	}
	if server.URL != "" {
		return fmt.Errorf("mcp: stdio server must not set url")
	}
	if len(server.Args) > maxStdioArgs {
		return fmt.Errorf("mcp: stdio server args exceed limit")
	}
	total := 0
	for _, arg := range server.Args {
		if len(arg) > maxStdioArgBytes {
			return fmt.Errorf("mcp: stdio server arg exceeds limit")
		}
		total += len(arg)
	}
	if total > maxStdioArgsTotal {
		return fmt.Errorf("mcp: stdio server args exceed limit")
	}
	return nil
}

// filteredEnv returns the child environment: the configured base (or the
// current process environment when nil) with credential entries removed. No
// secret values are ever added.
func filteredEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	safe, _ := redaction.FilterEnvironment(base)
	return append([]string(nil), safe...)
}

// stderrCapture keeps the tail of stderr diagnostics under a bound.
type stderrCapture struct {
	mu   sync.Mutex
	data []byte
}

func (b *stderrCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > maxStdioStderrBytes {
		b.data = append([]byte(nil), b.data[len(b.data)-maxStdioStderrBytes:]...)
	}
	return len(p), nil
}

func (b *stderrCapture) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// diagnostic renders a redacted, single-line, bounded diagnostic from a
// spawn/connect failure plus captured stderr. It never carries raw stderr:
// credential-shaped values are masked even when a server echoes them.
func diagnostic(policy *redaction.Policy, stderr *stderrCapture, err error) string {
	msg := ""
	if err != nil {
		msg = strings.TrimSpace(err.Error())
	}
	if stderr != nil {
		if redacted := strings.TrimSpace(policy.Redact(stderr.String())); redacted != "" {
			if msg != "" {
				msg += " "
			}
			msg += "stderr: " + redacted
		}
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	const maxDiagnosticLen = 500
	if len(msg) > maxDiagnosticLen {
		msg = strings.TrimSpace(msg[:maxDiagnosticLen]) + "…"
	}
	if msg == "" {
		return "mcp: unknown stdio error"
	}
	return msg
}

// HostStdioFactory returns a TransportFactory that launches each stdio
// server as a direct host child: argv used literally (no shell), working
// directory fixed to the chat root, environment credential-filtered.
// environment nil inherits the current process environment (still
// filtered); policy nil uses default redaction patterns.
func HostStdioFactory(workingDir string, environment []string, policy *redaction.Policy) TransportFactory {
	return func(ctx context.Context, name string, server config.MCPServer) (SDKTransport, error) {
		if err := checkStdioServer(server); err != nil {
			return nil, err
		}
		if strings.TrimSpace(workingDir) == "" {
			return nil, fmt.Errorf("mcp: stdio working directory is required")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &hostStdioTransport{
			command: server.Command,
			args:    append([]string(nil), server.Args...),
			dir:     workingDir,
			env:     filteredEnv(environment),
			policy:  policy,
		}, nil
	}
}

// hostStdioTransport spawns the server on Connect so cancellation during
// setup never leaves a child behind.
type hostStdioTransport struct {
	command string
	args    []string
	dir     string
	env     []string
	policy  *redaction.Policy
}

// Connect implements the SDK Transport interface.
func (t *hostStdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(t.command, t.args...)
	cmd.Dir = t.dir
	cmd.Env = append([]string(nil), t.env...)
	stderr := &stderrCapture{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdio server: %s", diagnostic(t.policy, nil, err))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp: stdio server: %s", diagnostic(t.policy, nil, err))
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: stdio server: %s", diagnostic(t.policy, stderr, err))
	}
	// A parent that cancelled during Start must not leak the child.
	if ctx.Err() != nil {
		_ = stdin.Close()
		terminateChild(cmd)
		_ = cmd.Wait()
		_ = stdout.Close()
		return nil, ctx.Err()
	}
	inner, err := (&mcp.IOTransport{
		Reader:        stdout,
		Writer:        stdin,
		MaxLineLength: MaxStdioFrameBytes,
	}).Connect(ctx)
	if err != nil {
		_ = stdin.Close()
		terminateChild(cmd)
		_ = cmd.Wait()
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp: stdio server: %s", diagnostic(t.policy, stderr, err))
	}
	return &hostStdioConn{Connection: inner, cmd: cmd, stderr: stderr, policy: t.policy}, nil
}

// hostStdioConn wraps the SDK framing connection with child teardown:
// Close closes stdin for graceful shutdown, waits a bounded period, then
// terminates and reaps. It is idempotent and safe for concurrent use.
type hostStdioConn struct {
	mcp.Connection
	cmd      *exec.Cmd
	stderr   *stderrCapture
	policy   *redaction.Policy
	once     sync.Once
	closeErr error
}

// Diagnostic returns the redacted, bounded stderr text captured so far.
// Stderr is diagnostic, never protocol.
func (c *hostStdioConn) Diagnostic() string {
	if c.stderr == nil {
		return ""
	}
	return c.policy.Redact(c.stderr.String())
}

// Close implements the SDK Connection interface.
func (c *hostStdioConn) Close() error {
	c.once.Do(func() {
		// Closing stdin asks a well-behaved server to exit; the waits
		// below bound a misbehaving one.
		cerr := c.Connection.Close()
		waited := make(chan error, 1)
		go func() { waited <- c.cmd.Wait() }()
		timer := time.NewTimer(stdioShutdownGrace)
		defer timer.Stop()
		select {
		case <-waited:
			// A child's exit after stdin close (whatever the status)
			// is ordinary teardown; surface only connection errors.
			c.closeErr = cerr
			return
		case <-timer.C:
		}
		terminateChild(c.cmd)
		timer.Reset(stdioTerminateGrace)
		select {
		case <-waited:
			c.closeErr = cerr
		case <-timer.C:
			killChild(c.cmd)
			c.closeErr = cerr
		}
	})
	return c.closeErr
}

// terminateChild sends SIGTERM to the server child.
func terminateChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

// killChild force-kills the server child.
func killChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// SandboxStdioFactory returns a TransportFactory that launches each stdio
// server inside the existing sandbox for rootfs: argv used literally (no
// shell), working directory /workspace, environment credential-filtered.
// Setup and boundary failures return FatalError failures and never fall
// back to host execution.
func SandboxStdioFactory(options sandbox.Options, rootfs string, policy *redaction.Policy) TransportFactory {
	return func(ctx context.Context, name string, server config.MCPServer) (SDKTransport, error) {
		if err := checkStdioServer(server); err != nil {
			return nil, err
		}
		if strings.TrimSpace(rootfs) == "" {
			return nil, fmt.Errorf("mcp: stdio sandbox rootfs is required")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &sandboxStdioTransport{
			options: options,
			rootfs:  rootfs,
			command: server.Command,
			args:    append([]string(nil), server.Args...),
			policy:  policy,
		}, nil
	}
}

// sandboxStdioTransport dials the sandbox worker channel on Connect so
// cancellation during setup never leaves a namespace behind.
type sandboxStdioTransport struct {
	options sandbox.Options
	rootfs  string
	command string
	args    []string
	policy  *redaction.Policy
}

// Connect implements the SDK Transport interface.
func (t *sandboxStdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proc, err := sandbox.StartMCPStdio(ctx, t.options, t.rootfs, t.command, t.args)
	if err != nil {
		return nil, fmt.Errorf("mcp: stdio server: %s", diagnostic(t.policy, nil, err))
	}
	// A parent that cancelled during startup must not leak the namespace.
	if ctx.Err() != nil {
		_ = proc.Close()
		return nil, ctx.Err()
	}
	inner, err := (&mcp.IOTransport{
		Reader:        proc.Reader(),
		Writer:        proc.Writer(),
		MaxLineLength: MaxStdioFrameBytes,
	}).Connect(ctx)
	if err != nil {
		diag := proc.Diagnostic(t.policy)
		_ = proc.Close()
		if diag != "" {
			return nil, fmt.Errorf("mcp: stdio server: %s", diagnostic(t.policy, nil, fmt.Errorf("%v stderr: %s", err, diag)))
		}
		return nil, err
	}
	return &sandboxStdioConn{Connection: inner, proc: proc}, nil
}

// sandboxStdioConn wraps the SDK framing connection with namespace
// teardown. It is idempotent and safe for concurrent use.
type sandboxStdioConn struct {
	mcp.Connection
	proc     *sandbox.MCPStdioProcess
	once     sync.Once
	closeErr error
}

// Diagnostic returns the redacted, bounded stderr text captured so far.
func (c *sandboxStdioConn) Diagnostic(policy *redaction.Policy) string {
	if c.proc == nil {
		return ""
	}
	return c.proc.Diagnostic(policy)
}

// Close implements the SDK Connection interface.
func (c *sandboxStdioConn) Close() error {
	c.once.Do(func() {
		cerr := c.Connection.Close()
		if perr := c.proc.Close(); c.closeErr == nil {
			c.closeErr = perr
		}
		if cerr != nil && c.closeErr == nil {
			c.closeErr = cerr
		}
	})
	return c.closeErr
}

var (
	_ SDKTransport   = (*hostStdioTransport)(nil)
	_ SDKTransport   = (*sandboxStdioTransport)(nil)
	_ io.Closer      = (*hostStdioConn)(nil)
	_ io.Closer      = (*sandboxStdioConn)(nil)
	_ mcp.Connection = (*hostStdioConn)(nil)
	_ mcp.Connection = (*sandboxStdioConn)(nil)
)
