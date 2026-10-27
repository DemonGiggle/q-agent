package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"q-agent/internal/logging"
	"q-agent/internal/state"
	"q-agent/internal/update"
)

// serveArgv returns the argv used to re-exec foreground serve. Production
// falls back to os.Args when the controller was not given an explicit copy.
func (c *Controller) serveArgv() ([]string, error) {
	if len(c.ServeArgv) != 0 {
		out := append([]string(nil), c.ServeArgv...)
		return out, nil
	}
	args := append([]string(nil), os.Args[1:]...)
	if len(args) > 0 && args[0] == "serve" {
		return args, nil
	}
	return nil, fmt.Errorf("detached maintenance requires ServeArgv (e.g. serve --config <path>); running binary was not started as serve")
}

func (c *Controller) spawn(ctx context.Context, executable string, argv []string, logPath string) (int, error) {
	if c.SpawnDetached != nil {
		return c.SpawnDetached(ctx, executable, argv, logPath)
	}
	return defaultSpawnDetached(executable, argv, logPath)
}

func defaultSpawnDetached(executable string, argv []string, logPath string) (int, error) {
	if err := (&logging.Writer{Path: logPath}).Prepare(); err != nil {
		return 0, fmt.Errorf("prepare detached log: %w", err)
	}
	// The supervisor owns the log pipe for the child's entire lifetime and
	// rotates it even when the detached bot runs across midnight.
	args := append([]string{"service", "log-run", "--log-path", logPath, "--", executable}, argv...)
	cmd := exec.Command(executable, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	cmd.Dir = "/"
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
		_ = cmd.Process.Release()
	}
	if pid <= 0 {
		return 0, fmt.Errorf("detached spawn did not return a process")
	}
	return pid, nil
}

func (c *Controller) signalOld(pid int, sig syscall.Signal) error {
	if c.Signal != nil {
		return c.Signal(pid, sig)
	}
	return syscall.Kill(pid, sig)
}

func (c *Controller) isAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if c.Alive != nil {
		return c.Alive(pid)
	}
	return syscall.Kill(pid, 0) == nil
}

// launchDetachedHelper is split for readability; Request inlines the same
// save/notify/arm sequence as the systemd backend so both stay auditable.
func (c *Controller) launchDetachedHelper(ctx context.Context, executable, stateDir, id string) (int, error) {
	argv := []string{"service", "maintain", "--state-dir", stateDir, "--id", id}
	return c.spawn(ctx, executable, argv, c.path("helper.log"))
}

// runDetached stops the foreground bot via SIGTERM, optionally updates the
// executable, then starts a new detached serve and waits for readiness. It
// runs in a Setsid helper so the old bot's exit cannot kill the updater.
func (c *Controller) runDetached(ctx context.Context, r request) error {
	backup := ""
	var err error
	if r.Action == "update" {
		backup, err = copyExecutable(r.Executable)
		if err != nil {
			return c.finish(r, "Update cancelled: could not preserve the installed executable.")
		}
		r.Backup = backup
		if err := c.save(r); err != nil {
			_ = os.Remove(backup)
			return err
		}
	}
	// Ask the old foreground process to exit gracefully. signal.NotifyContext
	// in serve converts SIGTERM into polling shutdown and serve.lock release.
	if c.isAlive(r.OldPID) {
		_ = c.signalOld(r.OldPID, syscall.SIGTERM)
		if err := c.waitForOldExit(ctx, r.OldPID, 30*time.Second); err != nil {
			// Escalate once; a stuck old bot must not block the new one forever.
			_ = c.signalOld(r.OldPID, syscall.SIGKILL)
			if err := c.waitForOldExit(ctx, r.OldPID, 5*time.Second); err != nil {
				return c.recoverDetached(ctx, r, "Maintenance failed while stopping the foreground process.", backup)
			}
		}
	}
	updateMessage := ""
	if r.Action == "update" {
		var output strings.Builder
		opts := update.Options{ExecutablePath: r.Executable, CurrentVersion: r.OldVersion,
			Stdout: &output, ValidateExecutable: true}
		runUpdate := c.Update
		if runUpdate == nil {
			runUpdate = func(ctx context.Context, o update.Options) error { return update.Run(ctx, nil, o) }
		}
		updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := runUpdate(updateCtx, opts)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "maintenance update:", err)
			return c.recoverDetached(ctx, r, "Update failed.", backup)
		}
		if strings.Contains(output.String(), "already up to date") {
			updateMessage = "Already up to date. "
		}
		if strings.Contains(output.String(), "newer than the latest release") {
			updateMessage = "Installed version is newer than the latest Release; no downgrade. "
		}
	}
	started, err := c.startDetached(ctx, &r, "starting")
	if err != nil {
		fmt.Fprintln(os.Stderr, "maintenance startup:", err)
		return c.recoverDetached(ctx, r, "The process did not become ready after maintenance.", backup)
	}
	if backup != "" {
		_ = os.Remove(backup)
		r.Backup = ""
	}
	label := "Restart"
	if r.Action == "update" {
		label = "Update"
	}
	return c.finish(r, fmt.Sprintf("%s%s completed. Running q-agent %s (previously %s).", updateMessage, label, started.Version, r.OldVersion))
}

func (c *Controller) waitForOldExit(ctx context.Context, pid int, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if !c.isAlive(pid) {
			return nil
		}
		if err := pause(deadline, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

func (c *Controller) startDetached(ctx context.Context, r *request, phase string) (boot, error) {
	r.Phase = phase
	if err := c.save(*r); err != nil {
		return boot{}, err
	}
	if err := os.Remove(c.path("boot.json")); err != nil && !os.IsNotExist(err) {
		return boot{}, err
	}
	argv := append([]string(nil), r.ServeArgv...)
	if len(argv) == 0 {
		var err error
		argv, err = c.serveArgv()
		if err != nil {
			return boot{}, err
		}
	}
	if _, err := c.spawn(ctx, r.Executable, argv, c.path("serve.log")); err != nil {
		return boot{}, fmt.Errorf("start detached serve: %w", err)
	}
	timeout := c.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var b boot
		data, err := os.ReadFile(c.path("boot.json"))
		if err == nil && json.Unmarshal(data, &b) == nil && b.ID == r.ID && b.PID != r.OldPID && b.Version != "" {
			if c.isAlive(b.PID) {
				return b, nil
			}
		}
		if err := pause(readyCtx, 100*time.Millisecond); err != nil {
			return boot{}, err
		}
	}
}

func (c *Controller) recoverDetached(ctx context.Context, r request, reason, backup string) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Second)
	defer cancel()
	if backup != "" {
		if c.isAlive(r.OldPID) {
			_ = c.signalOld(r.OldPID, syscall.SIGTERM)
			_ = c.waitForOldExit(recoveryCtx, r.OldPID, 10*time.Second)
		}
		if err := os.Rename(backup, r.Executable); err != nil {
			return c.finish(r, reason+" Could not restore the previous executable; manual recovery is required.")
		}
		_ = state.SyncDir(filepath.Dir(r.Executable))
		r.Backup = ""
		reason += " Previous executable restored."
	}
	b, err := c.startDetached(recoveryCtx, &r, "recovering")
	if err != nil {
		return c.finish(r, reason+" Automatic startup recovery failed; inspect the maintenance serve.log.")
	}
	return c.finish(r, reason+" Service recovered and is running q-agent "+b.Version+".")
}
