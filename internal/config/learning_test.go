package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLearningConfigLoadAndPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Defaults when [learning] is omitted.
	if err := os.WriteFile(path, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if cfg.Learning.Enabled || cfg.Learning.ChatRecall || cfg.Learning.Schedule != "" || cfg.Learning.IdleWait != "30m" {
		t.Fatalf("unexpected learning defaults: %+v", cfg.Learning)
	}
	policy := cfg.Learning.Policy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	if policy.Enabled || policy.Schedule.Enabled || policy.Schedule.IdleMinutes != 30 {
		t.Fatalf("unexpected default policy: %+v", policy)
	}

	// Full valid [learning] section.
	tomlBody := `
state_dir = "data"

[learning]
enabled = true
chat_recall = true
include_private_chats = true
include_group_chats = true
allow_domains = ["lite.duckduckgo.com", "example.org"]
block_domains = ["blocked.example.org"]
excluded_terms = ["secret-topic"]
schedule = "23:00-03:00"
timezone = "Asia/Taipei"
idle_wait = "45m"
`
	if err := os.WriteFile(path, []byte(tomlBody), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load full learning config: %v", err)
	}
	policy = cfg.Learning.Policy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("full policy invalid: %v", err)
	}
	if !policy.Enabled || !policy.Recall.ChatEnabled ||
		!policy.Permissions.IncludePrivateChats || !policy.Permissions.IncludeGroupChats ||
		!reflect.DeepEqual(policy.Permissions.AllowDomains, []string{"lite.duckduckgo.com", "example.org"}) ||
		!reflect.DeepEqual(policy.Permissions.BlockDomains, []string{"blocked.example.org"}) ||
		!reflect.DeepEqual(policy.Permissions.ExcludedTerms, []string{"secret-topic"}) ||
		!policy.Schedule.Enabled || policy.Schedule.TimeZone != "Asia/Taipei" ||
		policy.Schedule.WindowStart != "23:00" || policy.Schedule.WindowEnd != "03:00" ||
		policy.Schedule.IdleMinutes != 45 {
		t.Fatalf("unexpected policy: %+v", policy)
	}
}

func TestLearningConfigValidationErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	for _, tc := range []struct {
		name string
		body string
	}{
		{"schedule without enabled", "[learning]\nschedule = '02:00-05:00'\n"},
		{"bad schedule format", "[learning]\nenabled = true\nschedule = '02:00'\n"},
		{"empty schedule window", "[learning]\nenabled = true\nschedule = '02:00-02:00'\n"},
		{"bad timezone", "[learning]\nenabled = true\ntimezone = 'Not/A_Zone'\n"},
		{"negative idle_wait", "[learning]\nenabled = true\nidle_wait = '-5m'\n"},
		{"bad idle_wait", "[learning]\nenabled = true\nidle_wait = 'soon'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte("state_dir = 'data'\n"+tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestLearningConfigSetAndParseValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("state_dir = 'data'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := ParseValue("learning.enabled", "true")
	if err != nil || v != true {
		t.Fatalf("ParseValue learning.enabled = %v, %v", v, err)
	}
	domains, err := ParseValue("learning.allow_domains", `["lite.duckduckgo.com", "example.org"]`)
	if err != nil || !reflect.DeepEqual(domains, []string{"lite.duckduckgo.com", "example.org"}) {
		t.Fatalf("ParseValue learning.allow_domains = %v, %v", domains, err)
	}
	if err := Update(path, map[string]any{
		"learning.enabled":       true,
		"learning.allow_domains": []string{"lite.duckduckgo.com", "example.org"},
		"learning.schedule":      "02:00-05:00",
	}); err != nil {
		t.Fatalf("Update learning keys: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Set: %v", err)
	}
	if !cfg.Learning.Enabled || cfg.Learning.Schedule != "02:00-05:00" || len(cfg.Learning.AllowDomains) != 2 {
		t.Fatalf("unexpected config after Set: %+v", cfg.Learning)
	}
}
