package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"state_dir":"state","provider":{"name":"opencode-go","model":"model"}}`), 0600)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.APIKeyEnv != "OPENCODE_GO_API_KEY" || c.StateDir != filepath.Join(dir, "state") || c.Limits.MaxSteps != 8 {
		t.Fatalf("%+v", c)
	}
	for _, text := range []string{`{"limits":{"max_concurrent":3}}`, `{"limits":{"task_timeout":"0s"}}`, `{"unexpected":true}`, `{} {}`} {
		os.WriteFile(path, []byte(text), 0600)
		if _, err = Load(path); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
