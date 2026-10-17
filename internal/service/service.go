// Package service installs a systemd user unit without starting the bot or
// changing existing configuration. systemctl remains the service manager.
package service

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"q-agent/internal/config"
	"q-agent/internal/state"
)

const Help = `Usage: q-agent service install|print [--config FILE]

install  Create a systemd user unit and, if missing, a private config template.
print    Print the unit for review without writing files.

Install never overwrites a different unit or any existing config, imports
credentials, reloads systemd, enables a service, or starts the bot.
Use systemctl --user to manage q-agent.service after reviewing the config.
`

const initialConfig = `# Created by q-agent service install. Edit before starting the service.
state_dir = "~/.local/share/q-agent"

[provider]
name = "ollama"
model = "qwen3"
base_url = "http://127.0.0.1:11434"
# For OpenAI / OpenCode Go, change name/model, remove the Ollama base_url,
# and use api_key_path = "provider.key" (owned by this user, mode 0600).

[telegram]
token_path = "telegram.token"
approved_user_ids = []
approved_chat_ids = []

[sandbox]
enabled = false
# To enable: allocate subordinate IDs, install dependencies, and build the
# image as this same account first. See docs/systemd.md.
`

type Options struct {
	HomeDir, ConfigHome, Executable string
	Stdout, Stderr                  io.Writer
	// UID and GOOS make platform/identity rejection testable without sudo.
	UID  func() int
	GOOS string
}

func Run(args []string, o Options) error {
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(o.Stdout, Help)
		return err
	}
	action := args[0]
	if action != "install" && action != "print" {
		return fmt.Errorf("unknown service action %q; use q-agent service --help", action)
	}
	flags := flag.NewFlagSet("q-agent service "+action, flag.ContinueOnError)
	flags.SetOutput(o.Stderr)
	configPath := flags.String("config", config.DefaultPath, "configuration file")
	flags.Usage = func() { fmt.Fprint(o.Stderr, Help) }
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service %s does not accept positional arguments", action)
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOOS != "linux" {
		return fmt.Errorf("systemd service installation requires Linux")
	}
	if o.UID == nil {
		o.UID = os.Geteuid
	}
	if o.UID() == 0 {
		return fmt.Errorf("run service %s as the non-root account that will own the bot; do not use sudo", action)
	}
	var err error
	if o.HomeDir == "" {
		o.HomeDir, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	if !filepath.IsAbs(o.HomeDir) {
		return fmt.Errorf("home directory must be absolute")
	}
	if o.ConfigHome == "" {
		o.ConfigHome = os.Getenv("XDG_CONFIG_HOME")
	}
	if o.ConfigHome == "" {
		o.ConfigHome = filepath.Join(o.HomeDir, ".config")
	}
	if !filepath.IsAbs(o.ConfigHome) {
		return fmt.Errorf("XDG_CONFIG_HOME must be absolute")
	}
	if *configPath == "" {
		return fmt.Errorf("--config requires a file")
	}
	path := *configPath
	if path == "~" {
		path = o.HomeDir
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(o.HomeDir, path[2:])
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	if o.Executable == "" {
		o.Executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	executable, err := filepath.Abs(o.Executable)
	if err != nil {
		return err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("inspect executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("service executable must be a regular executable file")
	}
	unit, err := render(executable, path)
	if err != nil {
		return err
	}
	if action == "print" {
		_, err := io.WriteString(o.Stdout, unit)
		return err
	}
	unitPath := filepath.Join(o.ConfigHome, "systemd", "user", "q-agent.service")
	exists, err := sameUnit(unitPath, unit)
	if err != nil {
		return err
	}
	_, err = os.Lstat(path)
	created := false
	if os.IsNotExist(err) {
		if err = writeNew(path, initialConfig, 0600); err != nil {
			return fmt.Errorf("create config: %w", err)
		}
		created = true
	} else if err != nil {
		return err
	}
	// Load only settings, never credential contents or environment values.
	if _, err := config.Load(path); err != nil {
		return fmt.Errorf("configuration unchanged; fix it before installation: %w", err)
	}
	if !exists {
		if err = writeNew(unitPath, unit, 0644); err != nil {
			return fmt.Errorf("install user unit: %w", err)
		}
	}
	verb := "Preserved existing"
	if created {
		verb = "Created private template"
	}
	fmt.Fprintf(o.Stdout, "%s config: %s\nUser unit: %s\nExecutable: %s\n", verb, path, unitPath, executable)
	fmt.Fprintln(o.Stdout, "Review config/allowlists and credential files before starting. Keep the executable at this path.\nThen run:\n  systemctl --user daemon-reload\n  systemctl --user enable --now q-agent.service\n  systemctl --user status q-agent.service\nLogs: journalctl --user -u q-agent.service\nFor boot without login, an administrator must enable lingering for this account.\nNothing was enabled or started.")
	return nil
}

func quote(value string, exec bool) (string, error) {
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("service paths must not contain control characters")
		}
	}
	value = strings.ReplaceAll(value, "%", "%%")
	if exec {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\"", nil
}

func render(executable, configPath string) (string, error) {
	if strings.ContainsAny(executable, "\"\\") {
		return "", fmt.Errorf("systemd executable paths cannot contain quotes or backslashes; install q-agent at a plain path")
	}
	// systemd does not expand environment variables in the executable word.
	// Only subsequent arguments require doubling literal dollar signs.
	exe, err := quote(executable, false)
	if err != nil {
		return "", err
	}
	cfg, err := quote(configPath, true)
	if err != nil {
		return "", err
	}
	// Unlike ExecStart, EnvironmentFile is one unquoted path, not a command
	// word list. Quotes would become literal filename characters in systemd.
	env := filepath.Join(filepath.Dir(configPath), "service.env")
	// EnvironmentFile uses glob(), even for an otherwise literal filename.
	// Escape glob metacharacters so a configured path cannot match other files.
	env = strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]", "%", "%%").Replace(env)
	return fmt.Sprintf(`# Generated by q-agent service install. Review before enabling.
[Unit]
Description=q-agent Telegram agent

[Service]
Type=simple
WorkingDirectory=%%h
ExecStart=%s serve --config %s
Environment="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:%%h/.local/bin"
EnvironmentFile=-%s
UMask=0077
Restart=on-failure
RestartSec=5s
KillMode=control-group
TimeoutStopSec=30s
StandardOutput=journal
StandardError=journal
# Deliberately no NoNewPrivileges, RestrictNamespaces, PrivateUsers,
# CapabilityBoundingSet or syscall filters: rootless sandbox mapping needs
# user namespaces and the setuid newuidmap/newgidmap helpers.

[Install]
WantedBy=default.target
`, exe, cfg, env), nil
}

func sameUnit(path, unit string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refusing to replace non-regular unit %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if string(data) != unit {
		return false, fmt.Errorf("unit already exists with different content at %s; review and move it aside before reinstalling", path)
	}
	return true, nil
}

// Publish without clobbering a concurrently-created file. Keep existing files
// and directory modes intact; only new directories get private permissions.
func writeNew(path, contents string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".q-agent-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = io.WriteString(f, contents); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("file appeared during installation; left unchanged: %s", path)
		}
		return err
	}
	return state.SyncDir(dir)
}
