package console

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q-agent/internal/config"
)

func writeCheckConsoleConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigCheckValidHumanAndJSON(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "state_dir = 'state'\n[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n")
	out, _, err := execute(t, path, "", false, "config", "check")
	if err != nil || !strings.Contains(out, "OK") {
		t.Fatalf("human check failed: out=%q err=%v", out, err)
	}
	out, _, err = execute(t, path, "", false, "config", "check", "--json")
	if err != nil {
		t.Fatalf("json check failed: %v", err)
	}
	var rep struct {
		Valid    bool     `json:"valid"`
		Errors   []string `json:"errors"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || !rep.Valid {
		t.Fatalf("bad json report: %q %v", out, err)
	}
	if rep.Errors == nil || rep.Warnings == nil {
		t.Fatalf("errors/warnings must be arrays, got: %q", out)
	}
}

func TestConfigCheckErrorsFail(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n")
	_, _, err := execute(t, path, "", false, "config", "check")
	if err == nil {
		t.Fatal("check with empty allowlist accepted")
	}
	out, _, err := execute(t, path, "", false, "config", "check", "--json")
	if err == nil {
		t.Fatal("json check with errors accepted")
	}
	// Even on failure the JSON report must be a single value on stdout.
	var rep struct {
		Valid    bool     `json:"valid"`
		Errors   []string `json:"errors"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Valid || len(rep.Errors) == 0 {
		t.Fatalf("bad failing json report: %q", out)
	}
}

func TestConfigCheckWarningsPass(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\nthinking = 'auto'\n[telegram]\nallowed_users = [1]\n")
	out, _, err := execute(t, path, "", false, "config", "check")
	if err != nil {
		t.Fatalf("warnings should not fail: %v", err)
	}
	if !strings.Contains(out, "warning:") {
		t.Fatalf("warning line missing: %q", out)
	}
}

func TestConfigCheckNeverReadsStdin(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	path := writeCheckConsoleConfig(t, "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n")
	var out, errBuf bytes.Buffer
	handled, err := run([]string{"config", "check", "--json", "--config", path}, noRead{t}, &out, &errBuf, true)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("not a single JSON value: %s", &out)
	}
}

func TestConfigCheckDoesNotMigrateFile(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-token")
	before := "[provider]\nname = 'ollama'\nmodel = 'm'\n[telegram]\nallowed_users = [1]\n[limits]\nmax_tool_calls = 5\n"
	path := writeCheckConsoleConfig(t, before)
	_, _, err := execute(t, path, "", false, "config", "check")
	if err != nil {
		t.Fatalf("deprecated-only check should pass: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != before {
		t.Fatalf("check rewrote the file: %q", after)
	}
	// Sanity: plain Load still sees the legacy key, migrated load succeeds.
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected strict Load to reject max_tool_calls")
	}
}
