package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOverrideConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLegacyMemoryModelMigrates(t *testing.T) {
	path := writeOverrideConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory]\nmodel = 'digest-model'\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("legacy memory.model rejected: %v", err)
	}
	if c.Memory.Model != "" {
		t.Fatalf("legacy field not cleared: %q", c.Memory.Model)
	}
	if c.Memory.Override.Model != "digest-model" {
		t.Fatalf("override not populated: %+v", c.Memory.Override)
	}
}

func TestLegacyProactiveModelMigrates(t *testing.T) {
	path := writeOverrideConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[proactive]\nmodel = 'decider-model'\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("legacy proactive.model rejected: %v", err)
	}
	if c.Proactive.Override.Model != "decider-model" {
		t.Fatalf("override not populated: %+v", c.Proactive.Override)
	}
}

func TestLegacyAndOverrideConflict(t *testing.T) {
	for _, text := range []string{
		"[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory]\nmodel = 'old'\n[memory.override]\nmodel = 'new'\n",
		"[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[proactive]\nmodel = 'old'\n[proactive.override]\nprovider = 'ollama'\n",
	} {
		path := writeOverrideConfig(t, text)
		if _, err := Load(path); err == nil {
			t.Fatalf("conflicting legacy+override accepted: %s", text)
		}
	}
}

func TestOverrideProviderMustBeConfigured(t *testing.T) {
	path := writeOverrideConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory.override]\nprovider = 'openai'\nmodel = 'x'\n")
	if _, err := Load(path); err == nil {
		t.Fatal("unknown override provider accepted")
	}
	path = writeOverrideConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[providers.openai]\nmodel = 'x'\n[telegram]\nallowed_users = [1]\n[memory.override]\nprovider = 'openai'\nmodel = 'y'\nthinking = 'low'\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
}

func TestOverrideThinkingValidation(t *testing.T) {
	for _, tc := range []struct{ thinking, want string }{
		{"", ""},
		{"auto", ""},
		{"default", ""},
		{"low", "low"},
		{"HIGH", "high"},
	} {
		text := "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory.override]\nthinking = '" + tc.thinking + "'\n"
		path := writeOverrideConfig(t, text)
		c, err := Load(path)
		if err != nil {
			t.Fatalf("thinking %q rejected: %v", tc.thinking, err)
		}
		if c.Memory.Override.Thinking != tc.want {
			t.Fatalf("thinking %q normalized to %q, want %q", tc.thinking, c.Memory.Override.Thinking, tc.want)
		}
	}
	for _, thinking := range []string{"ultra", "bogus"} {
		text := "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory.override]\nthinking = '" + thinking + "'\n"
		if _, err := Load(writeOverrideConfig(t, text)); err == nil {
			t.Fatalf("thinking %q accepted", thinking)
		}
	}
}

func TestMisplacedTopLevelModelHint(t *testing.T) {
	path := writeOverrideConfig(t, "model = 'x'\n[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("top-level model accepted")
	}
	if !strings.Contains(err.Error(), "[provider]") || !strings.Contains(err.Error(), "model") {
		t.Fatalf("hint missing: %v", err)
	}
}

func TestMisplacedMemoryThinkingHint(t *testing.T) {
	path := writeOverrideConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory]\nthinking = 'low'\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("memory.thinking accepted")
	}
	if !strings.Contains(err.Error(), "override") {
		t.Fatalf("override hint missing: %v", err)
	}
}

func TestResolveModelOverride(t *testing.T) {
	c := Defaults()
	gotP, gotM, gotT := c.ResolveModelOverride(ModelOverride{}, "ollama", "chat-model", "high")
	if gotP != "ollama" || gotM != "chat-model" || gotT != "high" {
		t.Fatalf("empty override changed values: %s %s %s", gotP, gotM, gotT)
	}
	gotP, gotM, gotT = c.ResolveModelOverride(ModelOverride{Provider: "openai", Model: "o-model", Thinking: "low"}, "ollama", "chat-model", "high")
	if gotP != "openai" || gotM != "o-model" || gotT != "low" {
		t.Fatalf("override not applied: %s %s %s", gotP, gotM, gotT)
	}
	// Partial override keeps the rest.
	gotP, gotM, gotT = c.ResolveModelOverride(ModelOverride{Model: "o-model"}, "ollama", "chat-model", "high")
	if gotP != "ollama" || gotM != "o-model" || gotT != "high" {
		t.Fatalf("partial override wrong: %s %s %s", gotP, gotM, gotT)
	}
}

func TestUnknownKeyHint(t *testing.T) {
	if UnknownKeyHint("model") == "" {
		t.Fatal("no hint for top-level model")
	}
	if UnknownKeyHint("memory.model") == "" {
		t.Fatal("no hint for memory.model")
	}
	if UnknownKeyHint("provider.model") != "" {
		t.Fatalf("valid key got hint: %s", UnknownKeyHint("provider.model"))
	}
	if UnknownKeyHint("memory.override.model") != "" {
		t.Fatalf("valid override key got hint: %s", UnknownKeyHint("memory.override.model"))
	}
}
