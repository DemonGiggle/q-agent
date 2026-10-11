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
approved_user_ids = [123456789]
approved_chat_ids = [123456789, -1001234567890]
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.Enabled || c.Provider.APIKeyEnv != "OPENCODE_GO_API_KEY" || c.StateDir != filepath.Join(dir, "state") || c.Limits.MaxSteps != 8 || c.TaskDuration() != 5*time.Minute || c.ShellDuration() != 2*time.Minute {
		t.Fatalf("%+v", c)
	}
	if len(c.Telegram.ApprovedUsers) != 1 || len(c.Telegram.ApprovedChats) != 2 || c.Telegram.ApprovedChats[1] != -1001234567890 {
		t.Fatalf("allowlists: %+v", c.Telegram)
	}
	for _, tc := range []struct{ name, text string }{
		{"concurrency", "[limits]\nmax_concurrent = 3"},
		{"duration", "[limits]\ntask_timeout = '0s'"},
		{"unknown field", "unexpected = true"},
		{"unknown nested field", "[provider]\napi_key = 'not a configuration field'"},
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
