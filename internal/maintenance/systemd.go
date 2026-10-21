package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"q-agent/internal/state"
	"q-agent/internal/update"
)

// isSystemdManaged reports whether this process is MainPID of the user
// q-agent.service. Any error or PID mismatch means systemd backend is
// unavailable; the caller falls back to detached.
func (c *Controller) isSystemdManaged(ctx context.Context) bool {
	out, err := c.control(ctx, "show", serviceUnit, "--property=MainPID", "--value")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == strconv.Itoa(os.Getpid())
}

// runSystemd stops the service, optionally updates the executable, then starts
// it again and waits for readiness. It runs in a transient unit in a separate
// cgroup so stopping the bot cannot kill the helper.
func (c *Controller) runSystemd(ctx context.Context, r request) error {
	out, err := c.control(ctx, "show", serviceUnit, "--property=MainPID", "--value")
	if err != nil || strings.TrimSpace(string(out)) != strconv.Itoa(r.OldPID) {
		return c.finish(r, "Maintenance cancelled: the running service changed before the operation began.")
	}
	backup := ""
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
	if _, err := c.control(ctx, "stop", serviceUnit); err != nil {
		// A timed-out stop may still complete. Always request a start again.
		return c.recoverSystemd(ctx, r, "Maintenance failed while stopping the service.", backup)
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
			return c.recoverSystemd(ctx, r, "Update failed.", backup)
		}
		// Preserve only a fixed no-op classification, not arbitrary download text.
		if strings.Contains(output.String(), "already up to date") {
			updateMessage = "Already up to date. "
		}
		if strings.Contains(output.String(), "newer than the latest release") {
			updateMessage = "Installed version is newer than the latest Release; no downgrade. "
		}
	}
	started, err := c.startSystemd(ctx, &r, "starting")
	if err != nil {
		fmt.Fprintln(os.Stderr, "maintenance startup:", err)
		return c.recoverSystemd(ctx, r, "The service did not become ready after maintenance.", backup)
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

func (c *Controller) startSystemd(ctx context.Context, r *request, phase string) (boot, error) {
	r.Phase = phase
	if err := c.save(*r); err != nil {
		return boot{}, err
	}
	// Remove an earlier attempt's readiness marker before permitting a new boot.
	if err := os.Remove(c.path("boot.json")); err != nil && !os.IsNotExist(err) {
		return boot{}, err
	}
	if out, err := c.control(ctx, "start", serviceUnit); err != nil {
		return boot{}, fmt.Errorf("start service: %w: %s", err, strings.TrimSpace(string(out)))
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
			pid, err := c.control(readyCtx, "show", serviceUnit, "--property=MainPID", "--value")
			if err == nil && strings.TrimSpace(string(pid)) == strconv.Itoa(b.PID) {
				return b, nil
			}
		}
		if err := pause(readyCtx, 100*time.Millisecond); err != nil {
			return boot{}, err
		}
	}
}

func (c *Controller) recoverSystemd(ctx context.Context, r request, reason, backup string) error {
	// Recovery has its own budget even when the requested operation timed out.
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Second)
	defer cancel()
	if backup != "" {
		if _, err := c.control(recoveryCtx, "stop", serviceUnit); err != nil {
			return c.finish(r, reason+" Recovery could not stop the service; inspect the journal and installed executable.")
		}
		if err := os.Rename(backup, r.Executable); err != nil {
			return c.finish(r, reason+" Could not restore the previous executable; manual recovery is required.")
		}
		_ = state.SyncDir(filepath.Dir(r.Executable))
		r.Backup = ""
		reason += " Previous executable restored."
	}
	b, err := c.startSystemd(recoveryCtx, &r, "recovering")
	if err != nil {
		return c.finish(r, reason+" Automatic startup recovery failed; inspect journalctl --user -u q-agent.service.")
	}
	return c.finish(r, reason+" Service recovered and is running q-agent "+b.Version+".")
}
