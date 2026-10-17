package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"q-agent/internal/config"
)

func fixture(t *testing.T) (Options, string, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "q-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	o := Options{HomeDir: home, ConfigHome: filepath.Join(home, ".config"), Executable: exe, UID: func() int { return 1000 }, GOOS: "linux"}
	return o, filepath.Join(home, ".config", "q-agent", "config.toml"), filepath.Join(o.ConfigHome, "systemd", "user", "q-agent.service")
}
func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func TestInstallCreatesPrivateTemplateAndUserUnitOnly(t *testing.T) {
	o, cfg, unit := fixture(t)
	var output bytes.Buffer
	o.Stdout = &output
	if err := Run([]string{"install"}, o); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{cfg: 0600, unit: 0644} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode %s: %v %v", path, info, err)
		}
	}
	c, err := config.Load(cfg)
	if err != nil || c.Sandbox.Enabled || c.Telegram.TokenEnv != "" || c.Telegram.TokenPath != filepath.Join(filepath.Dir(cfg), "telegram.token") || len(c.Telegram.ApprovedUsers) != 0 || len(c.Telegram.ApprovedChats) != 0 {
		t.Fatalf("template: %+v %v", c, err)
	}
	rendered := read(t, unit)
	if !strings.Contains(rendered, `ExecStart="`+o.Executable+`" serve --config "`+cfg+`"`) || !strings.Contains(rendered, "Restart=on-failure") || !strings.Contains(rendered, "KillMode=control-group") || !strings.Contains(rendered, "EnvironmentFile=-"+filepath.Dir(cfg)+"/service.env") {
		t.Fatalf("unit:\n%s", rendered)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "NoNewPrivileges=") || strings.HasPrefix(line, "RestrictNamespaces=") || strings.HasPrefix(line, "PrivateUsers=") || strings.HasPrefix(line, "CapabilityBoundingSet=") {
			t.Fatalf("sandbox-incompatible restriction: %s", line)
		}
	}
	if !strings.Contains(output.String(), "Nothing was enabled or started.") {
		t.Fatalf("output: %s", &output)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "telegram.token")); !os.IsNotExist(err) {
		t.Fatal("installer created credentials")
	}
}

func TestExistingConfigAndSameUnitArePreserved(t *testing.T) {
	o, cfg, unit := fixture(t)
	if err := Run([]string{"install"}, o); err != nil {
		t.Fatal(err)
	}
	contents := "# Keep my comments\nstate_dir = './my-state'\n[telegram]\ntoken_path = 'missing-private-file'\n[sandbox]\nenabled = true\n"
	if err := os.WriteFile(cfg, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0400); err != nil {
		t.Fatal(err)
	}
	before := read(t, unit)
	if err := Run([]string{"install"}, o); err != nil {
		t.Fatal(err)
	}
	if read(t, cfg) != contents || read(t, unit) != before {
		t.Fatal("existing configuration or unit was changed")
	}
	info, err := os.Stat(cfg)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("existing mode changed")
	}
}

func TestInstallRefusesForeignOrSymlinkUnitBeforeCreatingConfig(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{true: "symlink", false: "foreign"}[symlink], func(t *testing.T) {
			o, cfg, unit := fixture(t)
			if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
				t.Fatal(err)
			}
			if symlink {
				target := filepath.Join(t.TempDir(), "other.service")
				if err := os.WriteFile(target, []byte("foreign unit"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, unit); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(unit, []byte("foreign unit"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Run([]string{"install"}, o); err == nil {
				t.Fatal("unit conflict accepted")
			}
			if read(t, unit) != "foreign unit" {
				t.Fatal("foreign unit changed")
			}
			if _, err := os.Stat(cfg); !os.IsNotExist(err) {
				t.Fatal("config written despite unit conflict")
			}
		})
	}
}

func TestInvalidConfigIsNotReplacedAndUnitIsNotInstalled(t *testing.T) {
	o, cfg, unit := fixture(t)
	if err := os.MkdirAll(filepath.Dir(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	const invalid = "unexpected_key = true\n"
	if err := os.WriteFile(cfg, []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"install"}, o); err == nil {
		t.Fatal("invalid config accepted")
	}
	if read(t, cfg) != invalid {
		t.Fatal("invalid config clobbered")
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit installed with invalid config")
	}
}

func TestPrintAndArgumentValidationHaveNoSideEffects(t *testing.T) {
	o, cfg, unit := fixture(t)
	var out bytes.Buffer
	o.Stdout = &out
	if err := Run([]string{"print", "--config", cfg}, o); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "# Generated by q-agent") {
		t.Fatalf("print output: %s", &out)
	}
	for _, path := range []string{cfg, unit} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("print wrote %s", path)
		}
	}
	for _, args := range [][]string{{"install", "--bad"}, {"install", "extra"}, {"install", "--config="}, {"start"}} {
		if err := Run(args, o); err == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
	o.UID = func() int { return 0 }
	if err := Run([]string{"install"}, o); err == nil {
		t.Fatal("root installation allowed")
	}
	o.UID = func() int { return 1000 }
	o.GOOS = "darwin"
	if err := Run([]string{"install"}, o); err == nil {
		t.Fatal("non-Linux installation allowed")
	}
}

func TestRenderedUnitPassesSystemdParserWithSpecialPaths(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze unavailable")
	}
	dir := filepath.Join(t.TempDir(), `space $dollar%percent"quote\backslash[literal]*?`)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, cfg := filepath.Join(t.TempDir(), "agent $cash%percent"), filepath.Join(dir, "config.toml")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	unit, err := render(exe, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "q-agent-parser-test.service")
	if err := os.WriteFile(path, []byte(unit), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(analyze, "--user", "verify", path)
	// Parser verification must also work on CI without a logged-in user bus.
	command.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir())
	output, err := command.CombinedOutput()
	if err != nil || len(output) != 0 {
		t.Fatalf("unit parser: %v\n%s\n%s", err, output, unit)
	}
	for _, unsafe := range []string{"/tmp/config\ninjected=true", "/tmp/null\x00file"} {
		if _, err := render(exe, unsafe); err == nil {
			t.Fatal("control character accepted")
		}
	}
	for _, unsupported := range []string{`/tmp/agent"quote`, `/tmp/agent\backslash`} {
		if _, err := render(unsupported, cfg); err == nil {
			t.Fatal("systemd-unsupported executable path accepted")
		}
	}
}

func TestWriteNewCannotClobberFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(path, []byte("original"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(path, "replacement", 0600); err == nil {
		t.Fatal("existing file overwritten")
	}
	if read(t, path) != "original" {
		t.Fatal("file changed")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary files leaked")
	}
}
