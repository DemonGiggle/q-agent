package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, `# Operator configuration
state_dir = 'state'
[provider]
name = "opencode-go"
model = "model"
[telegram]
allowed_users = [123456789]
allowed_groups = [-1001234567890]
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.Enabled || c.Provider.APIKeyEnv != "OPENCODE_GO_API_KEY" || c.StateDir != filepath.Join(dir, "state") || c.Limits.MaxSteps != 8 || c.TaskDuration() != 5*time.Minute || c.InteractiveTaskDuration() != 15*time.Minute || c.ShellDuration() != 2*time.Minute {
		t.Fatalf("%+v", c)
	}
	if !c.Memory.Enabled || c.Memory.DigestAt != "00:00" || c.Memory.MaxBytes != 8192 || c.Memory.MaxContextBytes != 2000 || c.Memory.MaxPendingBytes != 4<<20 {
		t.Fatalf("memory defaults: %+v", c.Memory)
	}
	if len(c.Telegram.AllowedUsers) != 1 || len(c.Telegram.AllowedGroups) != 1 || c.Telegram.AllowedGroups[0] != -1001234567890 {
		t.Fatalf("allowlists: %+v", c.Telegram)
	}
	for _, tc := range []struct{ name, text string }{
		{"concurrency", "[limits]\nmax_concurrent = 3"},
		{"duration", "[limits]\ntask_timeout = '0s'"},
		{"interactive duration", "[limits]\ninteractive_task_timeout = '0s'"},
		{"interactive invalid duration", "[limits]\ninteractive_task_timeout = 'forever'"},
		{"unknown field", "unexpected = true"},
		{"unknown nested field", "[provider]\napi_key = 'not a configuration field'"},
		{"unapproved administrator", "[telegram]\nallowed_users = [1]\nadmin_user_ids = [2]"},
		{"memory digest_at", "[memory]\ndigest_at = '25:00'"},
		{"memory timezone", "[memory]\ntimezone = 'Nope/Nowhere'"},
		{"memory max_bytes", "[memory]\nmax_bytes = 0"},
		{"memory max_context_bytes", "[memory]\nmax_context_bytes = 0"},
		{"memory max_pending_bytes", "[memory]\nmax_pending_bytes = 0"},
		{"memory max_bytes cap", "[memory]\nmax_bytes = 2097152"},
		{"memory max_pending_bytes cap", "[memory]\nmax_pending_bytes = 134217728"},
		{"memory max_context_bytes cap", "[memory]\nmax_context_bytes = 300000"},
		{"nonpositive administrator", "[telegram]\nallowed_users = [0]\nadmin_user_ids = [0]"},
		{"duplicate key", "state_dir = 'one'\nstate_dir = 'two'"},
		{"invalid syntax", "[provider"},
		{"JSON", "{\"state_dir\":\"state\"}"},
		{"oversized", strings.Repeat("# comment\n", 110000) + "unexpected = true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, path, tc.text)
			if _, err := Load(path); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestExampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.Enabled || c.Provider.Name != "ollama" || c.Provider.Model != "qwen3" || c.Telegram.TokenEnv != "TELEGRAM_BOT_TOKEN" || c.Limits.MaxConcurrent != 2 || !filepath.IsAbs(c.Sandbox.BaseImage) {
		t.Fatalf("example config: %+v", c)
	}
}

func TestLegacyAllowlistMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = \"ollama\"\nmodel = \"m\"\n[telegram]\napproved_user_ids = [123456789]\napproved_chat_ids = [123456789, -1001234567890]\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Telegram.AllowedUsers) != 1 || c.Telegram.AllowedUsers[0] != 123456789 {
		t.Fatalf("migrated users: %+v", c.Telegram)
	}
	if len(c.Telegram.AllowedGroups) != 1 || c.Telegram.AllowedGroups[0] != -1001234567890 {
		t.Fatalf("migrated groups: %+v", c.Telegram)
	}
	if len(c.Telegram.ApprovedUsers) != 0 || len(c.Telegram.ApprovedChats) != 0 {
		t.Fatalf("legacy keys not cleared: %+v", c.Telegram)
	}
	// Mixing old and new keys is rejected.
	writeConfig(t, path, "[telegram]\nallowed_users = [1]\napproved_user_ids = [1]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("mixed old/new keys accepted")
	}
	// New keys validate sign.
	writeConfig(t, path, "[telegram]\nallowed_users = [0]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("nonpositive allowed_users accepted")
	}
	writeConfig(t, path, "[telegram]\nallowed_groups = [10]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("non-negative allowed_groups accepted")
	}
	// Legacy console keys are rejected with a migration hint.
	if _, err := ParseValue("telegram.approved_user_ids", "[1]"); err == nil {
		t.Fatal("legacy console key accepted")
	}
	if err := Update(path, map[string]any{"telegram.approved_chat_ids": []int64{-10}}); err == nil {
		t.Fatal("legacy console update accepted")
	}
}

func TestSandboxEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, text       string
		enabled, invalid bool
	}{
		{name: "omitted"},
		{name: "existing sandbox settings", text: "[sandbox]\nbase_image = 'missing.tar'\nworker = 'missing-worker'"},
		{name: "false", text: "[sandbox]\nenabled = false"},
		{name: "true", text: "[sandbox]\nenabled = true", enabled: true},
		{name: "string", text: "[sandbox]\nenabled = 'true'", invalid: true},
		{name: "number", text: "[sandbox]\nenabled = 1", invalid: true},
		{name: "array", text: "[sandbox]\nenabled = [true]", invalid: true},
		{name: "table", text: "[sandbox.enabled]\nvalue = true", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeConfig(t, path, tc.text)
			c, err := Load(path)
			if (err != nil) != tc.invalid || err == nil && c.Sandbox.Enabled != tc.enabled {
				t.Fatalf("enabled=%v err=%v", c.Sandbox.Enabled, err)
			}
		})
	}
}

func TestAdditionalProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, `[provider]
name = 'ollama'
model = 'local'
[providers.openai]
model = 'remote'
api_key_path = 'openai.key'
[providers.opencode-go]
model = 'other'
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.ProviderByName("openai")
	if !ok || p.Name != "openai" || p.Model != "remote" || p.APIKeyPath != filepath.Join(filepath.Dir(path), "openai.key") || p.APIKeyEnv != "" {
		t.Fatalf("profile: %+v", p)
	}
	p, ok = c.ProviderByName("opencode-go")
	if !ok || p.APIKeyEnv != "OPENCODE_GO_API_KEY" {
		t.Fatalf("credential default: %+v", p)
	}
	for _, text := range []string{
		"[providers.ollama]\nmodel='duplicate'",
		"[providers.unknown]\nmodel='invalid'",
		"[providers.openai]\nname='ollama'\nmodel='invalid'",
		"[providers.openai]\nmodel='invalid'\napi_key_env='KEY'\napi_key_path='key'",
		"[providers.openai]\nbase_url='https://example.test'",
	} {
		writeConfig(t, path, "[provider]\nname='ollama'\nmodel='local'\n"+text)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid profile: %s", text)
		}
	}
}
