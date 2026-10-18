//go:build linux

package localtools

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func runShell(ctx context.Context, command string, options Options) (string, error) {
	timeout := options.ShellTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	maxExtends := options.MaxShellExtends
	if maxExtends <= 0 {
		maxExtends = 2
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = options.WorkingDir
	cmd.Env = options.Environment
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = 200 * time.Millisecond
	kill := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Always reap the process-group so timed-out or finished shells cannot
	// leave children behind. Detached double-forked children may survive.
	defer kill()
	extends := 0
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			kill()
			<-done
			if ctx.Err() != nil {
				return output.String(), ctx.Err()
			}
			return output.String(), context.Canceled
		case err := <-done:
			if ctx.Err() != nil {
				return output.String(), ctx.Err()
			}
			if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success() {
				err = nil
			}
			return output.String(), err
		case <-timer.C:
			if ctx.Err() != nil {
				kill()
				<-done
				return output.String(), ctx.Err()
			}
			hasOutput := output.Len() > 0 && strings.TrimSpace(output.String()) != ""
			if !hasOutput || options.OnShellTimeout == nil || extends >= maxExtends {
				kill()
				<-done
				if ctx.Err() != nil {
					return output.String(), ctx.Err()
				}
				return output.String(), context.DeadlineExceeded
			}
			last := output.LastLines(3)
			extend := options.OnShellTimeout(ctx, command, last)
			// The process may have finished while asking.
			select {
			case err := <-done:
				if ctx.Err() != nil {
					return output.String(), ctx.Err()
				}
				if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success() {
					err = nil
				}
				return output.String(), err
			default:
			}
			if ctx.Err() != nil {
				kill()
				<-done
				return output.String(), ctx.Err()
			}
			if !extend {
				kill()
				<-done
				if ctx.Err() != nil {
					return output.String(), ctx.Err()
				}
				return output.String(), context.DeadlineExceeded
			}
			extends++
			timer.Reset(timeout)
		}
	}
}
