package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backups(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func TestMigrateAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, `# Operator configuration
[provider]
name = 'ollama'
model = 'local' # keep me
[telegram]
# keep my allowlists
approved_user_ids = [123456789]
approved_chat_ids = [123456789, -1001234567890]
`)
	rep, err := MigrateIfNeeded(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Migrated || rep.Backup == "" || len(rep.Changes) != 2 {
		t.Fatalf("report: %+v", rep)
	}
	data, err := os.ReadFile(rep.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "approved_user_ids") {
		t.Fatal("backup must hold the original")
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Telegram.AllowedUsers) != 1 || c.Telegram.AllowedUsers[0] != 123456789 {
		t.Fatalf("users: %+v", c.Telegram)
	}
	if len(c.Telegram.AllowedGroups) != 1 || c.Telegram.AllowedGroups[0] != -1001234567890 {
		t.Fatalf("groups: %+v", c.Telegram)
	}
	after, _ := os.ReadFile(path)
	text := string(after)
	if strings.Contains(text, "approved_") {
		t.Fatalf("legacy keys remain:\n%s", text)
	}
	for _, want := range []string{"# Operator configuration", "model = 'local' # keep me", "# keep my allowlists", "allowed_users = [123456789]", "allowed_groups = [-1001234567890]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lost %q in:\n%s", want, text)
		}
	}
	if n := len(backups(t, dir)); n != 1 {
		t.Fatalf("backups: %d", n)
	}
	// Idempotent: second run rewrites nothing and creates no new backup.
	rep, err = MigrateIfNeeded(path)
	if err != nil || rep.Migrated {
		t.Fatalf("second run: %+v %v", rep, err)
	}
	if n := len(backups(t, dir)); n != 1 {
		t.Fatalf("backups after no-op: %d", n)
	}
	if _, _, err := LoadMigrated(path); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateMaxToolCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\n[limits]\nmax_steps = 8\nmax_tool_calls = 10\n")
	rep, err := MigrateIfNeeded(path)
	if err != nil || !rep.Migrated {
		t.Fatalf("report: %+v err: %v", rep, err)
	}
	if len(rep.Changes) != 1 || !strings.Contains(rep.Changes[0], "max_tool_calls") {
		t.Fatalf("changes: %v", rep.Changes)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxSteps != 8 {
		t.Fatalf("limits: %+v", c.Limits)
	}
	text, _ := os.ReadFile(path)
	if strings.Contains(string(text), "max_tool_calls") {
		t.Fatalf("key remains:\n%s", text)
	}
}

func TestMigrateDottedLayout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\ntelegram.approved_user_ids = [7]\n")
	rep, err := MigrateIfNeeded(path)
	if err != nil || !rep.Migrated {
		t.Fatalf("report: %+v err: %v", rep, err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Telegram.AllowedUsers) != 1 || c.Telegram.AllowedUsers[0] != 7 {
		t.Fatalf("users: %+v", c.Telegram)
	}
}

func TestMigrateMixedOldNewFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\n[telegram]\nallowed_users = [1]\napproved_user_ids = [1]\n")
	before, _ := os.ReadFile(path)
	if _, err := MigrateIfNeeded(path); err == nil {
		t.Fatal("mixed old/new keys migrated")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed migration changed the file")
	}
	if n := len(backups(t, dir)); n != 0 {
		t.Fatalf("backups: %d", n)
	}
}

func TestMigrateUnknownKeyUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\nnot_a_key = true\n")
	rep, err := MigrateIfNeeded(path)
	if err != nil || rep.Migrated {
		t.Fatalf("report: %+v err: %v", rep, err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown key accepted")
	}
	if n := len(backups(t, dir)); n != 0 {
		t.Fatalf("backups: %d", n)
	}
}

func TestMigrateEmptyLegacyKeysRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\n[telegram]\nallowed_users = [1]\n")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Empty legacy arrays are dead weight: drop them without touching values.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("approved_user_ids = []\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	rep, err := MigrateIfNeeded(path)
	if err != nil || !rep.Migrated {
		t.Fatalf("report: %+v err: %v", rep, err)
	}
	text, _ := os.ReadFile(path)
	if strings.Contains(string(text), "approved_user_ids") {
		t.Fatalf("empty legacy key remains:\n%s", text)
	}
	c, err := Load(path)
	if err != nil || len(c.Telegram.AllowedUsers) != 1 {
		t.Fatalf("users: %+v err: %v", c.Telegram, err)
	}
}

func TestMigrateReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\n[telegram]\napproved_user_ids = [5]\n")
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	// Allowlist-only legacy files stay readable: Load migrates in memory.
	rep, err := MigrateIfNeeded(path)
	if err != nil || rep.Migrated {
		t.Fatalf("report: %+v err: %v", rep, err)
	}
	c, err := Load(path)
	if err != nil || len(c.Telegram.AllowedUsers) != 1 {
		t.Fatalf("users: %+v err: %v", c.Telegram, err)
	}

	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "[provider]\nname = 'ollama'\nmodel = 'local'\n[limits]\nmax_steps = 8\nmax_tool_calls = 3\n")
	// Rewrite with owner-write, then lock it down: removals cannot be
	// deferred to memory, so migration must fail loudly.
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateIfNeeded(path); err == nil {
		t.Fatal("read-only removal accepted")
	}
}
