//go:build linux

package process

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"q-agent/internal/redaction"
)

func HostLauncher(dir string, environment []string) Launcher {
	// Nil means inherit ordinary host variables, still filtering credentials.
	if environment == nil {
		environment = os.Environ()
	}
	environment, _ = redaction.FilterEnvironment(environment)
	return func(ctx context.Context, spec Spec, output io.Writer) (Child, error) {
		cmd := exec.Command("/bin/sh", "-c", spec.Command)
		cmd.Dir = dir
		cmd.Env = append([]string(nil), environment...)
		c := &hostChild{cmd: cmd, ctx: ctx, done: make(chan struct{}), outputDone: make(chan struct{})}
		if spec.PTY {
			cmd.Env = append(cmd.Env, "TERM=xterm-256color", fmt.Sprintf("COLUMNS=%d", spec.Columns), fmt.Sprintf("LINES=%d", spec.Rows))
			master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(spec.Rows), Cols: uint16(spec.Columns)})
			if err != nil {
				return nil, err
			}
			// Reopen a nonblocking descriptor through os.NewFile so Go's poller can
			// interrupt reads/writes on Close and apply input deadlines.
			fd, err := syscall.Dup(int(master.Fd()))
			if err != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				master.Close()
				_ = cmd.Wait()
				return nil, err
			}
			_ = syscall.SetNonblock(fd, true)
			c.terminal = os.NewFile(uintptr(fd), "session-pty")
			master.Close()
			c.input = c.terminal
			c.reader = c.terminal
		} else {
			inputR, inputW, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			outputR, outputW, err := os.Pipe()
			if err != nil {
				inputR.Close()
				inputW.Close()
				return nil, err
			}
			cmd.Stdin = inputR
			cmd.Stdout = outputW
			cmd.Stderr = outputW
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			err = cmd.Start()
			inputR.Close()
			outputW.Close()
			if err != nil {
				inputW.Close()
				outputR.Close()
				return nil, err
			}
			c.input = inputW
			c.reader = outputR
		}
		drain := &drainingReader{file: c.reader, remaining: HistoryLimit}
		go func() { defer close(c.outputDone); _, _ = io.Copy(output, drain) }()
		go func() {
			err := cmd.Wait()
			c.killGroup()
			drain.exited.Store(true)
			_ = c.reader.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			<-c.outputDone
			c.input.Close()
			c.reader.Close()
			c.exit = Exit{Code: cmd.ProcessState.ExitCode(), Err: err}
			close(c.done)
		}()
		go func() {
			select {
			case <-ctx.Done():
				c.Stop()
			case <-c.done:
			}
		}()
		return c, nil
	}
}

type hostChild struct {
	cmd        *exec.Cmd
	ctx        context.Context
	input      *os.File
	reader     *os.File
	terminal   *os.File
	inputMu    sync.Mutex
	stop       sync.Once
	killed     sync.Once
	outputDone chan struct{}
	done       chan struct{}
	exit       Exit
}

func (c *hostChild) Write(p []byte) (int, error) {
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	_ = c.input.SetWriteDeadline(time.Now().Add(time.Second))
	return c.input.Write(p)
}
func (c *hostChild) Resize(rows, cols int) error {
	raw, err := c.terminal.SyscallConn()
	if err != nil {
		return err
	}
	var resizeErr error
	err = raw.Control(func(fd uintptr) {
		resizeErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	})
	if err != nil {
		return err
	}
	return resizeErr
}
func (c *hostChild) killGroup() {
	c.killed.Do(func() {
		owned := c.cmd.Process.Pid
		_ = syscall.Kill(-owned, syscall.SIGKILL)
		// Interactive shells use distinct foreground/background job groups. They
		// still belong to our session; deliberately setsid-detached descendants do
		// not. Never use the service's own session as a cleanup boundary.
		entries, _ := os.ReadDir("/proc")
		groups := map[int]bool{}
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if err != nil {
				continue
			}
			end := strings.LastIndex(string(data), ") ")
			if end < 0 {
				continue
			}
			fields := strings.Fields(string(data[end+2:]))
			if len(fields) < 4 {
				continue
			}
			sid, err := strconv.Atoi(fields[3])
			if err != nil || sid != owned {
				continue
			}
			pgid, err := strconv.Atoi(fields[2])
			if err == nil && pgid > 1 {
				groups[pgid] = true
			}
		}
		for group := range groups {
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
	})
}
func (c *hostChild) Stop()      { c.stop.Do(func() { c.killGroup(); c.input.Close(); c.reader.Close() }) }
func (c *hostChild) Wait() Exit { <-c.done; return c.exit }

// After exit, drain buffered output even when terminal processing is slow.
// An idle or continuously-writing detached descendant cannot hold Wait open.
type drainingReader struct {
	file      *os.File
	exited    atomic.Bool
	remaining int
}

func (r *drainingReader) Read(p []byte) (int, error) {
	if r.exited.Load() {
		if r.remaining <= 0 {
			return 0, io.EOF
		}
		p = p[:min(len(p), r.remaining)]
		_ = r.file.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := r.file.Read(p)
		r.remaining -= n
		return n, err
	}
	return r.file.Read(p)
}
