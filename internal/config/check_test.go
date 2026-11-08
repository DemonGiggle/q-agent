package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCheckConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckValid(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "state_dir = 'state'\n[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [123456789]\n")
	rep := Check(path)
	if !rep.Valid || len(rep.Errors) != 0 {
		t.Fatalf("valid config reported errors: %+v", rep)
	}
	if len(rep.Warnings) != 0 {
		t.Fatalf("valid config reported warnings: %+v", rep)
	}
}

func TestCheckEmptyAllowlistIsError(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("empty allowlist accepted: %+v", rep)
	}
	found := false
	for _, e := range rep.Errors {
		if strings.Contains(e, "allowed_users") {
			found = true
		}
	}
	if !found {
		t.Fatalf("allowlist error missing: %+v", rep)
	}
}

func TestCheckMissingTelegramCredential(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("missing telegram credential accepted: %+v", rep)
	}
}

func TestCheckMissingProviderCredential(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	t.Setenv("OPENAI_API_KEY", "")
	path := writeCheckConfig(t, "[provider]\nname = 'openai'\nmodel = 'gpt'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("missing provider credential accepted: %+v", rep)
	}
}

func TestCheckBadBaseURL(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\nbase_url = 'not-a-url'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("bad base_url accepted: %+v", rep)
	}
}

func TestCheckBaseURLUserinfoWarns(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\nbase_url = 'http://user:pass@example.com/v1'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if !rep.Valid {
		t.Fatalf("userinfo base_url should not fail: %+v", rep)
	}
	if len(rep.Warnings) == 0 {
		t.Fatalf("userinfo base_url warning missing: %+v", rep)
	}
}

func TestCheckDeprecatedMaxToolCallsWarns(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[limits]\nmax_tool_calls = 5\n")
	rep := Check(path)
	if !rep.Valid {
		t.Fatalf("removed max_tool_calls should warn, not fail: %+v", rep)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "max_tool_calls") {
			found = true
		}
	}
	if !found {
		t.Fatalf("max_tool_calls warning missing: %+v", rep)
	}
}

func TestCheckDeprecatedAllowlistWarns(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\napproved_user_ids = [123]\n")
	rep := Check(path)
	if !rep.Valid {
		t.Fatalf("deprecated allowlist should warn, not fail: %+v", rep)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "approved_user_ids") {
			found = true
		}
	}
	if !found {
		t.Fatalf("approved_* warning missing: %+v", rep)
	}
}

func TestCheckThinkingAutoWarns(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\nthinking = 'auto'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if !rep.Valid {
		t.Fatalf("thinking auto should warn, not fail: %+v", rep)
	}
	if len(rep.Warnings) == 0 {
		t.Fatalf("thinking warning missing: %+v", rep)
	}
}

func TestCheckMixedAllowlistIsError(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\napproved_user_ids = [1]\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("mixed allowlists accepted: %+v", rep)
	}
}

func TestCheckUnknownFieldIsError(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[limits]\nbogus_key = 1\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("unknown field accepted: %+v", rep)
	}
}

func TestCheckInteractiveShorterThanTaskWarns(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[limits]\ntask_timeout = '10m'\ninteractive_task_timeout = '5m'\n")
	rep := Check(path)
	if !rep.Valid {
		t.Fatalf("shorter interactive timeout should warn, not fail: %+v", rep)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "interactive_task_timeout") {
			found = true
		}
	}
	if !found {
		t.Fatalf("interactive timeout warning missing: %+v", rep)
	}
}

func TestCheckStateDirFileIsError(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("state_dir = '"+file+"'\n[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("state_dir file accepted: %+v", rep)
	}
}

func TestCheckChatGPTMissingOAuthIsError(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeCheckConfig(t, "[provider]\nname = 'chatgpt'\nmodel = 'gpt-6-sol'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if rep.Valid {
		t.Fatalf("missing chatgpt oauth accepted: %+v", rep)
	}
}

func TestCheckChatGPTOAuthPresentPasses(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	home := t.TempDir()
	t.Setenv("HOME", home)
	oauthDir := filepath.Join(home, ".config", "q-agent")
	if err := os.MkdirAll(oauthDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oauthDir, "chatgpt-oauth.json"), []byte(`{"access_token":"tok","refresh_token":"ref"}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := writeCheckConfig(t, "[provider]\nname = 'chatgpt'\nmodel = 'gpt-6-sol'\n[telegram]\nallowed_users = [1]\n")
	rep := Check(path)
	if !rep.Valid {
		t.Fatalf("valid chatgpt oauth reported errors: %+v", rep)
	}
}

func TestCheckNoSecretsInReport(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "env-token-value")
	secret := "super-secret-provider-key"
	t.Setenv("TEST_CHECK_KEY", secret)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "provider.key")
	if err := os.WriteFile(keyPath, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	text := "[provider]\nname = 'openai'\nmodel = 'gpt'\napi_key_path = '" + keyPath + "'\n[telegram]\nallowed_users = [1]\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	// Force an error so the report is non-empty (empty allowlist would do,
	// but here we use a bad base_url to keep allowlist valid).
	if err := os.WriteFile(path, []byte(text+"[nope]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rep := Check(path)
	joined := strings.Join(append(append([]string{}, rep.Errors...), rep.Warnings...), "\n")
	if strings.Contains(joined, secret) || strings.Contains(joined, "env-token-value") {
		t.Fatalf("report leaks secrets: %+v", rep)
	}
}
