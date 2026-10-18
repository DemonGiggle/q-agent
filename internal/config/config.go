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

// DefaultPath is the configuration file used when --config is omitted.
const DefaultPath = "~/.config/q-agent/config.toml"

type Config struct {
	StateDir  string              `toml:"state_dir"`
	Provider  Provider            `toml:"provider"`
	Providers map[string]Provider `toml:"providers"`
	Telegram  Telegram            `toml:"telegram"`
	Sandbox   Sandbox             `toml:"sandbox"`
	Limits    Limits              `toml:"limits"`
	Memory    Memory              `toml:"memory"`
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
	TokenPath     string  `toml:"token_path"`
	ApprovedUsers []int64 `toml:"approved_user_ids"`
	ApprovedChats []int64 `toml:"approved_chat_ids"`
	AdminUsers    []int64 `toml:"admin_user_ids"`
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
	// How long to wait for an extend/kill reply after a shell timeout prompt.
	// Empty defaults to 90s. Must parse as a positive duration when set.
	ShellPromptTimeout string `toml:"shell_prompt_timeout"`
	// How many user-approved extensions a single shell call may take.
	// Values <=0 default to 2.
	ShellMaxExtends int `toml:"shell_max_extends"`
}

// Memory configures the nightly long-term memory digest. The sweep distills
// each chat/group's day into workspace-scoped notes shared by its topics.
// Timezone empty means server-local midnight. MaxBytes caps the stored
// notes; MaxContextBytes caps how much memory each chat turn may consume.
type Memory struct {
	Enabled         bool   `toml:"enabled"`
	Timezone        string `toml:"timezone"`
	DigestAt        string `toml:"digest_at"`
	MaxBytes        int    `toml:"max_bytes"`
	MaxContextBytes int    `toml:"max_context_bytes"`
	Model           string `toml:"model"`
}

func Defaults() Config {
	return Config{StateDir: "~/.local/share/q-agent", Provider: Provider{Name: "ollama", Model: "qwen3"}, Telegram: Telegram{TokenEnv: "TELEGRAM_BOT_TOKEN"}, Sandbox: Sandbox{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", MMdebstrap: "mmdebstrap"}, Limits: Limits{MaxSteps: 8, MaxToolCalls: 16, MaxConcurrent: 2, TaskTimeout: "5m", ShellTimeout: "2m", ShellPromptTimeout: "90s", ShellMaxExtends: 2}, Memory: Memory{Enabled: true, DigestAt: "00:00", MaxBytes: 8192, MaxContextBytes: 2000}}
}
func Load(path string) (Config, error) {
	c := Defaults()
	path, err := absolute(path, ".")
	if err != nil {
		return c, err
	}
	// Clear the default credential name before decoding so an omitted
	// token_env stays empty and can be distinguished from an explicitly
	// configured one. The default is restored below when no token_path
	// is configured.
	c.Telegram.TokenEnv = ""
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
	if err = normalizeProvider(&c.Provider, filepath.Dir(path)); err != nil {
		return c, err
	}
	for name, provider := range c.Providers {
		if name == c.Provider.Name {
			return c, fmt.Errorf("providers.%s duplicates the default provider", name)
		}
		if provider.Name != "" && provider.Name != name {
			return c, fmt.Errorf("providers.%s.name must match its table name", name)
		}
		provider.Name = name
		if err = normalizeProvider(&provider, filepath.Dir(path)); err != nil {
			return c, fmt.Errorf("providers.%s: %w", name, err)
		}
		c.Providers[name] = provider
	}
	if c.Telegram.TokenPath != "" {
		if c.Telegram.TokenEnv != "" {
			return c, fmt.Errorf("telegram.token_env and telegram.token_path are mutually exclusive")
		}
		c.Telegram.TokenPath, err = absolute(c.Telegram.TokenPath, filepath.Dir(path))
		if err != nil {
			return c, err
		}
	} else if c.Telegram.TokenEnv == "" {
		c.Telegram.TokenEnv = "TELEGRAM_BOT_TOKEN"
	}
	for _, admin := range c.Telegram.AdminUsers {
		approved := false
		for _, user := range c.Telegram.ApprovedUsers {
			approved = approved || admin == user
		}
		if admin <= 0 || !approved {
			return c, fmt.Errorf("telegram.admin_user_ids must contain positive IDs from approved_user_ids")
		}
	}
	if c.Limits.MaxSteps < 1 || c.Limits.MaxToolCalls < 1 || c.Limits.MaxConcurrent < 1 || c.Limits.MaxConcurrent > 2 {
		return c, fmt.Errorf("positive limits required; max_concurrent must be 1 or 2")
	}
	for _, s := range []string{c.Limits.TaskTimeout, c.Limits.ShellTimeout} {
		if d, e := time.ParseDuration(s); e != nil || d <= 0 {
			return c, fmt.Errorf("invalid positive duration %q", s)
		}
	}
	if c.Limits.ShellPromptTimeout != "" {
		if d, e := time.ParseDuration(c.Limits.ShellPromptTimeout); e != nil || d <= 0 {
			return c, fmt.Errorf("invalid positive duration %q", c.Limits.ShellPromptTimeout)
		}
	}
	if c.Limits.ShellMaxExtends < 0 {
		return c, fmt.Errorf("shell_max_extends must not be negative")
	}
	if c.Memory.DigestAt == "" {
		c.Memory.DigestAt = "00:00"
	}
	if _, _, err := parseDigestAt(c.Memory.DigestAt); err != nil {
		return c, err
	}
	if c.Memory.Timezone != "" {
		if _, err := time.LoadLocation(c.Memory.Timezone); err != nil {
			return c, fmt.Errorf("invalid memory timezone: %w", err)
		}
	}
	if c.Memory.MaxBytes <= 0 {
		return c, fmt.Errorf("memory.max_bytes must be positive")
	}
	if c.Memory.MaxContextBytes <= 0 {
		return c, fmt.Errorf("memory.max_context_bytes must be positive")
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

// parseDigestAt parses a daily HH:MM wall time for the memory digest sweep.
func parseDigestAt(s string) (hour, min int, err error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, fmt.Errorf("invalid memory digest_at %q: want HH:MM", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 || len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("invalid memory digest_at %q: want HH:MM", s)
	}
	return h, m, nil
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

// maxCredentialFileSize bounds credential-file reads. Provider keys and
// Telegram tokens are short bearer tokens; the limit only guards against
// misconfigured paths.
const maxCredentialFileSize = 64 * 1024

// maxAPIKeyFileSize is retained for compatibility with existing callers.
const maxAPIKeyFileSize = maxCredentialFileSize

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
	return readCredentialFile(p.APIKeyPath, "provider api_key_path")
}

// Token returns the Telegram credential from token_path when configured,
// otherwise from token_env.
func (t Telegram) Token() (string, error) {
	if t.TokenPath == "" {
		if t.TokenEnv == "" {
			return "", nil
		}
		return os.Getenv(t.TokenEnv), nil
	}
	return readCredentialFile(t.TokenPath, "telegram token_path")
}

func checkCredentialFile(stat os.FileInfo, path, label string) error {
	if stat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %q must not be a symlink", label, path)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("%s %q must be a regular file", label, path)
	}
	if perm := stat.Mode().Perm(); perm != 0600 && perm != 0400 {
		return fmt.Errorf("%s %q must have mode 0600 or 0400 (has %04o)", label, path, perm)
	}
	if runtime.GOOS == "linux" {
		native, ok := stat.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s %q ownership unknown", label, path)
		}
		if native.Uid != uint32(os.Getuid()) {
			return fmt.Errorf("%s %q must be owned by the current user", label, path)
		}
	}
	if stat.Size() > maxCredentialFileSize {
		return fmt.Errorf("%s %q exceeds %d bytes", label, path, maxCredentialFileSize)
	}
	return nil
}

func checkKeyFile(stat os.FileInfo, path string) error {
	return checkCredentialFile(stat, path, "provider api_key_path")
}

// readCredentialFile loads a bearer token with owner-only permission checks.
// Error messages describe the path and the failed check, never the secret.
func readCredentialFile(path, label string) (string, error) {
	link, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if err := checkCredentialFile(link, path, label); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	defer f.Close()
	live, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if err := checkCredentialFile(live, path, label); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialFileSize+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if len(data) > maxCredentialFileSize {
		return "", fmt.Errorf("%s %q exceeds %d bytes", label, path, maxCredentialFileSize)
	}
	return strings.TrimSpace(string(data)), nil
}

// readAPIKeyFile loads a bearer token with owner-only permission checks.
// Error messages describe the path and the failed check, never the secret.
func readAPIKeyFile(path string) (string, error) {
	return readCredentialFile(path, "provider api_key_path")
}
func (c Config) TaskDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.TaskTimeout)
	return d
}
func (c Config) ShellDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.ShellTimeout)
	return d
}

// ShellPromptWait bounds how long a shell-timeout prompt waits for extend/kill.
func (c Config) ShellPromptWait() time.Duration {
	if c.Limits.ShellPromptTimeout == "" {
		return 90 * time.Second
	}
	if d, err := time.ParseDuration(c.Limits.ShellPromptTimeout); err == nil && d > 0 {
		return d
	}
	return 90 * time.Second
}

// ShellExtends caps user-approved shell extensions per call.
func (c Config) ShellExtends() int {
	if c.Limits.ShellMaxExtends <= 0 {
		return 2
	}
	return c.Limits.ShellMaxExtends
}

// ProviderByName only permits providers configured by the operator.
func (c Config) ProviderByName(name string) (Provider, bool) {
	if name == c.Provider.Name {
		return c.Provider, true
	}
	p, ok := c.Providers[name]
	p.Name = name
	return p, ok
}

func normalizeProvider(p *Provider, dir string) error {
	if p.Name != "openai" && p.Name != "ollama" && p.Name != "opencode-go" {
		return fmt.Errorf("unknown provider %q", p.Name)
	}
	if p.Model == "" {
		return fmt.Errorf("provider.model is required")
	}
	if p.APIKeyPath != "" {
		if p.APIKeyEnv != "" {
			return fmt.Errorf("provider.api_key_env and provider.api_key_path are mutually exclusive")
		}
		path, err := absolute(p.APIKeyPath, dir)
		if err != nil {
			return err
		}
		p.APIKeyPath = path
	} else if p.APIKeyEnv == "" {
		switch p.Name {
		case "openai":
			p.APIKeyEnv = "OPENAI_API_KEY"
		case "opencode-go":
			p.APIKeyEnv = "OPENCODE_GO_API_KEY"
		}
	}
	return nil
}
