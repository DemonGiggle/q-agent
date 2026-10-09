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
func run(ctx context.Context, o Options, mounts []string, input any) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	args := append(baseArgs(), mounts...)
	cmd := exec.CommandContext(ctx, o.Bubblewrap, args...)
	cmd.ExtraFiles = []*os.File{blockRead, infoWrite}
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := localtools.Buffer{Max: 128 << 10}
	var stderr localtools.Buffer
	cmd.Stdout = &stdout
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
	abort := func(e error) ([]byte, error) {
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
	err = <-done
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox failed: %w: %s", err, stderr.String())
	}
	return []byte(stdout.String()), nil
}
func execute(ctx context.Context, o Options, rootfs string, call llm.ToolCall) (string, error) {
	mounts := []string{"--bind", rootfs, "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chmod", "1777", "/tmp", "--tmpfs", "/run", "--ro-bind", o.Worker, workerPath, "--chdir", "/workspace", workerPath, "worker", "tool"}
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
