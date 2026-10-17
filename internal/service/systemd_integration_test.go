//go:build linux

package service

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/sandbox"
	"q-agent/internal/state"
)

// The test binary doubles as a finite, offline ExecStart probe. It never
// contacts Telegram/providers and is never installed as q-agent.service.
func TestMain(m *testing.M) {
	if os.Getenv("Q_AGENT_SYSTEMD_PROBE") == "1" {
		err := systemdProbe()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func systemdProbe() error {
	if len(os.Args) != 4 || os.Args[1] != "serve" || os.Args[2] != "--config" {
		return fmt.Errorf("unexpected ExecStart arguments")
	}
	c, err := config.Load(os.Args[3])
	if err != nil {
		return err
	}
	if token, err := c.Telegram.Token(); err != nil || token != "offline-systemd-test-token" {
		return fmt.Errorf("private credential file not resolved: %v", err)
	}
	if os.Getenv("Q_AGENT_ENV_PROBE") != "environment-file-loaded" {
		return fmt.Errorf("service.env not loaded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	root := filepath.Join(c.StateDir, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	execute := func(call llm.ToolCall) (string, error) {
		return localtools.Execute(ctx, call, localtools.Options{WorkingDir: root})
	}
	if c.Sandbox.Enabled {
		options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", Worker: os.Getenv("Q_AGENT_PROBE_WORKER")})
		if err != nil {
			return err
		}
		// An empty tar is sufficient to exercise real mapping, extraction,
		// mounts and static Go read/write tools without downloading Debian.
		image := filepath.Join(c.StateDir, "probe.tar")
		f, err := os.Create(image)
		if err != nil {
			return err
		}
		writer := tar.NewWriter(f)
		err = writer.Close()
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := sandbox.Remove(cleanup, options, root); err != nil {
				fmt.Fprintln(os.Stderr, "probe cleanup:", err)
			}
		}()
		if err := sandbox.Provision(ctx, options, image, root); err != nil {
			return err
		}
		runner := &sandbox.Runner{Options: options, RootFS: root}
		execute = func(call llm.ToolCall) (string, error) { return runner.Execute(ctx, call) }
	}
	write := llm.ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"probe.txt","content":"systemd-probe-ok"}`)}
	if _, err := execute(write); err != nil {
		return err
	}
	read := llm.ToolCall{Name: "read", Arguments: json.RawMessage(`{"path":"probe.txt"}`)}
	contents, err := execute(read)
	if err != nil || !strings.Contains(contents, "systemd-probe-ok") {
		return fmt.Errorf("service tool roundtrip: %q: %v", contents, err)
	}
	return state.Atomic(filepath.Join(c.StateDir, "result.json"), map[string]any{"ok": true, "sandbox": c.Sandbox.Enabled, "uid": os.Getuid()})
}

func TestIntegrationSystemdHostAndSandbox(t *testing.T) {
	if os.Getenv("Q_AGENT_SYSTEMD_INTEGRATION") != "1" {
		t.Skip("requires a running systemd user manager; set Q_AGENT_SYSTEMD_INTEGRATION=1 and Q_AGENT_WORKER to a static q-agent binary")
	}
	worker := os.Getenv("Q_AGENT_WORKER")
	if !filepath.IsAbs(worker) {
		t.Fatal("Q_AGENT_WORKER must be an absolute static binary path")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, isolated := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "sandbox"}[isolated], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), `config $cash%percent"quote\backslash[literal]*?`)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(dir, "config.toml")
			stateDir := filepath.Join(dir, "state")
			contents := fmt.Sprintf("state_dir = %s\n[telegram]\ntoken_path = 'telegram.token'\n[sandbox]\nenabled = %t\n", strconv.Quote(stateDir), isolated)
			for path, data := range map[string]string{
				cfg:                                  contents,
				filepath.Join(dir, "telegram.token"): "offline-systemd-test-token\n",
				filepath.Join(dir, "service.env"):    "Q_AGENT_SYSTEMD_PROBE=1\nQ_AGENT_ENV_PROBE=environment-file-loaded\nQ_AGENT_PROBE_WORKER=" + strconv.Quote(worker) + "\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			unit, err := render(binary, cfg)
			if err != nil {
				t.Fatal(err)
			}
			unit = strings.Replace(unit, "Restart=on-failure", "Restart=no", 1) // Finite offline probe, all sandbox-related settings unchanged.
			name := fmt.Sprintf("q-agent-qa-%d.service", time.Now().UnixNano())
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte(unit), 0600); err != nil {
				t.Fatal(err)
			}
			command := func(args ...string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				defer cancel()
				return exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
			}
			before, _ := command("show", name, "--property=LoadState", "--value")
			if strings.TrimSpace(string(before)) != "not-found" {
				t.Fatalf("refusing to touch an existing unit or unavailable manager: %s", before)
			}
			t.Cleanup(func() {
				if out, err := command("stop", name); err != nil {
					t.Logf("stop probe: %v %s", err, out)
				}
				if out, err := command("disable", "--runtime", name); err != nil {
					t.Errorf("remove probe link: %v %s", err, out)
				}
				command("reset-failed", name)
				if out, err := command("daemon-reload"); err != nil {
					t.Errorf("reload after cleanup: %v %s", err, out)
				}
			})
			if out, err := command("link", "--runtime", path); err != nil {
				t.Fatalf("link offline probe: %v %s", err, out)
			}
			if out, err := command("daemon-reload"); err != nil {
				t.Fatalf("reload probe: %v %s", err, out)
			}
			if out, err := command("start", "--wait", name); err != nil {
				logs, _ := exec.Command("journalctl", "--user", "-u", name, "--no-pager", "-n", "30").CombinedOutput()
				t.Fatalf("offline service probe: %v %s\n%s", err, out, logs)
			}
			var result struct {
				OK, Sandbox bool
				UID         int
			}
			data, err := os.ReadFile(filepath.Join(stateDir, "result.json"))
			if err != nil {
				logs, _ := exec.Command("journalctl", "--user", "-u", name, "--no-pager", "-n", "40").CombinedOutput()
				properties, _ := command("show", name, "--property=ExecStart,EnvironmentFiles,Result")
				t.Fatalf("%v\n%s\n%s", err, properties, logs)
			}
			if err := json.Unmarshal(data, &result); err != nil || !result.OK || result.Sandbox != isolated || result.UID != os.Getuid() {
				t.Fatalf("probe result: %s, %v", data, err)
			}
		})
	}
}
