package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
	"q-agent/internal/update"
)

type harness struct {
	c                                                                    *Controller
	commands                                                             []string
	pid                                                                  int
	starts                                                               int
	notified                                                             bool
	failLaunch, failNotice, failUpdate, failBoot, failRecovery, failStop bool
}

func fixture(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "q-agent")
	if err := os.WriteFile(exe, []byte("old executable"), 0751); err != nil {
		t.Fatal(err)
	}
	h := &harness{pid: os.Getpid()}
	h.c = &Controller{StateDir: filepath.Join(dir, "state"), Executable: exe, Version: "v1.0.0", ReadyTimeout: 10 * time.Millisecond}
	h.c.Notify = func(_ context.Context, _ platform.Message, _ string) error {
		h.notified = true
		if h.failNotice {
			return errors.New("offline")
		}
		return nil
	}
	h.c.Command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		h.commands = append(h.commands, command)
		if name == "systemd-run" {
			r, err := h.c.load()
			if err != nil || r.Phase != "waiting" || h.notified {
				t.Fatalf("launch must precede notification and arming: %+v %v", r, err)
			}
			if !strings.Contains(command, "ExecStopPost=") || strings.Contains(command, "--scope") {
				t.Fatal(command)
			}
			if h.failLaunch {
				return nil, errors.New("manager unavailable")
			}
			return nil, nil
		}
		if strings.Contains(command, "--property=MainPID") {
			return []byte(strconv.Itoa(h.pid)), nil
		}
		if strings.Contains(command, "--property=ActiveState") {
			return []byte("active"), nil
		}
		if len(args) < 2 {
			t.Fatal(command)
		}
		switch args[1] {
		case "stop":
			if !h.notified {
				t.Fatal("stopped before notification")
			}
			h.pid = 0
			if h.failStop {
				h.failStop = false
				return nil, errors.New("stop timed out")
			}
		case "start":
			h.starts++
			h.pid = os.Getpid() + h.starts + 1
			if h.failRecovery || h.failBoot && h.starts == 1 {
				return nil, nil
			}
			r, err := h.c.load()
			if err != nil {
				t.Fatal(err)
			}
			version := "v1.0.0"
			data, _ := os.ReadFile(h.c.Executable)
			if string(data) == "new executable" {
				version = "v1.1.0"
			}
			if err := state.Atomic(h.c.path("boot.json"), boot{ID: r.ID, PID: h.pid, Version: version}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal(command)
		}
		return nil, nil
	}
	h.c.Update = func(_ context.Context, opts update.Options) error {
		if h.pid != 0 || !opts.ValidateExecutable {
			t.Fatal("update ran with host active or validation disabled")
		}
		if err := os.WriteFile(opts.ExecutablePath, []byte("new executable"), 0751); err != nil {
			return err
		}
		if h.failUpdate {
			return errors.New("simulated interrupted update")
		}
		return nil
	}
	return h
}

func route() platform.Message {
	return platform.Message{Platform: "telegram", UserID: 1, ChatID: 1, UpdateID: 42, Text: "/update"}
}

func TestLifecycleOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, action, result, executable             string
		failUpdate, failBoot, failRecovery, failStop bool
	}{
		{name: "restart", action: "restart", result: "Restart completed", executable: "old executable"},
		{name: "update", action: "update", result: "Running q-agent v1.1.0", executable: "new executable"},
		{name: "download failure restores original", action: "update", failUpdate: true, result: "Previous executable restored", executable: "old executable"},
		{name: "new startup failure rolls back", action: "update", failBoot: true, result: "Service recovered", executable: "old executable"},
		{name: "restart startup failure retries", action: "restart", failBoot: true, result: "Service recovered", executable: "old executable"},
		{name: "stop timeout recovers", action: "update", failStop: true, result: "Service recovered", executable: "old executable"},
		{name: "failed recovery is not success", action: "update", failBoot: true, failRecovery: true, result: "Automatic startup recovery failed", executable: "old executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fixture(t)
			h.failUpdate, h.failBoot, h.failRecovery, h.failStop = tc.failUpdate, tc.failBoot, tc.failRecovery, tc.failStop
			if err := h.c.Request(context.Background(), tc.action, route()); err != nil {
				t.Fatal(err)
			}
			r, err := h.c.load()
			if err != nil || r.Phase != "armed" || !h.c.Busy() || r.Route.Text != "" {
				t.Fatalf("%+v %v", r, err)
			}
			if err := h.c.Request(context.Background(), tc.action, route()); err == nil {
				t.Fatal("duplicate accepted")
			}
			if err := h.c.Run(context.Background(), r.ID); err != nil {
				t.Fatal(err)
			}
			r, _ = h.c.load()
			if r.Phase != "done" || h.c.Busy() || !strings.Contains(r.Result, tc.result) {
				t.Fatalf("%+v", r)
			}
			data, _ := os.ReadFile(h.c.Executable)
			info, _ := os.Stat(h.c.Executable)
			if string(data) != tc.executable || info.Mode().Perm() != 0751 {
				t.Fatalf("installed %q mode %o", data, info.Mode().Perm())
			}
			if err := h.c.Run(context.Background(), r.ID); err == nil {
				t.Fatal("request replayed")
			}
			store, err := state.Open(h.c.StateDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.c.Report(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			reply, ok := store.Pending()
			if !ok || !strings.Contains(reply.Parts[0], tc.result) || reply.Route.ChatID != 1 {
				t.Fatalf("%+v", reply)
			}
			if err := store.Delivered(reply.ID); err != nil {
				t.Fatal(err)
			}
			// A fresh controller/store simulates another boot after result delivery.
			restarted := &Controller{StateDir: h.c.StateDir}
			store, _ = state.Open(h.c.StateDir, nil)
			if err := restarted.Report(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			if _, ok := store.Pending(); ok {
				t.Fatal("reported result replayed")
			}
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(h.c.Executable), ".q-agent-backup-*"))
			if len(matches) != 0 {
				t.Fatalf("backup leaked: %v", matches)
			}
		})
	}
}

func TestRequestPreconditionsAndFailedNoticeDoNotStop(t *testing.T) {
	for _, name := range []string{"not managed", "launcher failure", "notification failure", "separate worker", "invalid action"} {
		t.Run(name, func(t *testing.T) {
			h := fixture(t)
			action := "update"
			switch name {
			case "not managed":
				h.pid = -1
			case "launcher failure":
				h.failLaunch = true
			case "notification failure":
				h.failNotice = true
			case "separate worker":
				h.c.Worker = "/missing/worker"
			case "invalid action":
				action = "delete"
			}
			if err := h.c.Request(context.Background(), action, route()); err == nil {
				t.Fatal("expected rejection")
			}
			for _, command := range h.commands {
				if strings.Contains(command, "systemctl --user stop ") {
					t.Fatal("service stopped")
				}
			}
			if r, err := h.c.load(); err == nil {
				if r.Phase != "done" || !r.Reported {
					t.Fatalf("request blocked after rejected request: %+v", r)
				}
				if err := h.c.Run(context.Background(), r.ID); err == nil {
					t.Fatal("unarmed helper ran")
				}
			}
		})
	}
}

func TestInterruptedHelperReleasesGateAndReports(t *testing.T) {
	h := fixture(t)
	if err := h.c.Request(context.Background(), "restart", route()); err != nil {
		t.Fatal(err)
	}
	h.c.Command = func(context.Context, string, ...string) ([]byte, error) { return []byte("inactive"), nil }
	store, _ := state.Open(h.c.StateDir, nil)
	if err := h.c.Report(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if h.c.Busy() {
		t.Fatal("dead helper blocks forever")
	}
	reply, ok := store.Pending()
	if !ok || !strings.Contains(reply.Parts[0], "interrupted") {
		t.Fatalf("%+v", reply)
	}
}

func TestReadinessRequiresStartupAndMatchingPID(t *testing.T) {
	h := fixture(t)
	if err := h.c.MarkReady(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.c.path("boot.json")); !os.IsNotExist(err) {
		t.Fatal("ordinary start created readiness marker")
	}
	if err := h.c.Request(context.Background(), "restart", route()); err != nil {
		t.Fatal(err)
	}
	r, _ := h.c.load()
	r.Phase = "starting"
	if err := h.c.save(r); err != nil {
		t.Fatal(err)
	}
	if err := h.c.MarkReady(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(h.c.path("boot.json"))
	if !strings.Contains(string(data), fmt.Sprintf(`"PID":%d`, os.Getpid())) {
		t.Fatalf("%s", data)
	}
}
