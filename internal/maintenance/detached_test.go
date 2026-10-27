package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
	"q-agent/internal/update"
)

type detachedHarness struct {
	harness
	helperPID  int
	oldAlive   bool
	helperLive bool
	newPID     int
	spawnCalls [][]string
}

func detachedFixture(t *testing.T) *detachedHarness {
	t.Helper()
	base := &harness{}
	// Reuse systemd fixture for executable/state but force detached path.
	// pid = -1 makes isSystemdManaged false.
	dir := t.TempDir()
	exe := filepath.Join(dir, "q-agent")
	if err := os.WriteFile(exe, []byte("old executable"), 0751); err != nil {
		t.Fatal(err)
	}
	h := &detachedHarness{
		helperPID:  4100000 + os.Getpid()%1000,
		oldAlive:   true,
		helperLive: true,
	}
	h.harness = harness{pid: -1}
	h.c = &Controller{
		StateDir:     filepath.Join(dir, "state"),
		Executable:   exe,
		Version:      "v1.0.0",
		ReadyTimeout: 500 * time.Millisecond,
		ServeArgv:    []string{"serve", "--config", filepath.Join(dir, "config.toml")},
	}
	_ = base
	h.c.Notify = func(_ context.Context, _ platform.Message, _ string) error {
		h.notified = true
		if h.failNotice {
			return errors.New("offline")
		}
		return nil
	}
	h.c.Command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		// Detached path must not touch systemctl except Report's systemd check,
		// which is bypassed for detached backends.
		return nil, errors.New("systemctl must not be used for detached backend: " + name)
	}
	h.c.SpawnDetached = func(_ context.Context, executable string, argv []string, logPath string) (int, error) {
		h.spawnCalls = append(h.spawnCalls, argv)
		if h.failLaunch {
			return 0, errors.New("spawn unavailable")
		}
		if len(argv) >= 2 && argv[0] == "service" && argv[1] == "maintain" {
			if !strings.HasSuffix(logPath, ".log") {
				t.Fatalf("helper log path: %s", logPath)
			}
			return h.helperPID, nil
		}
		if len(argv) > 0 && argv[0] == "serve" {
			h.starts++
			h.newPID = os.Getpid() + 100000 + h.starts
			if h.failRecovery || h.failBoot && h.starts == 1 {
				// Simulate new bot never becoming ready: no boot.json.
				return h.newPID, nil
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
			if err := state.Atomic(h.c.path("boot.json"), boot{ID: r.ID, PID: h.newPID, Version: version}); err != nil {
				t.Fatal(err)
			}
			return h.newPID, nil
		}
		t.Fatalf("unexpected spawn argv: %v", argv)
		return 0, nil
	}
	h.c.Signal = func(pid int, sig syscall.Signal) error {
		if pid == os.Getpid() {
			h.oldAlive = false
		}
		return nil
	}
	h.c.Alive = func(pid int) bool {
		if pid == os.Getpid() {
			return h.oldAlive
		}
		if pid == h.helperPID {
			return h.helperLive
		}
		if pid == h.newPID {
			return true
		}
		// boot.json PIDs for detached are newPID; anything else is dead.
		return false
	}
	h.c.Update = func(_ context.Context, opts update.Options) error {
		if h.oldAlive || !opts.ValidateExecutable {
			t.Fatal("update ran with foreground active or validation disabled")
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

func TestDetachedLifecycleOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, action, result, executable   string
		failUpdate, failBoot, failRecovery bool
	}{
		{name: "restart", action: "restart", result: "Restart completed", executable: "old executable"},
		{name: "update", action: "update", result: "Running q-agent v1.1.0", executable: "new executable"},
		{name: "download failure restores original", action: "update", failUpdate: true, result: "Previous executable restored", executable: "old executable"},
		{name: "new startup failure rolls back", action: "update", failBoot: true, result: "Service recovered", executable: "old executable"},
		{name: "failed recovery is not success", action: "update", failBoot: true, failRecovery: true, result: "Automatic startup recovery failed", executable: "old executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := detachedFixture(t)
			h.failUpdate, h.failBoot, h.failRecovery = tc.failUpdate, tc.failBoot, tc.failRecovery
			if err := h.c.Request(context.Background(), tc.action, route()); err != nil {
				t.Fatal(err)
			}
			r, err := h.c.load()
			if err != nil || r.Phase != "armed" || r.Backend != backendDetached || r.HelperPID != h.helperPID {
				t.Fatalf("%+v %v", r, err)
			}
			if len(r.ServeArgv) == 0 || r.ServeArgv[0] != "serve" {
				t.Fatalf("ServeArgv not staged: %+v", r)
			}
			if !h.c.Busy() {
				t.Fatal("detached request must gate new work")
			}
			if err := h.c.Request(context.Background(), tc.action, route()); err == nil {
				t.Fatal("duplicate accepted")
			}
			// Simulate helper running in-process; old bot is considered alive
			// until runDetached signals it.
			h.helperLive = true
			if err := h.c.Run(context.Background(), r.ID); err != nil {
				t.Fatal(err)
			}
			r, _ = h.c.load()
			if r.Phase != "done" || h.c.Busy() || !strings.Contains(r.Result, tc.result) {
				t.Fatalf("%+v", r)
			}
			data, _ := os.ReadFile(h.c.Executable)
			if string(data) != tc.executable {
				t.Fatalf("installed %q", data)
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
			if !ok || !strings.Contains(reply.Parts[0], tc.result) {
				t.Fatalf("%+v", reply)
			}
		})
	}
}

func TestDetachedRequiresServeArgv(t *testing.T) {
	h := detachedFixture(t)
	h.c.ServeArgv = nil
	// os.Args in tests does not start with serve, so this must fail closed
	// without stopping anything.
	if err := h.c.Request(context.Background(), "restart", route()); err == nil {
		t.Fatal("expected rejection without ServeArgv")
	}
	if len(h.spawnCalls) != 0 {
		t.Fatalf("helper spawned despite failure: %v", h.spawnCalls)
	}
}

func TestDetachedInterruptedHelperReleasesGate(t *testing.T) {
	h := detachedFixture(t)
	if err := h.c.Request(context.Background(), "restart", route()); err != nil {
		t.Fatal(err)
	}
	// Helper died before finishing: no boot, no done phase.
	h.helperLive = false
	store, _ := state.Open(h.c.StateDir, nil)
	if err := h.c.Report(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if h.c.Busy() {
		t.Fatal("dead detached helper blocks forever")
	}
	reply, ok := store.Pending()
	if !ok || !strings.Contains(reply.Parts[0], "interrupted") {
		t.Fatalf("%+v", reply)
	}
}

func TestDetachedHelperSurvivesOldExit(t *testing.T) {
	h := detachedFixture(t)
	if err := h.c.Request(context.Background(), "restart", route()); err != nil {
		t.Fatal(err)
	}
	r, _ := h.c.load()
	if r.HelperPID == 0 {
		t.Fatal("HelperPID not staged")
	}
	// Old foreground exits on SIGTERM; helper must observe it.
	if !h.oldAlive {
		t.Fatal("old must be alive until helper signals")
	}
	if err := h.c.Run(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if h.oldAlive {
		t.Fatal("helper did not stop old foreground process")
	}
}

func TestDetachedProcessRotatesWhileRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	ready := filepath.Join(dir, "continue")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := defaultSpawnDetached(executable, []string{"logging-probe", ready}, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	waitFor := func(path, text string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), text) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s never contained %q", path, text)
	}
	waitFor(path, "first day")
	yesterday := time.Now().AddDate(0, 0, -1)
	if err := os.Chtimes(path, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(path, "second day")
	waitFor(path+".1", "first day")
}
