//go:build linux

package sandbox

import (
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const workerPath = "/.q-agent-worker"

var caps = []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_SETUID", "CAP_SETGID", "CAP_SYS_CHROOT"}

func Resolve(o Options) (Options, error) {
	if os.Getuid() == 0 {
		return o, fmt.Errorf("run q-agent as a non-root service user with subordinate UID/GID allocations")
	}
	for _, p := range []*string{&o.Bubblewrap, &o.NewUIDMap, &o.NewGIDMap} {
		path, err := exec.LookPath(*p)
		if err != nil {
			return o, err
		}
		*p = path
	}
	var err error
	o.UIDBase, err = subordinate("/etc/subuid")
	if err != nil {
		return o, err
	}
	o.GIDBase, err = subordinate("/etc/subgid")
	if err != nil {
		return o, err
	}
	f, err := elf.Open(o.Worker)
	if err != nil {
		return o, fmt.Errorf("worker must be a static ELF binary: %w", err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return o, fmt.Errorf("worker must be statically linked (build with CGO_ENABLED=0)")
		}
	}
	return o, nil
}
func subordinate(path string) (int, error) {
	u, err := user.Current()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 3 || fields[0] != u.Username && fields[0] != u.Uid {
			continue
		}
		base, e := strconv.Atoi(fields[1])
		n, e2 := strconv.Atoi(fields[2])
		if e == nil && e2 == nil && n >= 65536 && base > 0 && base <= int(^uint32(0))-65536 {
			return base, nil
		}
	}
	return 0, fmt.Errorf("%s needs at least 65536 subordinate IDs for the service user", path)
}
func baseArgs() []string {
	args := []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup", "--die-with-parent", "--new-session", "--uid", "65536", "--gid", "65536", "--cap-drop", "ALL", "--clearenv", "--setenv", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "--setenv", "HOME", "/home/agent", "--setenv", "LANG", "C.UTF-8", "--setenv", "Q_AGENT_WORKER", "1", "--setenv", "DEBIAN_FRONTEND", "noninteractive", "--userns-block-fd", "3", "--info-fd", "4"}
	for _, c := range caps {
		args = append(args, "--cap-add", c)
	}
	return args
}

// run follows bubblewrap's userns-block-fd handshake. Never release the block
// unless both mapping helpers succeeded and the installed maps match exactly.
func startWorker(ctx context.Context, o Options, mounts []string, input io.Reader, output io.Writer) (*workerProcess, error) {
	ctx, cancel := context.WithCancel(ctx)
	ok := false
	defer func() {
		if !ok {
			cancel()
		}
	}()
	blockRead, blockWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer blockRead.Close()
	defer blockWrite.Close()
	infoRead, infoWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer infoRead.Close()
	defer infoWrite.Close()
	args := append(baseArgs(), mounts...)
	cmd := exec.CommandContext(ctx, o.Bubblewrap, args...)
	cmd.ExtraFiles = []*os.File{blockRead, infoWrite}
	cmd.Stdin = input
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr localtools.Buffer
	cmd.Stdout = output
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			return cmd.Process.Kill()
		}
		return nil
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	blockRead.Close()
	infoWrite.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	abort := func(e error) (*workerProcess, error) {
		cancel()
		_ = cmd.Process.Kill()
		<-done
		return nil, fmt.Errorf("sandbox setup: %w: %s", e, stderr.String())
	}
	type info struct {
		PID int `json:"child-pid"`
	}
	ready := make(chan struct {
		info info
		err  error
	}, 1)
	go func() {
		var i info
		e := json.NewDecoder(io.LimitReader(infoRead, 64<<10)).Decode(&i)
		ready <- struct {
			info info
			err  error
		}{i, e}
	}()
	var i info
	select {
	case value := <-ready:
		if value.err != nil {
			return abort(value.err)
		}
		i = value.info
	case <-ctx.Done():
		return abort(ctx.Err())
	}
	if i.PID <= 1 {
		return abort(fmt.Errorf("invalid namespace child PID"))
	}
	for _, mapping := range []struct {
		helper, kind string
		base, host   int
	}{{o.NewUIDMap, "uid", o.UIDBase, os.Getuid()}, {o.NewGIDMap, "gid", o.GIDBase, os.Getgid()}} {
		var diag localtools.Buffer
		helper := exec.CommandContext(ctx, mapping.helper, strconv.Itoa(i.PID), "0", strconv.Itoa(mapping.base), "65536", "65536", strconv.Itoa(mapping.host), "1")
		helper.Env = cmd.Env
		helper.Stdout = &diag
		helper.Stderr = &diag
		if err = helper.Run(); err != nil {
			return abort(fmt.Errorf("%s failed: %w: %s", mapping.kind, err, diag.String()))
		}
		data, e := os.ReadFile(fmt.Sprintf("/proc/%d/%s_map", i.PID, mapping.kind))
		if e != nil {
			return abort(e)
		}
		expected := []string{"0", strconv.Itoa(mapping.base), "65536", "65536", strconv.Itoa(mapping.host), "1"}
		if strings.Join(strings.Fields(string(data)), " ") != strings.Join(expected, " ") {
			return abort(fmt.Errorf("unexpected %s mapping", mapping.kind))
		}
	}
	if _, err = blockWrite.Write([]byte{1}); err != nil {
		return abort(err)
	}
	blockWrite.Close()
	ok = true
	return &workerProcess{cmd: cmd, cancel: cancel, done: done, stderr: &stderr, ctx: ctx}, nil
}

type workerProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan error
	stderr *localtools.Buffer
	ctx    context.Context
}

func (w *workerProcess) stop() { w.cancel() }
func (w *workerProcess) wait() error {
	err := <-w.done
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("sandbox failed: %w: %s", err, w.stderr.String())
	}
	return nil
}
func run(ctx context.Context, o Options, mounts []string, input any) ([]byte, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	stdout := localtools.Buffer{Max: 128 << 10}
	worker, err := startWorker(ctx, o, mounts, strings.NewReader(string(payload)), &stdout)
	if err != nil {
		return nil, err
	}
	defer worker.stop()
	if err = worker.wait(); err != nil {
		return nil, err
	}
	return []byte(stdout.String()), nil
}
func toolMounts(o Options, rootfs, mode string) []string {
	return []string{"--bind", rootfs, "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chmod", "1777", "/tmp", "--tmpfs", "/run", "--ro-bind", o.Worker, workerPath, "--chdir", "/workspace", workerPath, "worker", mode}
}
func execute(ctx context.Context, o Options, rootfs string, call llm.ToolCall) (string, error) {
	mounts := toolMounts(o, rootfs, "tool")
	data, err := run(ctx, o, mounts, localtools.Request{Call: call, ShellTimeout: o.ShellTimeout})
	if err != nil {
		return "", FatalError{err}
	}
	var response localtools.Response
	if err = json.Unmarshal(data, &response); err != nil {
		return "", FatalError{fmt.Errorf("invalid worker response: %w", err)}
	}
	if response.Error != "" {
		return response.Output, fmt.Errorf("%s", response.Error)
	}
	return response.Output, nil
}
func Provision(ctx context.Context, o Options, image, target string) error {
	mounts := []string{"--tmpfs", "/", "--bind", target, "/target", "--ro-bind", image, "/image.tar", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chmod", "1777", "/tmp", "--ro-bind", o.Worker, workerPath, "--chdir", "/", workerPath, "worker", "extract"}
	_, err := run(ctx, o, mounts, nil)
	return err
}
func Remove(ctx context.Context, o Options, target string) error {
	mounts := []string{"--tmpfs", "/", "--bind", target, "/target", "--proc", "/proc", "--ro-bind", o.Worker, workerPath, "--chdir", "/", workerPath, "worker", "remove"}
	_, err := run(ctx, o, mounts, nil)
	return err
}

// inboxPutMounts binds the chat rootfs as / plus the host staging file as
// /input.bin (read-only). The worker copies /input.bin into
// /workspace/inbox where directory write permission exists.
func inboxPutMounts(o Options, rootfs, staging string) []string {
	return []string{"--tmpfs", "/", "--bind", rootfs, "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chmod", "1777", "/tmp", "--tmpfs", "/run", "--ro-bind", o.Worker, workerPath, "--ro-bind", staging, "/input.bin", "--chdir", "/", workerPath, "worker", "inbox-put"}
}

func inboxClearMounts(o Options, rootfs string) []string {
	return []string{"--tmpfs", "/", "--bind", rootfs, "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chmod", "1777", "/tmp", "--tmpfs", "/run", "--ro-bind", o.Worker, workerPath, "--chdir", "/", workerPath, "worker", "inbox-clear"}
}

// PutInboxFile stages data host-side (0600, account-private) and delegates
// the inbox write to the namespace so subordinate-UID ownership never blocks
// the host. Quota, dedup, and containment are enforced inside.
func PutInboxFile(ctx context.Context, o Options, rootfs, name string, data []byte, maxBytes int64) (string, error) {
	if err := validateInboxName(name); err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty inbox input")
	}
	if int64(len(data)) > 20<<20 {
		return "", fmt.Errorf("inbox input exceeds 20 MB limit")
	}
	// Stage under the chat base (0700, host-owned) so the payload never
	// traverses the 1 MB JSON stdin path and never lands world-readable.
	stagingDir := filepath.Dir(rootfs)
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(stagingDir, ".inbox-put-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// The worker only needs read access; keep the staging file 0600.
	_ = os.Chmod(tmpName, 0600)
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := run(timeoutCtx, o, inboxPutMounts(o, rootfs, tmpName), inboxPutRequest{Name: name, MaxBytes: maxBytes})
	if err != nil {
		return "", err
	}
	var resp inboxPutResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("invalid inbox worker response: %w", err)
	}
	if err := validateInboxName(resp.Name); err != nil {
		return "", fmt.Errorf("invalid inbox worker response")
	}
	return resp.Name, nil
}

// ClearInbox removes inbox entries older than cutoff inside the namespace,
// where directory write permission exists for subordinate-UID-owned trees.
func ClearInbox(ctx context.Context, o Options, rootfs string, cutoff time.Time) (int, int64, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := run(timeoutCtx, o, inboxClearMounts(o, rootfs), inboxClearRequest{CutoffUnix: cutoff.Unix()})
	if err != nil {
		return 0, 0, err
	}
	var resp inboxClearResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, 0, fmt.Errorf("invalid inbox worker response: %w", err)
	}
	if resp.Removed < 0 || resp.Freed < 0 {
		return 0, 0, fmt.Errorf("invalid inbox worker response")
	}
	return resp.Removed, resp.Freed, nil
}
