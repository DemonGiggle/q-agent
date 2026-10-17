package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func editFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0640); err != nil {
		t.Fatal(err)
	}
	return path
}
func readEdit(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUpdatePreservesDocument(t *testing.T) {
	text := `# My configuration
state_dir = './state' # relative path

[provider] # selected provider
name = 'ollama'
model = 'old' # keep this comment
base_url = "http://localhost:11434"

[telegram]
# keep my allowlists
approved_user_ids = [1, 2]
approved_chat_ids = [
 1, # mine
 -10,
] # chat note

[limits]
max_steps = 8
`
	path := editFile(t, text)
	err := Update(path, map[string]any{"provider.model": "new\"model\nnext", "provider.thinking": "high", "telegram.approved_chat_ids": []int64{3, -20}, "limits.max_steps": 12, "sandbox.enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Model != "new\"model\nnext" || cfg.Provider.Thinking != "high" || cfg.Limits.MaxSteps != 12 || !cfg.Sandbox.Enabled {
		t.Fatalf("wrong settings: %+v", cfg)
	}
	if cfg.Telegram.ApprovedChats[1] != -20 || cfg.StateDir != filepath.Join(filepath.Dir(path), "state") {
		t.Fatalf("paths or arrays changed: %+v", cfg)
	}
	data := string(readEdit(t, path))
	for _, part := range []string{"# My configuration", "state_dir = './state' # relative path", "[provider] # selected provider", "# keep this comment", "# keep my allowlists", "approved_user_ids = [1, 2]", "# chat note"} {
		if !strings.Contains(data, part) {
			t.Errorf("lost %q in\n%s", part, data)
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0640 {
		t.Fatal("permissions changed")
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".q-agent-config-*"))
	if len(temps) != 0 {
		t.Fatalf("temp files left: %v", temps)
	}
}
func TestUpdateLayouts(t *testing.T) {
	for _, text := range []string{
		"", "[provider]", "[provider] # last comment", "[provider]\r\nmodel = 'old'\r\n",
		"provider.model = 'old'\n", "[provider]\n\"model\" = 'old' # quoted key\n",
		"[provider]\nmodel = '''multi\nline''' # comment\n",
	} {
		t.Run(text, func(t *testing.T) {
			path := editFile(t, text)
			if err := Update(path, map[string]any{"provider.model": "next", "state_dir": "relative"}); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil || c.Provider.Model != "next" {
				t.Fatalf("bad update: %v %+v", err, c)
			}
		})
	}
}
func TestUpdateRejectedWithoutChangingFile(t *testing.T) {
	for _, change := range []map[string]any{
		{"provider.name": "not-a-provider"}, {"provider.model": ""}, {"limits.max_concurrent": 3},
		{"limits.task_timeout": "-1s"}, {"provider.api_key_env": "KEY", "provider.api_key_path": "key"},
		{"unknown": "x"}, {"limits.max_steps": "wrong type"},
	} {
		path := editFile(t, "[provider]\nmodel = 'original'\n")
		before := readEdit(t, path)
		if err := Update(path, change); err == nil {
			t.Fatalf("accepted invalid changes %v", change)
		}
		if !bytes.Equal(before, readEdit(t, path)) {
			t.Fatal("failed write changed configuration")
		}
	}
	// Inline tables not supported by the surgical editor must fail safely.
	path := editFile(t, "provider = { model = 'old' }\n")
	before := readEdit(t, path)
	if err := Update(path, map[string]any{"provider.model": "new"}); err == nil {
		t.Fatal("unsupported layout accepted")
	}
	if !bytes.Equal(before, readEdit(t, path)) {
		t.Fatal("unsupported layout changed file")
	}
}
func TestUpdateLockAndSymlink(t *testing.T) {
	path := editFile(t, "[provider]\nmodel = 'original'\n")
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, map[string]any{"provider.model": "other"}); err == nil {
		t.Fatal("ignored writer lock")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	link := filepath.Join(filepath.Dir(path), "link.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := Update(link, map[string]any{"provider.model": "other"}); err == nil {
		t.Fatal("replaced symlink")
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, map[string]any{"provider.model": "other"}); err == nil {
		t.Fatal("replaced read-only config")
	}
}
func TestParseEditValue(t *testing.T) {
	for _, test := range []struct {
		key, text string
		valid     bool
	}{
		{"limits.max_steps", "12", true}, {"sandbox.enabled", "true", true}, {"telegram.approved_chat_ids", "[1, -20]", true},
		{"provider.model", "literal\nmodel", true}, {"limits.max_steps", "1.5", false}, {"limits.max_steps", "'12'", false},
		{"limits.max_steps", "12\nextra = 4", false}, {"telegram.approved_chat_ids", "[1, 'two']", false}, {"secret", "value", false},
	} {
		t.Run(test.key+test.text, func(t *testing.T) {
			_, err := ParseValue(test.key, test.text)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
}
