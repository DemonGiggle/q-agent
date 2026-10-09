//go:build linux

package localtools

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func runShell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = "/workspace"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = 200 * time.Millisecond
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success() {
		err = nil
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.String(), err
}
