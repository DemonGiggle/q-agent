package config

import (
	"path/filepath"
	"testing"
)

func TestAdministratorConfigurationAndConsoleEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeConfig(t, path, "# keep this comment\n[telegram]\nallowed_users = [1]\nallowed_groups = [-10]\n")
	c, err := Load(path)
	if err != nil || len(c.Telegram.AdminUsers) != 0 {
		t.Fatalf("not disabled by default: %+v %v", c.Telegram, err)
	}
	value, err := ParseValue("telegram.admin_user_ids", "[1]")
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(path, map[string]any{"telegram.admin_user_ids": value}); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil || len(c.Telegram.AdminUsers) != 1 || c.Telegram.AdminUsers[0] != 1 {
		t.Fatalf("%+v %v", c.Telegram, err)
	}
}
