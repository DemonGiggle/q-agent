package console

import (
	"strings"
	"testing"
)

func TestConfigOverrideSetGet(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "state_dir = 'state'\n[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n")
	_, _, err := execute(t, path, "", false, "config", "set", "memory.override.model", "digest-model")
	if err != nil {
		t.Fatalf("set override failed: %v", err)
	}
	out, _, err := execute(t, path, "", false, "config", "get", "memory.override.model", "--json")
	if err != nil || !strings.Contains(out, "digest-model") {
		t.Fatalf("get override failed: out=%q err=%v", out, err)
	}
	_, _, err = execute(t, path, "", false, "config", "set", "proactive.override.thinking", "low")
	if err != nil {
		t.Fatalf("set proactive thinking failed: %v", err)
	}
}

func TestConfigLegacyModelHint(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "state_dir = 'state'\n[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n")
	_, _, err := execute(t, path, "", false, "config", "set", "memory.model", "x")
	if err == nil || !strings.Contains(err.Error(), "memory.override.model") {
		t.Fatalf("legacy set missing hint: %v", err)
	}
	_, _, err = execute(t, path, "", false, "config", "get", "memory.model")
	if err == nil || !strings.Contains(err.Error(), "memory.override.model") {
		t.Fatalf("legacy get missing hint: %v", err)
	}
	_, _, err = execute(t, path, "", false, "config", "get", "model")
	if err == nil || !strings.Contains(err.Error(), "[provider]") {
		t.Fatalf("misplaced get missing hint: %v", err)
	}
}

func TestConfigCheckWarnsLegacyOverride(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[memory]\nmodel = 'digest-model'\n")
	out, _, err := execute(t, path, "", false, "config", "check")
	if err != nil {
		t.Fatalf("deprecated-only check should pass: %v", err)
	}
	if !strings.Contains(out, "memory.model is deprecated") {
		t.Fatalf("legacy warning missing: %q", out)
	}
}
