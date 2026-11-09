package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	MCP       MCP                 `toml:"mcp"`
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
// ~/.config/q-agent/chatgpt-oauth.json. HostPath persists the stable
// ext_agent_host_id (urn:uuid:...); empty means the default
// ~/.config/q-agent/chatgpt-host-id. Neither field ever holds a secret itself.
type ChatGPT struct {
	OAuthPath string `toml:"oauth_path"`
	HostPath  string `toml:"host_path"`
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
	// Deprecated: flat model override at the section root. Use Override
	// (provider-specific) instead; e.g. [memory.override] with
	// provider/model/thinking. When both are set the file is rejected.
	Model string `toml:"model"`
	// Override selects the provider-specific model used for the digest.
	// Empty provider/model/thinking each fall back to the chat's own
	// provider, model, and thinking. Base URL and credentials always follow
	// the resolved provider's [provider]/[providers.*] entry.
	Override ModelOverride `toml:"override"`
}

// Proactive configures the opt-out companion check-in sweep. Enabled by
// default; Enabled=false is the single-flag kill switch (no goroutine, no
// background LLM/history reads, no queue writes). ActiveTime is the daily
// wall-clock window (HH:MM-HH:MM) the companion may wake in; Timezone empty
// means server-local time. Wake-ups inside the window are randomized.
//
// RecentWindow bounds the live-chat tap buffer the sweep reads in addition to
// addressed history+memory. Every human group message inside the window is
// kept (even without a mention) so the decider sees real chatter; messages
// older than the window expire and processed messages are consumed after a
// terminal sweep, so disk stays bounded. RecentMaxMessages/RecentMaxBytes cap
// each chat; oldest taps are evicted first.
type Proactive struct {
	Enabled            bool   `toml:"enabled"`
	Timezone           string `toml:"timezone"`
	ActiveTime         string `toml:"active_time"`
	MaxPerDay          int    `toml:"max_per_day"`
	Cooldown           string `toml:"cooldown"`
	SkipRecentActivity string `toml:"skip_recent_activity"`
	// Deprecated: flat model override at the section root. Use Override
	// (provider-specific) instead; e.g. [proactive.override] with
	// provider/model/thinking. When both are set the file is rejected.
	Model string `toml:"model"`
	// Override selects the provider-specific model used for the companion
	// decider. Empty provider/model/thinking each fall back to the chat's
	// own provider, model, and thinking. Base URL and credentials always
	// follow the resolved provider's [provider]/[providers.*] entry.
	Override          ModelOverride `toml:"override"`
	Kinds             []string      `toml:"kinds"`
	GroupPolicy       string        `toml:"group_policy"`
	RecentWindow      string        `toml:"recent_window"`
	RecentMaxMessages int           `toml:"recent_max_messages"`
	RecentMaxBytes    int           `toml:"recent_max_bytes"`
}

// ModelOverride is a provider-specific model selection for background tasks
// (memory digest, proactive decider). It never holds credentials or base URLs
// at this section root: those always come from the resolved provider's own
// [provider]/[providers.*] entry. Empty fields fall back to the chat's own
// provider, model, and thinking respectively.
type ModelOverride struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Thinking string `toml:"thinking"`
}

// MCP holds optional Model Context Protocol server configurations. Each entry
// in Servers is one named [mcp.servers.<name>] table. MCP stays fully
// inactive unless at least one server has enabled=true; no child process is
// spawned and no network connection is opened otherwise. Configuration edits
// apply at the next start (restart required); there is no live reload.
//
// This is the configuration-only foundation (issue #45). Task lifecycle,
// transports, discovery, execution, and runtime wiring arrive in later issues
// (#46 and up) and build on these types without changing their TOML shape.
// Credential fields, custom headers, OAuth, and environment-secret injection
// are intentionally excluded: servers are credential-free stdio commands or
// Streamable HTTP endpoints (HTTPS anywhere, HTTP on loopback only).
type MCP struct {
	Servers map[string]MCPServer `toml:"servers"`
}

// MCPServer is one named [mcp.servers.<name>] table.
//
//   - transport "stdio": spawns command with optional args. command is
//     required; url must be empty.
//   - transport "http": Streamable HTTP endpoint at url. url is required;
//     command and args must be empty.
//
// connect_timeout bounds initial setup per task (default "10s"); call_timeout
// bounds each tool call (default "60s"). Both are positive durations up to
// "10m". Empty values load as their defaults.
type MCPServer struct {
	Enabled        bool     `toml:"enabled"`
	Transport      string   `toml:"transport"`
	Command        string   `toml:"command"`
	Args           []string `toml:"args"`
	URL            string   `toml:"url"`
	ConnectTimeout string   `toml:"connect_timeout"`
	CallTimeout    string   `toml:"call_timeout"`
}

// Timeout defaults applied when connect_timeout/call_timeout are empty.
// Documented in config.example.toml and internal/mcpclient.
const (
	// DefaultMCPConnectTimeout bounds MCP setup per task when
	// mcp.servers.<name>.connect_timeout is empty.
	DefaultMCPConnectTimeout = "10s"
	// DefaultMCPCallTimeout bounds each MCP tool call when
	// mcp.servers.<name>.call_timeout is empty.
	DefaultMCPCallTimeout = "60s"
)

// maxMCPTimeout caps connect_timeout and call_timeout (both must be positive
// durations not exceeding this bound).
const maxMCPTimeout = 10 * time.Minute

// mcpServerNameRe constrains server names so they round-trip through TOML
// tables, console keys, and diagnostics without ambiguity.
var mcpServerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// validMCPServerName reports whether name may select an [mcp.servers.<name>]
// table: 1-64 chars, letters/digits/_/-, starting with a letter or digit
// (dots are rejected so table names never nest).
func validMCPServerName(name string) bool {
	return mcpServerNameRe.MatchString(name)
}

// validMCPField reports whether field is a settable mcp.servers.<name> field.
func validMCPField(field string) bool {
	switch field {
	case "enabled", "transport", "command", "args", "url",
		"connect_timeout", "call_timeout":
		return true
	}
	return false
}

// ParseMCPServersKey splits mcp.servers.<name>.<field> into its parts. It
// returns ok=false for non-MCP keys. Server names never contain dots, so the
// last dot separates the field and the middle segment is the name.
func ParseMCPServersKey(key string) (server, field string, ok bool) {
	if !strings.HasPrefix(key, "mcp.servers.") {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, "mcp.servers.")
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	server, field = rest[:i], rest[i+1:]
	if server == "" || field == "" || strings.Contains(server, ".") {
		return "", "", false
	}
	return server, field, true
}

// MCPActive reports whether at least one MCP server is enabled. With no
// enabled servers MCP performs no startup, spawns no child process, and opens
// no network connection.
func (c Config) MCPActive() bool {
	for _, s := range c.MCP.Servers {
		if s.Enabled {
			return true
		}
	}
	return false
}

// EnabledMCPServers returns the enabled subset of [mcp.servers.*] entries.
func (c Config) EnabledMCPServers() map[string]MCPServer {
	out := map[string]MCPServer{}
	for name, s := range c.MCP.Servers {
		if s.Enabled {
			out[name] = s
		}
	}
	return out
}

// ConnectTimeoutDuration parses the effective connection/setup timeout,
// falling back to DefaultMCPConnectTimeout when empty or invalid.
func (s MCPServer) ConnectTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(s.ConnectTimeout); err == nil && d > 0 {
		return d
	}
	d, _ := time.ParseDuration(DefaultMCPConnectTimeout)
	return d
}

// CallTimeoutDuration parses the effective per-call timeout, falling back to
// DefaultMCPCallTimeout when empty or invalid.
func (s MCPServer) CallTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(s.CallTimeout); err == nil && d > 0 {
		return d
	}
	d, _ := time.ParseDuration(DefaultMCPCallTimeout)
	return d
}

// normalizeMCP validates every [mcp.servers.<name>] table and fills timeout
// defaults. All errors identify the server and field.
func normalizeMCP(c *Config) error {
	for _, name := range sortedMCPServerNames(c.MCP.Servers) {
		s := c.MCP.Servers[name]
		if !validMCPServerName(name) {
			return fmt.Errorf("mcp.servers.%q: invalid server name (want 1-64 chars of letters/digits/_/-, starting with a letter or digit)", name)
		}
		switch s.Transport {
		case "stdio":
			if strings.TrimSpace(s.Command) == "" {
				return fmt.Errorf("mcp.servers.%q.command is required for stdio transport", name)
			}
			if s.URL != "" {
				return fmt.Errorf("mcp.servers.%q.url must be empty for stdio transport", name)
			}
		case "http":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp.servers.%q.url is required for http transport", name)
			}
			if s.Command != "" {
				return fmt.Errorf("mcp.servers.%q.command must be empty for http transport", name)
			}
			if len(s.Args) != 0 {
				return fmt.Errorf("mcp.servers.%q.args must be empty for http transport", name)
			}
			if err := validateMCPServerURL(name, s.URL); err != nil {
				return err
			}
		case "":
			return fmt.Errorf("mcp.servers.%q.transport is required (want stdio|http)", name)
		default:
			return fmt.Errorf("mcp.servers.%q.transport %q is invalid (want stdio|http)", name, s.Transport)
		}
		if s.ConnectTimeout == "" {
			s.ConnectTimeout = DefaultMCPConnectTimeout
		}
		if d, err := time.ParseDuration(s.ConnectTimeout); err != nil || d <= 0 || d > maxMCPTimeout {
			return fmt.Errorf("mcp.servers.%q.connect_timeout is invalid: want positive duration up to 10m (got %q)", name, s.ConnectTimeout)
		}
		if s.CallTimeout == "" {
			s.CallTimeout = DefaultMCPCallTimeout
		}
		if d, err := time.ParseDuration(s.CallTimeout); err != nil || d <= 0 || d > maxMCPTimeout {
			return fmt.Errorf("mcp.servers.%q.call_timeout is invalid: want positive duration up to 10m (got %q)", name, s.CallTimeout)
		}
		c.MCP.Servers[name] = s
	}
	return nil
}

// sortedMCPServerNames returns server names in sorted order so validation
// errors are deterministic.
func sortedMCPServerNames(servers map[string]MCPServer) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// validateMCPServerURL enforces the credential-free endpoint policy: HTTPS
// anywhere, HTTP on loopback only, never with userinfo (credentials).
func validateMCPServerURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("mcp.servers.%q.url is invalid: want http(s) URL with a host", name)
	}
	if u.User != nil {
		return fmt.Errorf("mcp.servers.%q.url must not contain credentials", name)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isMCPLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("mcp.servers.%q.url: plain http is only allowed for loopback (127.0.0.0/8, ::1, localhost); use https", name)
	default:
		return fmt.Errorf("mcp.servers.%q.url is invalid: want http(s) URL with a host", name)
	}
}

// isMCPLoopbackHost reports whether host is a loopback address for the
// plain-http exception: 127.0.0.0/8, ::1, or localhost.
func isMCPLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// misplacedProviderHint explains where model/thinking/base_url and
// credentials belong when a dotted key puts them outside provider settings.
// It reports ok=false for keys that are not provider fields in the wrong place.
func misplacedProviderHint(key string) (hint string, ok bool) {
	providerFields := map[string]bool{
		"model": true, "thinking": true, "base_url": true,
		"api_key_env": true, "api_key_path": true, "name": true,
	}
	// Top-level fields without a section: model/thinking/... belong under
	// [provider] or [providers.<name>].
	if providerFields[key] {
		return "model, thinking, base_url and credentials belong under [provider] or [providers.<name>], not at the document root", true
	}
	// Bare [providers] without a name.
	if key == "providers" || strings.HasPrefix(key, "providers.") {
		if _, _, pok := ParseProvidersKey(key); !pok {
			return "use providers.<name>.model/thinking/base_url/api_key_env/api_key_path with a concrete provider name", true
		}
		return "", false
	}
	// MCP tables are dynamic like providers; unknown mcp.* keys are reported
	// as unknown MCP keys, never as misplaced provider fields.
	if key == "mcp" || strings.HasPrefix(key, "mcp.") {
		return "", false
	}
	// Provider fields nested under non-provider sections.
	if i := strings.Index(key, "."); i > 0 {
		section, field := key[:i], key[i+1:]
		// Allow the canonical provider tables and the new override tables.
		if section == "provider" || section == "providers" {
			return "", false
		}
		if (section == "memory" || section == "proactive") &&
			strings.HasPrefix(field, "override.") {
			return "", false
		}
		if providerFields[field] ||
			strings.HasSuffix(field, ".model") || strings.HasSuffix(field, ".thinking") ||
			strings.HasSuffix(field, ".base_url") || strings.HasSuffix(field, ".api_key_env") ||
			strings.HasSuffix(field, ".api_key_path") {
			return "model, thinking, base_url and credentials belong under [provider] or [providers.<name>]; background tasks use [memory.override]/[proactive.override] with provider/model/thinking", true
		}
		// memory.thinking / proactive.base_url style: field without override prefix.
		if (section == "memory" || section == "proactive") && providerFields[strings.TrimPrefix(field, "override.")] {
			return "use [memory.override]/[proactive.override] with provider/model/thinking; base URL and credentials follow the resolved provider entry", true
		}
	}
	return "", false
}

func Defaults() Config {
	return Config{StateDir: "~/.local/share/q-agent", Provider: Provider{Name: "ollama", Model: "qwen3"}, Telegram: Telegram{TokenEnv: "TELEGRAM_BOT_TOKEN"}, Sandbox: Sandbox{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", MMdebstrap: "mmdebstrap"}, Limits: Limits{MaxSteps: 8, MaxConcurrent: 2, TaskTimeout: "5m", InteractiveTaskTimeout: "15m", ShellTimeout: "2m", ShellPromptTimeout: "90s", ShellMaxExtends: 2}, Memory: Memory{Enabled: true, DigestAt: "00:00", MaxBytes: 32768, MaxContextBytes: 6000, MaxPendingBytes: 4 << 20}, Proactive: Proactive{Enabled: true, ActiveTime: "09:00-22:00", MaxPerDay: 1, Cooldown: "6h", SkipRecentActivity: "60m", Kinds: []string{"reminder", "warmth", "comfort", "suggestion"}, GroupPolicy: "conservative", RecentWindow: "12h", RecentMaxMessages: 200, RecentMaxBytes: 32768}}
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
		if hint := misplacedHintFromData(data); hint != "" {
			return c, fmt.Errorf("%w; %s", err, hint)
		}
		if hint := mcpHintFromData(data); hint != "" {
			return c, fmt.Errorf("%w; %s", err, hint)
		}
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
	if err := migrateLegacyModelOverride("memory", &c.Memory.Model, &c.Memory.Override, c); err != nil {
		return c, err
	}
	if err := validateModelOverride("memory.override", &c.Memory.Override, c); err != nil {
		return c, err
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
	if c.Proactive.RecentWindow == "" {
		c.Proactive.RecentWindow = Defaults().Proactive.RecentWindow
	}
	if d, e := time.ParseDuration(c.Proactive.RecentWindow); e != nil || d <= 0 || d > 7*24*time.Hour {
		return c, fmt.Errorf("invalid proactive.recent_window %q: want positive duration up to 168h", c.Proactive.RecentWindow)
	}
	if c.Proactive.RecentMaxMessages == 0 {
		c.Proactive.RecentMaxMessages = Defaults().Proactive.RecentMaxMessages
	}
	if c.Proactive.RecentMaxMessages < 10 || c.Proactive.RecentMaxMessages > 1000 {
		return c, fmt.Errorf("invalid proactive.recent_max_messages %d: want 10-1000", c.Proactive.RecentMaxMessages)
	}
	if c.Proactive.RecentMaxBytes == 0 {
		c.Proactive.RecentMaxBytes = Defaults().Proactive.RecentMaxBytes
	}
	if c.Proactive.RecentMaxBytes < 1024 || c.Proactive.RecentMaxBytes > 1<<20 {
		return c, fmt.Errorf("invalid proactive.recent_max_bytes %d: want 1024-1048576", c.Proactive.RecentMaxBytes)
	}
	if err := migrateLegacyModelOverride("proactive", &c.Proactive.Model, &c.Proactive.Override, c); err != nil {
		return c, err
	}
	if err := validateModelOverride("proactive.override", &c.Proactive.Override, c); err != nil {
		return c, err
	}
	if err := normalizeMCP(&c); err != nil {
		return c, err
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
	if c.ChatGPT.HostPath != "" {
		c.ChatGPT.HostPath, err = absolute(c.ChatGPT.HostPath, filepath.Dir(path))
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
	if server, field, ok := ParseMCPServersKey(key); ok {
		return validMCPServerName(server) && validMCPField(field)
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

// misplacedHintFromData scans raw TOML keys and returns a placement hint when
// provider fields sit outside provider settings. It returns "" when the file
// does not parse or nothing looks misplaced.
func misplacedHintFromData(data []byte) string {
	occs, _, err := scanKeys(data)
	if err != nil {
		return ""
	}
	for _, o := range occs {
		if hint, ok := misplacedProviderHint(o.key); ok {
			return hint + " (offending key: " + o.key + ")"
		}
	}
	return ""
}

// mcpHintFromData scans raw TOML keys and explains unknown or malformed
// mcp.servers keys when strict decoding rejects them before validation runs.
// It returns "" when nothing looks MCP-shaped.
func mcpHintFromData(data []byte) string {
	occs, _, err := scanKeys(data)
	if err != nil {
		return ""
	}
	for _, o := range occs {
		if server, field, ok := ParseMCPServersKey(o.key); ok {
			if !validMCPServerName(server) {
				return fmt.Sprintf("invalid MCP server name %q (offending key: %s)", server, o.key)
			}
			if !validMCPField(field) {
				return fmt.Sprintf("unknown configuration key %q; want mcp.servers.<name>.enabled/transport/command/args/url/connect_timeout/call_timeout", o.key)
			}
			continue
		}
		if o.key == "mcp" || strings.HasPrefix(o.key, "mcp.") {
			return fmt.Sprintf("unknown configuration key %q; use mcp.servers.<name>.enabled/transport/command/args/url/connect_timeout/call_timeout with a concrete server name", o.key)
		}
	}
	return ""
}

// migrateLegacyModelOverride folds the deprecated flat model string at a
// section root into its provider-specific override table. When both are set
// the file is rejected so the operator resolves the conflict explicitly.
func migrateLegacyModelOverride(section string, legacy *string, override *ModelOverride, c Config) error {
	if strings.TrimSpace(*legacy) == "" {
		return nil
	}
	if override.Provider != "" || override.Model != "" || override.Thinking != "" {
		return fmt.Errorf("%s.model and %s.override cannot both be set; use %s.override.provider/model/thinking", section, section, section)
	}
	override.Model = *legacy
	*legacy = ""
	_ = c
	return nil
}

// validateModelOverride checks a background-task override. Empty means "use
// the chat's own settings". A named provider must already be configured
// (default [provider] or an extra [providers.*]); base URL and credentials
// always follow that provider entry and are never set here.
func validateModelOverride(label string, o *ModelOverride, c Config) error {
	if o.Provider == "" && o.Model == "" && o.Thinking == "" {
		return nil
	}
	if o.Provider != "" {
		if _, ok := c.ProviderByName(o.Provider); !ok {
			return fmt.Errorf("invalid %s.provider %q: provider is not configured (default [provider] or [providers.*])", label, o.Provider)
		}
	}
	if o.Model != "" {
		if len(o.Model) > 256 || strings.TrimSpace(o.Model) != o.Model {
			return fmt.Errorf("invalid %s.model %q: want 1-256 chars without surrounding whitespace", label, o.Model)
		}
		for _, r := range o.Model {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("invalid %s.model %q: must not contain control characters", label, o.Model)
			}
		}
	}
	raw := strings.TrimSpace(o.Thinking)
	if raw == "" {
		o.Thinking = ""
		return nil
	}
	norm := strings.ToLower(raw)
	if norm == "auto" || norm == "default" {
		o.Thinking = ""
		return nil
	}
	switch norm {
	case "ultra", "persistent", "custom":
		return fmt.Errorf("%s.thinking %q is not sendable in v1 (client-only)", label, o.Thinking)
	case "off", "on", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		o.Thinking = norm
		return nil
	}
	return fmt.Errorf("unknown thinking level %q", o.Thinking)
}

// ResolveModelOverride applies a provider-specific background-task override.
// Empty override fields fall back to the chat's own provider, model, and
// thinking. Base URL and credentials always follow the resolved provider's
// [provider]/[providers.*] entry.
func (c Config) ResolveModelOverride(o ModelOverride, provider, model, thinking string) (string, string, string) {
	if o.Provider != "" {
		provider = o.Provider
	}
	if o.Model != "" {
		model = o.Model
	}
	if o.Thinking != "" {
		thinking = o.Thinking
	}
	return provider, model, thinking
}

// InteractiveTaskDuration is the total budget from task start once a managed
// process is opened. The runtime preserves a longer ordinary task budget.
func (c Config) InteractiveTaskDuration() time.Duration {
	if d, err := time.ParseDuration(c.Limits.InteractiveTaskTimeout); err == nil && d > 0 {
		return d
	}
	return 15 * time.Minute
}
