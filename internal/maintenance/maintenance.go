// Package maintenance runs administrator-requested lifecycle operations outside
// the bot's own cgroup or foreground process. It never exposes host
// administration as model tools.
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
	"syscall"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
	"q-agent/internal/update"
)

const serviceUnit = "q-agent.service"

// Backends share request.json/boot.json. systemd uses a transient unit in a
// separate cgroup; detached re-execs serve from a Setsid helper for foreground
// processes without a user manager.
const (
	backendSystemd  = "systemd"
	backendDetached = "detached"
)

type Controller struct {
	StateDir, Executable, Version, Worker string
	Notify                                func(context.Context, platform.Message, string) error
	// Command and Update are injectable to exercise failures without touching a bot.
	Command      func(context.Context, string, ...string) ([]byte, error)
	Update       func(context.Context, update.Options) error
	ReadyTimeout time.Duration
	// ServeArgv re-execs the foreground bot (e.g. ["serve" "--config" path]).
	// Empty defaults to os.Args[1:] when it starts with serve.
	ServeArgv []string
	// SpawnDetached launches a Setsid helper or a new bot, redirecting
	// stdout/stderr to logPath. Injectable for tests.
	SpawnDetached func(ctx context.Context, executable string, argv []string, logPath string) (int, error)
	// Signal sends sig to pid; Alive reports kill(pid, 0) success. Injectable.
	Signal func(pid int, sig syscall.Signal) error
	Alive  func(pid int) bool
	mu     sync.Mutex
}

type request struct {
	ID, Action, Executable, OldVersion, HelperUnit, Phase, Result, Backup string
	Backend                                                               string
	HelperPID                                                             int
	ServeArgv                                                             []string
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

// Request validates the caller, stages a durable request, dispatches an
// independent helper (transient systemd unit or detached Setsid process),
// then arms it only after Telegram acknowledged the pre-stop notice.
// No shell, sudo, daemon-reload or unit edits are involved.
//
// When this process is not MainPID of the systemd user q-agent.service (for
// example a foreground `./bin/q-agent serve`), the detached backend SIGTERMs
// the old process and re-execs serve when ServeArgv is available. If no
// re-exec argv is available, /restart is rejected with guidance while /update
// falls back to the same direct download-and-replace as `q-agent update` and
// reports immediately without stopping; direct foreground updates still
// require a manual restart to run the new binary.
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
	managedEarly := c.isSystemdManaged(ctx)
	if !managedEarly {
		if _, serr := c.serveArgv(); serr != nil {
			if action != "update" {
				return errors.New("restart requires this process to be MainPID of the systemd user q-agent.service or a foreground serve with re-exec argv; foreground serve: stop and restart this process manually")
			}
			return c.foregroundUpdate(ctx, route)
		}
	}
	executable := c.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	executable, err := filepath.EvalSymlinks(executable)
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
		OldPID: os.Getpid(), Phase: "waiting", Route: route}
	managed := managedEarly
	if managed {
		controlPath := "systemctl"
		if c.Command == nil {
			var err error
			controlPath, err = exec.LookPath("systemctl")
			if err != nil {
				return err
			}
		}
		r.Backend = backendSystemd
		r.HelperUnit = "q-agent-maintenance-" + id + ".service"
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
	// Detached backend for foreground serve without a user manager.
	r.Backend = backendDetached
	argv, err := c.serveArgv()
	if err != nil {
		return err
	}
	r.ServeArgv = argv
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
	helperArgv := []string{"service", "maintain", "--state-dir", c.StateDir, "--id", id}
	pid, err := c.spawn(ctx, executable, helperArgv, c.path("helper-"+id+".log"))
	if err != nil {
		return fail(fmt.Errorf("could not launch independent detached maintenance helper: %w", err))
	}
	r.HelperPID = pid
	if err := c.save(r); err != nil {
		return fail(err)
	}
	notice := "Restart requested. The process will stop briefly; I will report the running version after startup."
	if action == "update" {
		notice = "Update requested: latest stable GitHub Release (not main). The process will stop briefly; I will report the result after startup."
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
		if r.Backend == backendDetached || (r.Backend == "" && r.HelperUnit == "" && r.HelperPID != 0) {
			if c.isAlive(r.HelperPID) {
				return nil
			}
			// Re-read after observing exit so a helper's final commit wins the race.
			r, err = c.load()
			if err != nil {
				return err
			}
			if r.Phase != "done" {
				r.Phase, r.Result = "done", "Maintenance was interrupted before completion. Check the maintenance helper log before retrying."
				if err := c.save(r); err != nil {
					return err
				}
			}
		} else {
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
	if r.Backend == backendDetached || (r.Backend == "" && r.HelperUnit == "" && r.HelperPID != 0) {
		return c.runDetached(ctx, r)
	}
	return c.runSystemd(ctx, r)
}

func (c *Controller) finish(r request, message string) error {
	r.Phase, r.Result = "done", message
	return c.save(r)
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
