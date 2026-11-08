package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"q-agent/internal/sandbox"
	"q-agent/internal/workspace"
)

// CheckReport is the offline result of `q-agent config check`.
// Valid is true only when Errors is empty; Warnings never fail the check.
type CheckReport struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// Check loads path and reports misused settings as errors and deprecated
// settings as warnings. It never contacts the network, never prints secrets,
// and never writes the config file (load-time auto-migration still applies
// to the in-memory view via Load, but Check itself is read-only apart from
// LoadMigrated callers elsewhere).
//
// Errors cover what `serve` would refuse plus what `validate` already
// rejects:
//   - unreadable/oversized file, TOML syntax, unknown fields, invalid values
//   - empty telegram allowlists (nobody can talk to the bot)
//   - missing/unreadable Telegram or provider credentials (file permission
//     failures included; ollama may stay keyless, chatgpt uses OAuth)
//   - missing chatgpt OAuth file or file without an access token
//   - malformed provider base_url (must be http(s) with a host)
//   - state_dir pointing at a non-directory
//   - sandbox readiness (image checksum, helpers, subordinate IDs, worker)
//
// Warnings cover deprecated or ignored settings:
//   - limits.max_tool_calls (removed, ignored)
//   - telegram.approved_user_ids / approved_chat_ids (renamed)
//   - thinking "auto"/"default" (normalized to auto/empty)
//   - base_url userinfo/query/fragment (stripped from display, ignored)
//   - interactive_task_timeout shorter than task_timeout (longer still caps)
func Check(path string) CheckReport {
	rep := CheckReport{Errors: []string{}, Warnings: []string{}}
	abs, err := absolute(path, ".")
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return rep
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("cannot read configuration: %v", err))
		return rep
	}
	if len(data) > 1<<20 {
		rep.Errors = append(rep.Errors, "configuration exceeds 1 MiB")
		return rep
	}
	cfg, err := Load(abs)
	if err != nil {
		// Files carrying only migratable legacy keys (removed
		// limits.max_tool_calls, deprecated allowlists) fail strict Load
		// but succeed after planMigration. Retry against the migrated
		// candidate so `check` reports warnings, not errors, for those.
		if migrated, ok := migratedConfig(abs, data); ok {
			cfg = migrated
		} else {
			rep.Errors = append(rep.Errors, err.Error())
			rep.Warnings = append(rep.Warnings, deprecatedWarnings(data)...)
			rep.Valid = false
			return rep
		}
	}
	rep.Warnings = append(rep.Warnings, deprecatedWarnings(data)...)
	rep.Warnings = append(rep.Warnings, thinkingWarnings(data)...)

	if len(cfg.Telegram.AllowedUsers) == 0 && len(cfg.Telegram.AllowedGroups) == 0 {
		rep.Errors = append(rep.Errors, "telegram.allowed_users and/or telegram.allowed_groups are required (no one can talk to the bot)")
	}

	if token, err := cfg.Telegram.Token(); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	} else if token == "" {
		if cfg.Telegram.TokenPath != "" {
			rep.Errors = append(rep.Errors, fmt.Sprintf("missing Telegram credential file %s (empty)", cfg.Telegram.TokenPath))
		} else {
			rep.Errors = append(rep.Errors, fmt.Sprintf("missing Telegram credential environment variable %s (empty or unset)", cfg.Telegram.TokenEnv))
		}
	}

	providers := map[string]Provider{cfg.Provider.Name: cfg.Provider}
	for name, p := range cfg.Providers {
		providers[name] = p
	}
	for name, p := range providers {
		if p.BaseURL != "" {
			u, uerr := url.Parse(p.BaseURL)
			if uerr != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				rep.Errors = append(rep.Errors, fmt.Sprintf("provider.%s.base_url is invalid: want http(s) URL with a host", name))
			} else if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("provider.%s.base_url userinfo/query/fragment is ignored", name))
			}
		}
		if p.Name == "chatgpt" {
			if err := checkChatGPTOAuth(cfg.ChatGPT.OAuthPath); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("chatgpt OAuth for provider %s: %v", name, err))
			}
			continue
		}
		key, err := p.APIKey()
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("provider %s: %v", name, err))
			continue
		}
		if key == "" && p.Name != "ollama" {
			env := p.APIKeyEnv
			if env == "" {
				env = "(api_key_env)"
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("missing credential for provider %s (set %s or api_key_path)", name, env))
		}
	}

	if cfg.StateDir != "" {
		if fi, serr := os.Stat(cfg.StateDir); serr == nil {
			if !fi.IsDir() {
				rep.Errors = append(rep.Errors, fmt.Sprintf("state_dir %q is not a directory", cfg.StateDir))
			}
		} else if !os.IsNotExist(serr) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("state_dir %q: %v", cfg.StateDir, serr))
		}
	}

	if cfg.Sandbox.Enabled {
		rep.Errors = append(rep.Errors, sandboxReadiness(cfg)...)
	}

	if task, e1 := time.ParseDuration(cfg.Limits.TaskTimeout); e1 == nil {
		if inter, e2 := time.ParseDuration(cfg.Limits.InteractiveTaskTimeout); e2 == nil && inter < task {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("limits.interactive_task_timeout (%s) is shorter than limits.task_timeout (%s); the longer task_timeout still caps the run", cfg.Limits.InteractiveTaskTimeout, cfg.Limits.TaskTimeout))
		}
	}

	rep.Valid = len(rep.Errors) == 0
	return rep
}

// migratedConfig returns the post-migration view of data when the only load
// failures are migratable legacy keys. It reports ok=false when migration
// does not apply or the candidate still fails validation.
func migratedConfig(abs string, data []byte) (Config, bool) {
	candidate, _, needWrite, err := planMigration(data)
	if err != nil || !needWrite {
		return Config{}, false
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".q-agent-check-*")
	if err != nil {
		return Config{}, false
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(candidate); err != nil {
		tmp.Close()
		return Config{}, false
	}
	if err := tmp.Close(); err != nil {
		return Config{}, false
	}
	cfg, err := Load(name)
	if err != nil {
		return Config{}, false
	}
	return cfg, true
}

// deprecatedWarnings scans raw TOML for keys that load-time migration
// handles. scanKeys failure means the file does not parse, which Load
// already reports, so no warnings are added.
func deprecatedWarnings(data []byte) []string {
	occs, _, err := scanKeys(data)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range occs {
		switch o.key {
		case maxToolCallsKey:
			if !seen[o.key] {
				seen[o.key] = true
				out = append(out, "limits.max_tool_calls is removed and ignored; remove it from the file")
			}
		case "telegram.approved_user_ids", "telegram.approved_chat_ids":
			if !seen[o.key] {
				seen[o.key] = true
				out = append(out, fmt.Sprintf("%s is deprecated; use telegram.allowed_users and telegram.allowed_groups (auto-migrated on load)", o.key))
			}
		}
	}
	return out
}

// thinkingWarnings reports thinking = "auto"/"default", which normalizeThinking
// folds to auto (empty). Explicit auto works but is redundant; warn so the
// operator can use the canonical empty value.
func thinkingWarnings(data []byte) []string {
	occs, _, err := scanKeys(data)
	if err != nil {
		return nil
	}
	var out []string
	for _, o := range occs {
		if o.key != "provider.thinking" && !strings.HasPrefix(o.key, "providers.") {
			continue
		}
		if !strings.HasSuffix(o.key, ".thinking") {
			continue
		}
		raw := data[o.start:o.end]
		eq := strings.IndexByte(string(raw), '=')
		if eq < 0 {
			continue
		}
		var holder struct {
			Value string `toml:"value"`
		}
		if err := toml.Unmarshal(append([]byte("value = "), raw[eq+1:]...), &holder); err != nil {
			continue
		}
		norm := strings.ToLower(strings.TrimSpace(holder.Value))
		if norm == "auto" || norm == "default" {
			out = append(out, fmt.Sprintf("%s %q is normalized to auto (empty); use empty string to silence this warning", o.key, holder.Value))
		}
	}
	return out
}

// sandboxReadiness mirrors the local parts of serve startup for sandbox mode:
// image checksum plus helper/subordinate/worker resolution. Network and
// Telegram identity stay serve-time checks.
func sandboxReadiness(cfg Config) []string {
	var out []string
	if err := workspace.CheckImage(cfg.Sandbox.BaseImage); err != nil {
		out = append(out, fmt.Sprintf("sandbox.base_image: %v", err))
	}
	if _, err := sandbox.Resolve(sandbox.Options{
		Bubblewrap: cfg.Sandbox.Bubblewrap,
		NewUIDMap:  cfg.Sandbox.NewUIDMap,
		NewGIDMap:  cfg.Sandbox.NewGIDMap,
		Worker:     cfg.Sandbox.Worker,
	}); err != nil {
		out = append(out, fmt.Sprintf("sandbox: %v", err))
	}
	return out
}

// checkChatGPTOAuth validates the OAuth file offline: presence, regular file,
// owner-only mode, size bound, valid JSON with an access token. Expiry and
// refresh need the network and stay a serve-time check.
func checkChatGPTOAuth(configuredPath string) error {
	full := configuredPath
	if full == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot resolve default OAuth path: %w", err)
		}
		full = filepath.Join(home, ".config", "q-agent", "chatgpt-oauth.json")
	}
	link, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing OAuth file %s; run `q-agent login chatgpt`", full)
		}
		return err
	}
	if link.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("oauth file %q must not be a symlink", full)
	}
	if !link.Mode().IsRegular() {
		return fmt.Errorf("oauth file %q must be a regular file", full)
	}
	if perm := link.Mode().Perm(); perm != 0600 && perm != 0400 {
		return fmt.Errorf("oauth file %q must have mode 0600 or 0400 (has %04o)", full, perm)
	}
	if link.Size() > 64*1024 {
		return fmt.Errorf("oauth file %q exceeds 65536 bytes", full)
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return fmt.Errorf("oauth file %q is not valid JSON; run `q-agent login chatgpt` again", full)
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return fmt.Errorf("oauth file %q has no access token; run `q-agent login chatgpt` again", full)
	}
	return nil
}
