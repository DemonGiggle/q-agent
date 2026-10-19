package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKeyFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestAPIKeyPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	writeConfig(t, configPath, "[provider]\nname = \"openai\"\nmodel = \"m\"\napi_key_env = \"FOO\"\napi_key_path = \"key\"\n")
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both credential sources accepted: %v", err)
	}
	keyPath := filepath.Join(dir, "provider.key")
	writeKeyFile(t, keyPath, "  file-secret-value \n", 0600)
	writeConfig(t, configPath, "[provider]\nname = \"openai\"\nmodel = \"m\"\napi_key_env = \"\"\napi_key_path = \"provider.key\"\n[telegram]\nallowed_users = [1]\nallowed_groups = [-10]\n")
	c, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Provider.APIKeyPath) || c.Provider.APIKeyPath != keyPath {
		t.Fatalf("unresolved key path: %q", c.Provider.APIKeyPath)
	}
	if c.Provider.APIKeyEnv != "" {
		t.Fatalf("env should stay empty when path is configured: %q", c.Provider.APIKeyEnv)
	}
	got, err := c.Provider.APIKey()
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-secret-value" {
		t.Fatalf("trimmed key = %q", got)
	}
	writeKeyFile(t, keyPath, "ro-secret", 0400)
	if got, err := c.Provider.APIKey(); err != nil || got != "ro-secret" {
		t.Fatalf("0400 key = %q err = %v", got, err)
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"group readable", 0640},
		{"world readable", 0644},
		{"owner execute", 0700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "k")
			writeKeyFile(t, p, "x", 0600)
			if err := os.Chmod(p, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := (Provider{APIKeyPath: p}).APIKey(); err == nil {
				t.Fatalf("insecure mode %04o accepted", tc.mode)
			}
		})
	}
	t.Run("symlink rejected", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real")
		writeKeyFile(t, target, "secret", 0600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := (Provider{APIKeyPath: link}).APIKey(); err == nil {
			t.Fatal("symlink accepted")
		}
	})
	t.Run("directory rejected", func(t *testing.T) {
		d := t.TempDir()
		if _, err := (Provider{APIKeyPath: d}).APIKey(); err == nil {
			t.Fatal("directory accepted")
		}
	})
	t.Run("missing rejected", func(t *testing.T) {
		if _, err := (Provider{APIKeyPath: filepath.Join(t.TempDir(), "absent")}).APIKey(); err == nil {
			t.Fatal("missing file accepted")
		}
	})
	t.Run("oversized rejected", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "big")
		writeKeyFile(t, p, strings.Repeat("k", 64*1024+1), 0600)
		if _, err := (Provider{APIKeyPath: p}).APIKey(); err == nil {
			t.Fatal("oversized file accepted")
		}
	})
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("Q_AGENT_TEST_API_KEY", "env-secret-value")
		p := Provider{APIKeyEnv: "Q_AGENT_TEST_API_KEY"}
		got, err := p.APIKey()
		if err != nil || got != "env-secret-value" {
			t.Fatalf("env key = %q err = %v", got, err)
		}
		if got, err := (Provider{}).APIKey(); err != nil || got != "" {
			t.Fatalf("empty source = %q err = %v", got, err)
		}
	})
}
