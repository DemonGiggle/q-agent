//go:build linux

package maintenance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"q-agent/internal/platform"
	"q-agent/internal/state"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("Q_AGENT_MAINTENANCE_PROBE_MODE"); mode != "" {
		if err := maintenanceProbe(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// This offline bot/helper pair uses real systemd cgroups and the production
// controller. Only the service name is remapped to a disposable probe unit.
// Neither Telegram nor the installed q-agent.service is ever contacted.
func maintenanceProbe(mode string) error {
	dir, unit := os.Getenv("Q_AGENT_MAINTENANCE_PROBE_DIR"), os.Getenv("Q_AGENT_MAINTENANCE_PROBE_UNIT")
	if !strings.HasPrefix(unit, "q-agent-maintenance-probe-") {
		return fmt.Errorf("invalid test unit")
	}
	c := &Controller{StateDir: dir, Version: "v-probe", Notify: func(context.Context, platform.Message, string) error { return state.Atomic(dir+"/notice.json", true) }}
	c.Command = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mapped := make([]string, 0, len(args)+3)
		for _, arg := range args {
			if name == "systemd-run" && arg == "--" {
				mapped = append(mapped, "--setenv=Q_AGENT_MAINTENANCE_PROBE_MODE=helper", "--setenv=Q_AGENT_MAINTENANCE_PROBE_DIR="+dir, "--setenv=Q_AGENT_MAINTENANCE_PROBE_UNIT="+unit)
			}
			mapped = append(mapped, strings.ReplaceAll(arg, serviceUnit, unit))
		}
		return exec.CommandContext(ctx, name, mapped...).CombinedOutput()
	}
	if mode == "helper" {
		if len(os.Args) != 7 || os.Args[1] != "service" || os.Args[2] != "maintain" || os.Args[3] != "--state-dir" || os.Args[5] != "--id" {
			return fmt.Errorf("unexpected helper args: %v", os.Args)
		}
		return c.Run(context.Background(), os.Args[len(os.Args)-1])
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	if _, err := c.load(); os.IsNotExist(err) {
		if err := c.Request(ctx, "restart", platform.Message{Platform: "telegram", ChatID: 1, UserID: 1, UpdateID: 1}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := c.MarkReady(); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func TestSystemdMaintenanceSurvivesBotCgroupStop(t *testing.T) {
	if os.Getenv("Q_AGENT_MAINTENANCE_SYSTEMD_TEST") != "1" {
		t.Skip("opt-in: requires a running systemd user manager; uses only disposable offline units")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	unit := fmt.Sprintf("q-agent-maintenance-probe-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	command := func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	before, err := command("systemctl", "--user", "show", unit, "--property=LoadState", "--value")
	if err != nil || strings.TrimSpace(string(before)) != "not-found" {
		t.Fatalf("refusing existing unit/unavailable manager: %s %v", before, err)
	}
	c := &Controller{StateDir: dir}
	t.Cleanup(func() {
		if r, err := c.load(); err == nil {
			command("systemctl", "--user", "stop", r.HelperUnit)
			command("systemctl", "--user", "reset-failed", r.HelperUnit)
		}
		command("systemctl", "--user", "stop", unit)
		command("systemctl", "--user", "disable", "--runtime", unit)
		command("systemctl", "--user", "reset-failed", unit)
		command("systemctl", "--user", "daemon-reload")
	})
	path := filepath.Join(dir, unit)
	contents := fmt.Sprintf("[Service]\nType=exec\nExecStart=%s\nKillMode=control-group\nEnvironment=%s\nEnvironment=%s\nEnvironment=%s\n", strconv.Quote(binary), strconv.Quote("Q_AGENT_MAINTENANCE_PROBE_MODE=bot"), strconv.Quote("Q_AGENT_MAINTENANCE_PROBE_DIR="+dir), strconv.Quote("Q_AGENT_MAINTENANCE_PROBE_UNIT="+unit))
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := command("systemctl", "--user", "link", "--runtime", path); err != nil {
		t.Fatalf("link disposable probe: %s %v", out, err)
	}
	if out, err := command("systemctl", "--user", "daemon-reload"); err != nil {
		t.Fatalf("reload disposable probe: %s %v", out, err)
	}
	out, err := command("systemctl", "--user", "start", unit)
	if err != nil {
		t.Fatalf("start probe: %s %v", out, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := c.load()
		if err == nil && r.Phase == "done" {
			if !strings.Contains(r.Result, "Restart completed. Running q-agent v-probe") {
				logs, _ := command("journalctl", "--user", "-u", unit, "-u", r.HelperUnit, "--no-pager", "-n", "40")
				t.Fatalf("%+v\n%s", r, logs)
			}
			pid, err := command("systemctl", "--user", "show", unit, "--property=MainPID", "--value")
			if err != nil || strings.TrimSpace(string(pid)) == strconv.Itoa(r.OldPID) || strings.TrimSpace(string(pid)) == "0" {
				t.Fatalf("probe not restarted: %s %v", pid, err)
			}
			if _, err := os.Stat(dir + "/notice.json"); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(c.path("request.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("non-private request state")
			}
			t.Logf("real systemd restart: old PID=%d, new PID=%s; independent helper=%s", r.OldPID, strings.TrimSpace(string(pid)), r.HelperUnit)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := c.load()
	logs, _ := command("journalctl", "--user", "-u", unit, "-u", r.HelperUnit, "--no-pager", "-n", "40")
	t.Fatalf("probe timed out: %+v\n%s", r, logs)
}
