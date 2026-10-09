package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	StateDir string   `json:"state_dir"`
	Provider Provider `json:"provider"`
	Telegram Telegram `json:"telegram"`
	Sandbox  Sandbox  `json:"sandbox"`
	Limits   Limits   `json:"limits"`
}
type Provider struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	Thinking  string `json:"thinking"`
}
type Telegram struct {
	TokenEnv      string  `json:"token_env"`
	ApprovedUsers []int64 `json:"approved_user_ids"`
	ApprovedChats []int64 `json:"approved_chat_ids"`
}
type Sandbox struct {
	BaseImage  string `json:"base_image"`
	Worker     string `json:"worker"`
	Bubblewrap string `json:"bubblewrap"`
	NewUIDMap  string `json:"newuidmap"`
	NewGIDMap  string `json:"newgidmap"`
	MMdebstrap string `json:"mmdebstrap"`
}
type Limits struct {
	MaxSteps      int    `json:"max_steps"`
	MaxToolCalls  int    `json:"max_tool_calls"`
	MaxConcurrent int    `json:"max_concurrent"`
	TaskTimeout   string `json:"task_timeout"`
	ShellTimeout  string `json:"shell_timeout"`
}

func Defaults() Config {
	return Config{StateDir: "~/.local/share/q-agent", Provider: Provider{Name: "ollama", Model: "qwen3"}, Telegram: Telegram{TokenEnv: "TELEGRAM_BOT_TOKEN"}, Sandbox: Sandbox{Bubblewrap: "bwrap", NewUIDMap: "newuidmap", NewGIDMap: "newgidmap", MMdebstrap: "mmdebstrap"}, Limits: Limits{MaxSteps: 8, MaxToolCalls: 16, MaxConcurrent: 2, TaskTimeout: "5m", ShellTimeout: "2m"}}
}
func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, fmt.Errorf("config must contain one JSON object")
	}
	if c.Provider.Name != "openai" && c.Provider.Name != "ollama" && c.Provider.Name != "opencode-go" {
		return c, fmt.Errorf("unknown provider %q", c.Provider.Name)
	}
	if c.Provider.APIKeyEnv == "" {
		switch c.Provider.Name {
		case "openai":
			c.Provider.APIKeyEnv = "OPENAI_API_KEY"
		case "opencode-go":
			c.Provider.APIKeyEnv = "OPENCODE_GO_API_KEY"
		}
	}
	if c.Provider.Model == "" {
		return c, fmt.Errorf("provider.model is required")
	}
	if c.Limits.MaxSteps < 1 || c.Limits.MaxToolCalls < 1 || c.Limits.MaxConcurrent < 1 || c.Limits.MaxConcurrent > 2 {
		return c, fmt.Errorf("positive limits required; max_concurrent must be 1 or 2")
	}
	for _, s := range []string{c.Limits.TaskTimeout, c.Limits.ShellTimeout} {
		if d, e := time.ParseDuration(s); e != nil || d <= 0 {
			return c, fmt.Errorf("invalid positive duration %q", s)
		}
	}
	c.StateDir, err = absolute(c.StateDir, filepath.Dir(path))
	if err != nil {
		return c, err
	}
	if c.Sandbox.BaseImage == "" {
		c.Sandbox.BaseImage = filepath.Join(c.StateDir, "images", "debian-trixie.tar")
	} else {
		c.Sandbox.BaseImage, err = absolute(c.Sandbox.BaseImage, filepath.Dir(path))
		if err != nil {
			return c, err
		}
	}
	if c.Sandbox.Worker == "" {
		c.Sandbox.Worker, err = os.Executable()
	} else {
		c.Sandbox.Worker, err = absolute(c.Sandbox.Worker, filepath.Dir(path))
	}
	if err != nil {
		return c, err
	}
	return c, nil
}
func absolute(path, base string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Abs(path)
}
func (c Config) TaskDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.TaskTimeout)
	return d
}
func (c Config) ShellDuration() time.Duration {
	d, _ := time.ParseDuration(c.Limits.ShellTimeout)
	return d
}
