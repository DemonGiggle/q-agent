// Package maintenance runs administrator-requested lifecycle operations outside
// the bot's systemd cgroup. It never exposes host administration as model tools.
package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
	"q-agent/internal/update"
)

const serviceUnit = "q-agent.service"

type Controller struct {
	StateDir, Executable, Version, Worker string
	Notify                                func(context.Context, platform.Message, string) error
	// Command and Update are injectable to exercise failures without touching a bot.
	Command      func(context.Context, string, ...string) ([]byte, error)
	Update       func(context.Context, update.Options) error
	ReadyTimeout time.Duration
	mu           sync.Mutex
}

type request struct {
	ID, Action, Executable, OldVersion, HelperUnit, Phase, Result, Backup string
	Route                                                                 platform.Message
	OldPID                                                                int
	Reported                                                              bool
}

type boot struct {
	ID, Version string
	PID         int
}

func (c *Controller) path(name string) string { return filepath.Join(c.StateDir, "maintenance", name) }
func (c *Controller) load() (request, error) {
	var r request
	f, err := os.Open(c.path("request.json"))
	if err != nil {
		return r, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&r)
	return r, err
}
func (c *Controller) save(r request) error { return state.Atomic(c.path("request.json"), r) }
func (c *Controller) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if c.Command != nil {
		return c.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
func (c *Controller) control(ctx context.Context, args ...string) ([]byte, error) {
	return c.command(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

// Busy fails closed on unreadable state. Completed operations do not block work.
func (c *Controller) Busy() bool {
	r, err := c.load()
	return !os.IsNotExist(err) && (err != nil || r.Phase != "done")
}

// Request validates the actual MainPID, stages a durable request, dispatches a
// separate transient service, then arms it only after Telegram acknowledged the
// pre-stop notice. No shell, sudo, daemon-reload or unit edits are involved.
//
// When this process is not MainPID of the systemd user q-agent.service (for
// example a foreground `./bin/q-agent serve`), /restart is rejected with
// guidance, while /update performs the same direct download-and-replace as
// `q-agent update` and reports the result immediately without stopping the
// process. Foreground updates still require a manual restart to run the new
// binary.
func (c *Controller) Request(ctx context.Context, action string, route platform.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if action != "update" && action != "restart" {
		return errors.New("unsupported maintenance action")
	}
	if old, err := c.load(); err == nil {
		if old.Phase != "done" || !old.Reported {
			return errors.New("a maintenance operation or its result is still pending")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	out, err := c.control(ctx, "show", serviceUnit, "--property=MainPID", "--value")
	if err != nil || strings.TrimSpace(string(out)) != strconv.Itoa(os.Getpid()) {
		if action != "update" {
			return errors.New("restart requires this process to be MainPID of the systemd user q-agent.service; foreground serve: stop and restart this process manually")
		}
		return c.foregroundUpdate(ctx, route)
	}
	executable := c.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	if action == "update" && c.Worker != "" {
		worker, err := filepath.EvalSymlinks(c.Worker)
		if err != nil || worker != executable {
			return errors.New("/update requires sandbox.worker to use the service executable; update a separately configured worker manually")
		}
	}
	if c.Notify == nil {
		return errors.New("maintenance notification transport unavailable")
	}
	controlPath, err := exec.LookPath("systemctl")
	if err != nil {
		return err
	}
	unlock, err := state.Lock(c.path("lock"))
	if err != nil {
		return err
	}
	defer unlock()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	id := hex.EncodeToString(nonce)
	route.Text, route.CallbackData = "", ""
	r := request{ID: id, Action: action, Executable: executable, OldVersion: c.Version,
		OldPID: os.Getpid(), HelperUnit: "q-agent-maintenance-" + id + ".service", Phase: "waiting", Route: route}
	if err := c.save(r); err != nil {
		return err
	}
	fail := func(err error) error {
		r.Phase, r.Reported = "done", true
		if saveErr := c.save(r); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	}
	_, err = c.command(ctx, "systemd-run", "--user", "--quiet", "--collect", "--unit="+r.HelperUnit,
		"--property=Type=exec", "--property=RuntimeMaxSec=10min", "--property=UMask=0077",
		"--property=ExecStopPost="+strconv.Quote(controlPath)+" --user start "+serviceUnit,
		"--", executable, "service", "maintain", "--state-dir", c.StateDir, "--id", id)
	if err != nil {
		return fail(fmt.Errorf("could not launch independent systemd maintenance service: %w", err))
	}
	notice := "Restart requested. The service will stop briefly; I will report the running version after startup."
	if action == "update" {
		notice = "Update requested: latest stable GitHub Release (not main). The service will stop briefly; I will report the result after startup."
	}
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = c.Notify(noticeCtx, route, notice)
	cancel()
	if err != nil {
		return fail(errors.New("maintenance cancelled because the pre-stop notification could not be delivered"))
	}
	r.Phase = "armed"
	if err := c.save(r); err != nil {
		return fail(err)
	}
	return nil
}

// foregroundUpdate performs a direct `q-agent update` without stopping the
// process. It is used when the bot runs as a foreground `serve` process
// instead of MainPID of the systemd user service. The caller must hold c.mu
// and have already validated that no other maintenance operation is pending.
func (c *Controller) foregroundUpdate(ctx context.Context, route platform.Message) error {
	executable := c.Executable
	var err error
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	if c.Worker != "" {
		worker, err := filepath.EvalSymlinks(c.Worker)
		if err != nil || worker != executable {
			return errors.New("/update requires sandbox.worker to use the service executable; update a separately configured worker manually")
		}
	}
	if c.Notify == nil {
		return errors.New("maintenance notification transport unavailable")
	}
	unlock, err := state.Lock(c.path("lock"))
	if err != nil {
		return err
	}
	defer unlock()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	id := hex.EncodeToString(nonce)
	route.Text, route.CallbackData = "", ""
	r := request{ID: id, Action: "update", Executable: executable, OldVersion: c.Version,
		OldPID: os.Getpid(), Phase: "waiting", Route: route}
	if err := c.save(r); err != nil {
		return err
	}
	fail := func(err error) error {
		r.Phase, r.Reported = "done", true
		if saveErr := c.save(r); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	}
	var output strings.Builder
	opts := update.Options{ExecutablePath: executable, CurrentVersion: c.Version,
		Stdout: &output, ValidateExecutable: true}
	runUpdate := c.Update
	if runUpdate == nil {
		runUpdate = func(ctx context.Context, o update.Options) error { return update.Run(ctx, nil, o) }
	}
	updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = runUpdate(updateCtx, opts)
	cancel()
	if err != nil {
		return fail(err)
	}
	message := strings.TrimSpace(output.String())
	if message == "" {
		message = "Update completed."
	} else if !strings.Contains(message, "already up to date") && !strings.Contains(message, "newer than the latest release") {
		message += " Restart this process to use the new version."
	}
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	notifyErr := c.Notify(noticeCtx, route, message)
	cancel()
	r.Phase, r.Result, r.Reported = "done", message, true
	if err := c.save(r); err != nil {
		if notifyErr != nil {
			return errors.Join(notifyErr, err)
		}
		return err
	}
	if notifyErr != nil {
		return notifyErr
	}
	return nil
}

// MarkReady is called only after the bot's normal startup checks, Telegram
// identity lookup, store initialization and scheduler initialization succeed.
func (c *Controller) MarkReady() error {
	r, err := c.load()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Phase != "starting" && r.Phase != "recovering" {
		return nil
	}
	return state.Atomic(c.path("boot.json"), boot{ID: r.ID, Version: c.Version, PID: os.Getpid()})
}

// Report queues results through the existing durable/redacted outbox. Helpers
// never open or rewrite that outbox concurrently with the bot.
func (c *Controller) Report(ctx context.Context, store *state.Store) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.load()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Phase != "done" {
		out, showErr := c.control(ctx, "show", r.HelperUnit, "--property=ActiveState", "--value")
		if showErr != nil {
			return nil
		} // A bus outage is not proof the job failed.
		status := strings.TrimSpace(string(out))
		if status != "inactive" && status != "failed" {
			return nil
		}
		// Re-read after observing exit so a helper's final commit wins the race.
		r, err = c.load()
		if err != nil {
			return err
		}
		if r.Phase != "done" {
			r.Phase, r.Result = "done", "Maintenance was interrupted before completion. Check the maintenance service journal before retrying."
			if err := c.save(r); err != nil {
				return err
			}
		}
	}
	if r.Reported {
		return nil
	}
	if err := store.Queue(r.Route, []string{r.Result}); err != nil {
		return err
	}
	r.Reported = true
	return c.save(r)
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run is the internal, prompt-free maintenance helper entry point. A copied
// request cannot be replayed: nonce, phase and a cross-process lock are checked.
func (c *Controller) Run(ctx context.Context, id string) error {
	if len(id) != 24 {
		return errors.New("invalid maintenance request ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return err
	}
	var unlock func()
	var err error
	for attempts := 0; attempts < 150; attempts++ {
		unlock, err = state.Lock(c.path("lock"))
		if err == nil {
			break
		}
		if err := pause(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.load()
	if err != nil {
		return err
	}
	if r.ID != id || r.Phase != "armed" {
		return errors.New("maintenance request is not armed or has already run")
	}
	if r.Action != "update" && r.Action != "restart" {
		return errors.New("invalid maintenance action")
	}
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
		return c.recover(ctx, r, "Maintenance failed while stopping the service.", backup)
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
			return c.recover(ctx, r, "Update failed.", backup)
		}
		// Preserve only a fixed no-op classification, not arbitrary download text.
		if strings.Contains(output.String(), "already up to date") {
			updateMessage = "Already up to date. "
		}
		if strings.Contains(output.String(), "newer than the latest release") {
			updateMessage = "Installed version is newer than the latest Release; no downgrade. "
		}
	}
	started, err := c.start(ctx, &r, "starting")
	if err != nil {
		fmt.Fprintln(os.Stderr, "maintenance startup:", err)
		return c.recover(ctx, r, "The service did not become ready after maintenance.", backup)
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

func (c *Controller) finish(r request, message string) error {
	r.Phase, r.Result = "done", message
	return c.save(r)
}

func (c *Controller) start(ctx context.Context, r *request, phase string) (boot, error) {
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

func (c *Controller) recover(ctx context.Context, r request, reason, backup string) error {
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
	b, err := c.start(recoveryCtx, &r, "recovering")
	if err != nil {
		return c.finish(r, reason+" Automatic startup recovery failed; inspect journalctl --user -u q-agent.service.")
	}
	return c.finish(r, reason+" Service recovered and is running q-agent "+b.Version+".")
}

func copyExecutable(path string) (result string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 128<<20 {
		return "", errors.New("invalid installed executable")
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".q-agent-backup-*")
	if err != nil {
		return "", err
	}
	defer out.Close()
	defer func() {
		if err != nil {
			_ = os.Remove(out.Name())
		}
	}()
	count, err := io.Copy(out, io.LimitReader(f, (128<<20)+1))
	if err != nil {
		return "", err
	}
	if count > 128<<20 {
		return "", errors.New("installed executable exceeds backup size limit")
	}
	if err = out.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	if err = state.SyncDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	return out.Name(), nil
}
