package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	StateDir string   `toml:"state_dir"`
	Provider Provider `toml:"provider"`
	Telegram Telegram `toml:"telegram"`
	Sandbox  Sandbox  `toml:"sandbox"`
	Limits   Limits   `toml:"limits"`
}
type Provider struct {
	Name       string `toml:"name"`
	Model      string `toml:"model"`
	BaseURL    string `toml:"base_url"`
	APIKeyEnv  string `toml:"api_key_env"`
	APIKeyPath string `toml:"api_key_path"`
	Thinking   string `toml:"thinking"`
}
type Telegram struct {
	TokenEnv      string  `toml:"token_env"`
	ApprovedUsers []int64 `toml:"approved_user_ids"`
	ApprovedChats []int64 `toml:"approved_chat_ids"`
}
type Sandbox struct {
	Enabled    bool   `toml:"enabled"`
	BaseImage  string `toml:"base_image"`
	Worker     string `toml:"worker"`
	Bubblewrap string `toml:"bubblewrap"`
	NewUIDMap  string `toml:"newuidmap"`
	NewGIDMap  string `toml:"newgidmap"`
	MMdebstrap string `toml:"mmdebstrap"`
}
type Limits struct {
	MaxSteps      int    `toml:"max_steps"`
	MaxToolCalls  int    `toml:"max_tool_calls"`
	MaxConcurrent int    `toml:"max_concurrent"`
	TaskTimeout   string `toml:"task_timeout"`
	ShellTimeout  string `toml:"shell_timeout"`
}

func Defaults() Config {
	return Config{StateDir: "~/.local/share/q-agent", Provider: Provider{Name: "ollama", Model: "qwen3"}, Telegram: Telegram{TokenEnv: "TELEGRAM_BOT_TOKEN"}, Sandbox: Sandbox{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", MMdebstrap: "mmdebstrap"}, Limits: Limits{MaxSteps: 8, MaxToolCalls: 16, MaxConcurrent: 2, TaskTimeout: "5m", ShellTimeout: "2m"}}
}
func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return c, err
	}
	if len(data) > 1<<20 {
		return c, fmt.Errorf("configuration exceeds 1 MiB")
	}
	d := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if c.Provider.Name != "openai" && c.Provider.Name != "ollama" && c.Provider.Name != "opencode-go" {
		return c, fmt.Errorf("unknown provider %q", c.Provider.Name)
	}
	if c.Provider.APIKeyPath != "" {
		if c.Provider.APIKeyEnv != "" {
			return c, fmt.Errorf("provider.api_key_env and provider.api_key_path are mutually exclusive")
		}
		c.Provider.APIKeyPath, err = absolute(c.Provider.APIKeyPath, filepath.Dir(path))
		if err != nil {
			return c, err
		}
	} else if c.Provider.APIKeyEnv == "" {
		switch c.Provider.Name {
		case "openai":
			c.Provider.APIKeyEnv = "OPENAI_API_KEY"
		case "opencode-go":
			c.Provider.APIKeyEnv = "OPENCODE_GO_API_KEY"
		}
	}
	if c.Provider.Model == "" {
		return c, fmt.Errorf("provider.model is required")
	}
	if c.Limits.MaxSteps < 1 || c.Limits.MaxToolCalls < 1 || c.Limits.MaxConcurrent < 1 || c.Limits.MaxConcurrent > 2 {
		return c, fmt.Errorf("positive limits required; max_concurrent must be 1 or 2")
	}
	for _, s := range []string{c.Limits.TaskTimeout, c.Limits.ShellTimeout} {
		if d, e := time.ParseDuration(s); e != nil || d <= 0 {
			return c, fmt.Errorf("invalid positive duration %q", s)
		}
	}
	c.StateDir, err = absolute(c.StateDir, filepath.Dir(path))
	if err != nil {
		return c, err
	}
	if c.Sandbox.BaseImage == "" {
		c.Sandbox.BaseImage = filepath.Join(c.StateDir, "images", "debian-trixie.tar")
	} else {
		c.Sandbox.BaseImage, err = absolute(c.Sandbox.BaseImage, filepath.Dir(path))
		if err != nil {
			return c, err
		}
	}
	if c.Sandbox.Worker == "" && c.Sandbox.Enabled {
		c.Sandbox.Worker, err = os.Executable()
	} else if c.Sandbox.Worker != "" {
		c.Sandbox.Worker, err = absolute(c.Sandbox.Worker, filepath.Dir(path))
	}
	if err != nil {
		return c, err
	}
	return c, nil
}
func absolute(path, base string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Abs(path)
}

// maxAPIKeyFileSize bounds key-file reads. Provider keys are short bearer
// tokens; the limit only guards against misconfigured paths.
const maxAPIKeyFileSize = 64 * 1024

// APIKey returns the provider credential from api_key_path when configured,
// otherwise from api_key_env. An empty env name yields an empty key so
// providers without credentials (ollama defaults) keep working.
func (p Provider) APIKey() (string, error) {
	if p.APIKeyPath == "" {
		if p.APIKeyEnv == "" {
			return "", nil
		}
		return os.Getenv(p.APIKeyEnv), nil
	}
	return readAPIKeyFile(p.APIKeyPath)
}

func checkKeyFile(stat os.FileInfo, path string) error {
	if stat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("provider api_key_path %q must not be a symlink", path)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("provider api_key_path %q must be a regular file", path)
	}
	if perm := stat.Mode().Perm(); perm != 0600 && perm != 0400 {
		return fmt.Errorf("provider api_key_path %q must have mode 0600 or 0400 (has %04o)", path, perm)
	}
	if runtime.GOOS == "linux" {
		native, ok := stat.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("provider api_key_path %q ownership unknown", path)
		}
		if native.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("provider api_key_path %q must be owned by the current user", path)
		}
	}
	if stat.Size() > maxAPIKeyFileSize {
		return fmt.Errorf("provider api_key_path %q exceeds %d bytes", path, maxAPIKeyFileSize)
	}
	return nil
}

// readAPIKeyFile loads a bearer token with owner-only permission checks.
// Error messages describe the path and the failed check, never the secret.
func readAPIKeyFile(path string) (string, error) {
	link, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("provider api_key_path: %w", err)
	}
	if err := checkKeyFile(link, path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("provider api_key_path: %w", err)
	}
	defer f.Close()
	live, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("provider api_key_path: %w", err)
	}
	if err := checkKeyFile(live, path); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAPIKeyFileSize+1))
	if err != nil {
		return "", fmt.Errorf("provider api_key_path: %w", err)
	}
	if len(data) > maxAPIKeyFileSize {
		return "", fmt.Errorf("provider api_key_path %q exceeds %d bytes", path, maxAPIKeyFileSize)
	}
	return strings.TrimSpace(string(data)), nil
}
func (c Config) TaskDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.TaskTimeout)
	return d
}
func (c Config) ShellDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.ShellTimeout)
	return d
}
