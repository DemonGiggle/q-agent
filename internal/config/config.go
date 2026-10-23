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
	Proactive Proactive           `toml:"proactive"`
	ChatGPT   ChatGPT             `toml:"chatgpt"`
}
type Provider struct {
	Name       string `toml:"name"`
	Model      string `toml:"model"`
	BaseURL    string `toml:"base_url"`
	APIKeyEnv  string `toml:"api_key_env"`
	APIKeyPath string `toml:"api_key_path"`
	Thinking   string `toml:"thinking"`
}

// ChatGPT holds the console-only ChatGPT OAuth settings. OAuthPath points at
// the JSON file written by `q-agent login chatgpt`. Empty means the default
// ~/.config/q-agent/chatgpt-oauth.json. It never holds a secret itself.
type ChatGPT struct {
	OAuthPath string `toml:"oauth_path"`
}
type Telegram struct {
	TokenEnv      string  `toml:"token_env"`
	TokenPath     string  `toml:"token_path"`
	AllowedUsers  []int64 `toml:"allowed_users"`
	AllowedGroups []int64 `toml:"allowed_groups"`
	AdminUsers    []int64 `toml:"admin_user_ids"`
	// Deprecated: accepted on load and migrated to AllowedUsers/AllowedGroups.
	// New configurations must use allowed_users and allowed_groups.
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
	MaxSteps               int    `toml:"max_steps"`
	MaxConcurrent          int    `toml:"max_concurrent"`
	TaskTimeout            string `toml:"task_timeout"`
	InteractiveTaskTimeout string `toml:"interactive_task_timeout"`
	ShellTimeout           string `toml:"shell_timeout"`
	// How long to wait for an extend/kill reply after a shell timeout prompt.
	// Empty defaults to 90s. Must parse as a positive duration when set.
	ShellPromptTimeout string `toml:"shell_prompt_timeout"`
	// How many user-approved extensions a single shell call may take.
	// Values <=0 default to 2.
	ShellMaxExtends int `toml:"shell_max_extends"`
}

// Memory configures the nightly long-term memory digest. The sweep distills
// each chat/group's day into workspace-scoped notes shared by its topics.
// Timezone empty means server-local midnight. MaxBytes caps persisted note
// text; MaxContextBytes caps the complete injected message; MaxPendingBytes
// caps the encoded source journal independently of bounded prompt history.
type Memory struct {
	Enabled         bool   `toml:"enabled"`
	Timezone        string `toml:"timezone"`
	DigestAt        string `toml:"digest_at"`
	MaxBytes        int    `toml:"max_bytes"`
	MaxContextBytes int    `toml:"max_context_bytes"`
	MaxPendingBytes int    `toml:"max_pending_bytes"`
	Model           string `toml:"model"`
}

// Proactive configures the opt-out companion check-in sweep. Enabled by
// default; Enabled=false is the single-flag kill switch (no goroutine, no
// extra LLM/history reads, no queue writes). ActiveTime is the daily
// wall-clock window (HH:MM-HH:MM) the companion may wake in; Timezone empty
// means server-local time. Wake-ups inside the window are randomized.
type Proactive struct {
	Enabled            bool     `toml:"enabled"`
	Timezone           string   `toml:"timezone"`
	ActiveTime         string   `toml:"active_time"`
	MaxPerDay          int      `toml:"max_per_day"`
	Cooldown           string   `toml:"cooldown"`
	SkipRecentActivity string   `toml:"skip_recent_activity"`
	Model              string   `toml:"model"`
	Kinds              []string `toml:"kinds"`
	GroupPolicy        string   `toml:"group_policy"`
}

func Defaults() Config {
	return Config{StateDir: "~/.local/share/q-agent", Provider: Provider{Name: "ollama", Model: "qwen3"}, Telegram: Telegram{TokenEnv: "TELEGRAM_BOT_TOKEN"}, Sandbox: Sandbox{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", MMdebstrap: "mmdebstrap"}, Limits: Limits{MaxSteps: 8, MaxConcurrent: 2, TaskTimeout: "5m", InteractiveTaskTimeout: "15m", ShellTimeout: "2m", ShellPromptTimeout: "90s", ShellMaxExtends: 2}, Memory: Memory{Enabled: true, DigestAt: "00:00", MaxBytes: 8192, MaxContextBytes: 2000, MaxPendingBytes: 4 << 20}, Proactive: Proactive{Enabled: true, ActiveTime: "09:00-22:00", MaxPerDay: 1, Cooldown: "6h", SkipRecentActivity: "60m", Kinds: []string{"reminder", "warmth", "comfort", "suggestion"}, GroupPolicy: "conservative"}}
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
	// Deprecated allowlists migrate automatically: positive approved_user_ids
	// (plus orphan positive approved_chat_ids) become allowed_users, and
	// negative approved_chat_ids become allowed_groups. Mixing old and new
	// keys is rejected so the operator resolves the conflict explicitly.
	// Group-only members gain private access via this migration; trim
	// allowed_users afterwards if that is undesired.
	hasLegacy := len(c.Telegram.ApprovedUsers) > 0 || len(c.Telegram.ApprovedChats) > 0
	hasNew := len(c.Telegram.AllowedUsers) > 0 || len(c.Telegram.AllowedGroups) > 0
	if hasLegacy && hasNew {
		return c, fmt.Errorf("telegram.approved_user_ids/approved_chat_ids were replaced by telegram.allowed_users and telegram.allowed_groups; remove the old keys")
	}
	if hasLegacy {
		seen := map[int64]bool{}
		for _, id := range c.Telegram.ApprovedUsers {
			if id <= 0 {
				continue
			}
			if !seen[id] {
				seen[id] = true
				c.Telegram.AllowedUsers = append(c.Telegram.AllowedUsers, id)
			}
		}
		for _, id := range c.Telegram.ApprovedChats {
			if id > 0 {
				if !seen[id] {
					seen[id] = true
					c.Telegram.AllowedUsers = append(c.Telegram.AllowedUsers, id)
				}
			} else if id < 0 {
				c.Telegram.AllowedGroups = append(c.Telegram.AllowedGroups, id)
			}
		}
		c.Telegram.ApprovedUsers = nil
		c.Telegram.ApprovedChats = nil
	}
	for _, id := range c.Telegram.AllowedUsers {
		if id <= 0 {
			return c, fmt.Errorf("telegram.allowed_users must contain positive IDs")
		}
	}
	for _, id := range c.Telegram.AllowedGroups {
		if id >= 0 {
			return c, fmt.Errorf("telegram.allowed_groups must contain negative IDs")
		}
	}
	for _, admin := range c.Telegram.AdminUsers {
		approved := false
		for _, user := range c.Telegram.AllowedUsers {
			approved = approved || admin == user
		}
		if admin <= 0 || !approved {
			return c, fmt.Errorf("telegram.admin_user_ids must contain positive IDs from allowed_users")
		}
	}
	if c.Limits.MaxSteps < 1 || c.Limits.MaxConcurrent < 1 || c.Limits.MaxConcurrent > 2 {
		return c, fmt.Errorf("positive limits required; max_concurrent must be 1 or 2")
	}
	for _, s := range []string{c.Limits.TaskTimeout, c.Limits.ShellTimeout} {
		if d, e := time.ParseDuration(s); e != nil || d <= 0 {
			return c, fmt.Errorf("invalid positive duration %q", s)
		}
	}
	if c.Limits.InteractiveTaskTimeout != "" {
		if d, e := time.ParseDuration(c.Limits.InteractiveTaskTimeout); e != nil || d <= 0 {
			return c, fmt.Errorf("invalid limits.interactive_task_timeout")
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
	if c.Memory.MaxBytes > 1<<20 {
		return c, fmt.Errorf("memory.max_bytes must not exceed 1048576")
	}
	if c.Memory.MaxPendingBytes <= 0 {
		return c, fmt.Errorf("memory.max_pending_bytes must be positive")
	}
	if c.Memory.MaxPendingBytes > 64<<20 {
		return c, fmt.Errorf("memory.max_pending_bytes must not exceed 67108864")
	}
	if c.Memory.MaxContextBytes <= 0 {
		return c, fmt.Errorf("memory.max_context_bytes must be positive")
	}
	if c.Memory.MaxContextBytes > 200000 {
		return c, fmt.Errorf("memory.max_context_bytes must not exceed 200000")
	}
	if c.Proactive.ActiveTime == "" {
		c.Proactive.ActiveTime = Defaults().Proactive.ActiveTime
	}
	if _, _, err := parseActiveTime(c.Proactive.ActiveTime); err != nil {
		return c, err
	}
	if c.Proactive.Timezone != "" {
		if _, err := time.LoadLocation(c.Proactive.Timezone); err != nil {
			return c, fmt.Errorf("invalid proactive timezone: %w", err)
		}
	}
	if c.Proactive.Cooldown == "" {
		c.Proactive.Cooldown = "6h"
	}
	if d, e := time.ParseDuration(c.Proactive.Cooldown); e != nil || d <= 0 {
		return c, fmt.Errorf("invalid proactive.cooldown %q: want positive duration", c.Proactive.Cooldown)
	}
	if c.Proactive.SkipRecentActivity == "" {
		c.Proactive.SkipRecentActivity = "60m"
	}
	if d, e := time.ParseDuration(c.Proactive.SkipRecentActivity); e != nil || d <= 0 {
		return c, fmt.Errorf("invalid proactive.skip_recent_activity %q: want positive duration", c.Proactive.SkipRecentActivity)
	}
	if c.Proactive.MaxPerDay <= 0 {
		return c, fmt.Errorf("proactive.max_per_day must be positive")
	}
	if len(c.Proactive.Kinds) == 0 {
		c.Proactive.Kinds = Defaults().Proactive.Kinds
	}
	for _, k := range c.Proactive.Kinds {
		switch k {
		case "reminder", "warmth", "comfort", "suggestion":
		default:
			return c, fmt.Errorf("invalid proactive kind %q: want reminder|warmth|comfort|suggestion", k)
		}
	}
	if c.Proactive.GroupPolicy == "" {
		c.Proactive.GroupPolicy = "conservative"
	}
	switch c.Proactive.GroupPolicy {
	case "conservative", "off", "same":
	default:
		return c, fmt.Errorf("invalid proactive.group_policy %q: want conservative|off|same", c.Proactive.GroupPolicy)
	}
	c.StateDir, err = absolute(c.StateDir, filepath.Dir(path))
	if err != nil {
		return c, err
	}
	if c.ChatGPT.OAuthPath != "" {
		c.ChatGPT.OAuthPath, err = absolute(c.ChatGPT.OAuthPath, filepath.Dir(path))
		if err != nil {
			return c, err
		}
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

// parseActiveTime parses a daily active window HH:MM-HH:MM for the proactive
// companion. Start and end are minutes since midnight; equal values are
// rejected as an empty window. Overnight windows (end <= start, e.g.
// 22:00-02:00) are allowed and match evening plus after-midnight.
func parseActiveTime(s string) (startMin, endMin int, err error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: want HH:MM-HH:MM", s)
	}
	sh, sm, err := parseDigestAt(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: want HH:MM-HH:MM", s)
	}
	eh, em, err := parseDigestAt(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: want HH:MM-HH:MM", s)
	}
	startMin, endMin = sh*60+sm, eh*60+em
	if startMin == endMin {
		return 0, 0, fmt.Errorf("invalid proactive active_time %q: empty window", s)
	}
	return startMin, endMin, nil
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

// ValidProviderAdapter reports whether name selects a supported LLM adapter.
func ValidProviderAdapter(name string) bool {
	switch name {
	case "ollama", "openai", "opencode-go", "chatgpt":
		return true
	}
	return false
}

// validProviderField reports whether field is a settable providers.* field.
// The table name selects the adapter, so providers.<name>.name is rejected;
// use the table name itself.
func validProviderField(field string) bool {
	switch field {
	case "model", "base_url", "api_key_env", "api_key_path", "thinking":
		return true
	}
	return false
}

// ParseProvidersKey splits providers.<name>.<field> into its parts.
// It returns ok=false for non-providers keys. Provider names never contain
// dots, so the last dot separates the field.
func ParseProvidersKey(key string) (provider, field string, ok bool) {
	if !strings.HasPrefix(key, "providers.") {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, "providers.")
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	provider, field = rest[:i], rest[i+1:]
	if provider == "" || field == "" || strings.Contains(provider, ".") {
		return "", "", false
	}
	return provider, field, true
}

// IsValidKey reports whether key can be read or written via the console.
// Static keys come from Defaults(); dynamic providers.<name>.<field> keys are
// accepted for any supported adapter so new extras can be added.
func IsValidKey(key string) bool {
	if _, ok := Values(Defaults())[key]; ok {
		return true
	}
	if provider, field, ok := ParseProvidersKey(key); ok {
		return ValidProviderAdapter(provider) && validProviderField(field)
	}
	return false
}

func normalizeProvider(p *Provider, dir string) error {
	if !ValidProviderAdapter(p.Name) {
		return fmt.Errorf("unknown provider %q", p.Name)
	}
	if p.Model == "" {
		return fmt.Errorf("provider.model is required")
	}
	if err := normalizeThinking(p); err != nil {
		return err
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
	if p.Name == "chatgpt" && (p.APIKeyEnv != "" || p.APIKeyPath != "") {
		return fmt.Errorf("provider chatgpt uses ChatGPT OAuth (q-agent login chatgpt); clear api_key_env and api_key_path")
	}
	return nil
}

// normalizeThinking validates the shared thinking selector without per-model
// catalog metadata. Empty (auto) omits effort; "auto"/"default" are normalized
// to empty for backward compatibility. Client-only ultra/persistent/custom are
// rejected in v1, and any other value must be in the known cross-provider set.
// Per-model strict validation (known catalog) happens at /think time and at
// request time in Complete, where the exact account snapshot is available.
func normalizeThinking(p *Provider) error {
	raw := strings.TrimSpace(p.Thinking)
	if raw == "" {
		p.Thinking = ""
		return nil
	}
	norm := strings.ToLower(raw)
	if norm == "auto" || norm == "default" {
		p.Thinking = ""
		return nil
	}
	switch norm {
	case "ultra", "persistent", "custom":
		return fmt.Errorf("provider.thinking %q is not sendable in v1 (client-only)", p.Thinking)
	case "off", "on", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		p.Thinking = norm
		return nil
	}
	return fmt.Errorf("unknown thinking level %q", p.Thinking)
}

// InteractiveTaskDuration is the total budget from task start once a managed
// process is opened. The runtime preserves a longer ordinary task budget.
func (c Config) InteractiveTaskDuration() time.Duration {
	if d, err := time.ParseDuration(c.Limits.InteractiveTaskTimeout); err == nil && d > 0 {
		return d
	}
	return 15 * time.Minute
}
