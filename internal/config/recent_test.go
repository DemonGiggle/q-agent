package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestProactiveRecentDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "# Operator configuration\nstate_dir = 'state'\n[provider]\nname = \"ollama\"\nmodel = \"m\"\n[telegram]\nallowed_users = [1]\nallowed_groups = [-10]\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proactive.RecentWindow != "12h" || c.Proactive.RecentMaxMessages != 200 || c.Proactive.RecentMaxBytes != 32768 {
		t.Fatalf("recent defaults: %+v", c.Proactive)
	}
	if d, _ := time.ParseDuration(c.Proactive.RecentWindow); d != 12*time.Hour {
		t.Fatalf("window=%v", d)
	}
	for _, tc := range []struct{ name, text string }{
		{"recent window zero", "[proactive]\nrecent_window = '0s'"},
		{"recent window bad", "[proactive]\nrecent_window = 'forever'"},
		{"recent window too large", "[proactive]\nrecent_window = '200h'"},
		{"recent max messages low", "[proactive]\nrecent_max_messages = 5"},
		{"recent max messages high", "[proactive]\nrecent_max_messages = 5000"},
		{"recent max bytes low", "[proactive]\nrecent_max_bytes = 10"},
		{"recent max bytes high", "[proactive]\nrecent_max_bytes = 2097152"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, path, tc.text)
			if _, err := Load(path); err == nil {
				t.Fatal("invalid recent config accepted")
			}
		})
	}
	// Old configs without [proactive] backfill recent defaults (backward compat).
	writeConfig(t, path, "[provider]\nname = \"ollama\"\nmodel = \"m\"\n[telegram]\nallowed_users = [1]\n")
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proactive.RecentWindow != "12h" {
		t.Fatalf("backfill: %+v", c.Proactive)
	}
}
