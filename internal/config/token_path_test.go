package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	// Omitted token_env defaults to TELEGRAM_BOT_TOKEN.
	writeConfig(t, configPath, "[provider]\nname = \"ollama\"\nmodel = \"m\"\n[telegram]\nallowed_users = [1]\n")
	c, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.TokenEnv != "TELEGRAM_BOT_TOKEN" || c.Telegram.TokenPath != "" {
		t.Fatalf("default token source: %+v", c.Telegram)
	}

	// Mutual exclusivity.
	writeConfig(t, configPath, "[provider]\nname = \"ollama\"\nmodel = \"m\"\n[telegram]\ntoken_env = \"FOO\"\ntoken_path = \"tok\"\nallowed_users = [1]\n")
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both telegram sources accepted: %v", err)
	}

	// Path resolution and trimming.
	tokenPath := filepath.Join(dir, "telegram.token")
	writeKeyFile(t, tokenPath, "  file-token-value \n", 0600)
	writeConfig(t, configPath, "[provider]\nname = \"ollama\"\nmodel = \"m\"\n[telegram]\ntoken_env = \"\"\ntoken_path = \"telegram.token\"\nallowed_users = [1]\n")
	c, err = Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Telegram.TokenPath) || c.Telegram.TokenPath != tokenPath {
		t.Fatalf("unresolved token path: %q", c.Telegram.TokenPath)
	}
	if c.Telegram.TokenEnv != "" {
		t.Fatalf("env should stay empty when path is configured: %q", c.Telegram.TokenEnv)
	}
	got, err := c.Telegram.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-token-value" {
		t.Fatalf("trimmed token = %q", got)
	}

	t.Run("insecure mode rejected", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "tok")
		writeKeyFile(t, p, "x", 0600)
		if err := os.Chmod(p, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := (Telegram{TokenPath: p}).Token(); err == nil {
			t.Fatal("insecure mode accepted")
		}
	})

	t.Run("symlink rejected", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real")
		writeKeyFile(t, target, "secret", 0600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := (Telegram{TokenPath: link}).Token(); err == nil {
			t.Fatal("symlink accepted")
		}
	})

	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("Q_AGENT_TEST_TELEGRAM_TOKEN", "env-token-value")
		got, err := (Telegram{TokenEnv: "Q_AGENT_TEST_TELEGRAM_TOKEN"}).Token()
		if err != nil || got != "env-token-value" {
			t.Fatalf("env token = %q err = %v", got, err)
		}
	})
}
