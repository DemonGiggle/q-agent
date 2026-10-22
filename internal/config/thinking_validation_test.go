package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Review P2-2 (config path): client-only presets and typos must be rejected
// at Update/Load time, not silently stored. Per-model strict checks happen at
// /think and request time where the catalog snapshot is available.
func TestThinkingValidationRejectsClientOnlyAndTypo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	base := "state_dir = \"" + dir + "\"\n[provider]\nname = \"chatgpt\"\nmodel = \"m\"\n"
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	for _, lv := range []string{"ultra", "persistent", "custom", "ULTRA", "typo-level", "highe"} {
		if err := Update(path, map[string]any{"provider.thinking": lv}); err == nil {
			t.Fatalf("Update thinking %q must fail", lv)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load after failed Update: %v", err)
		}
		if cfg.Provider.Thinking != "" {
			t.Fatalf("failed Update must not persist, thinking=%q", cfg.Provider.Thinking)
		}
	}
	// Valid levels (including case-insensitive) pass; auto/default normalize to empty.
	for _, lv := range []string{"high", "High", " LOW ", "none", "minimal", "max", "off", "on"} {
		if err := Update(path, map[string]any{"provider.thinking": lv}); err != nil {
			t.Fatalf("Update thinking %q: %v", lv, err)
		}
	}
	for _, lv := range []string{"auto", "AUTO", "default", ""} {
		if err := Update(path, map[string]any{"provider.thinking": lv}); err != nil {
			t.Fatalf("Update thinking %q: %v", lv, err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Provider.Thinking != "" {
			t.Fatalf("thinking %q must normalize to auto empty, got %q", lv, cfg.Provider.Thinking)
		}
	}
	// Hand-edited file with client-only must fail Load.
	bad := base + "thinking = \"ultra\"\n"
	if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load with thinking ultra must fail")
	}
}
