package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"q-agent/internal/config"
)

func consoleConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# custom\n[provider]\nname = 'ollama'\nmodel = 'before'\nbase_url = 'http://localhost:11434'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func execute(t *testing.T, path, input string, terminal bool, args ...string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	args = append(args, "--config", path)
	handled, err := run(args, strings.NewReader(input), &out, &stderr, terminal)
	if !handled {
		t.Fatal("console did not handle command")
	}
	return out.String(), stderr.String(), err
}

func TestDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "q-agent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("state_dir = 'state'\n[provider]\nmodel = 'home-model'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	override := consoleConfig(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"config", "get", "provider.model", "--json"}, `"home-model"`},
		{[]string{"models", "show", "--json"}, `"provider.model":"home-model"`},
		{[]string{"config", "get", "state_dir", "--json"}, fmt.Sprintf("%q", filepath.Join(dir, "state"))},
		{[]string{"config", "set", "provider.model", "config-model", "--json"}, `"saved":true`},
		{[]string{"config", "get", "provider.model", "--json"}, `"config-model"`},
		{[]string{"models", "set", "--model", "models-model", "--json"}, `"saved":true`},
		{[]string{"config", "get", "provider.model", "--json"}, `"models-model"`},
		{[]string{"config", "get", "provider.model", "--config", override, "--json"}, `"before"`},
	} {
		var out bytes.Buffer
		handled, err := run(test.args, noRead{t}, &out, &out, false)
		if !handled || err != nil || !strings.Contains(out.String(), test.want) {
			t.Fatalf("%v: handled=%v err=%v out=%s; want %s", test.args, handled, err, &out, test.want)
		}
	}
}

func TestExplicitCommandsNeverReadStdin(t *testing.T) {
	path := consoleConfig(t)
	for _, args := range [][]string{
		{"models", "show", "--json"}, {"models", "show", "--all", "--json"}, {"models", "providers", "--json"}, {"config", "get", "provider.model", "--json"},
		{"config", "show", "--json"}, {"config", "validate", "--json"},
		{"models", "set", "--model", "new", "--json"}, {"models", "set-default", "--model", "new", "--json"}, {"config", "set", "limits.max_steps", "12", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errBuf bytes.Buffer
			handled, err := run(append(args, "--config", path), noRead{t}, &out, &errBuf, true)
			if !handled || err != nil {
				t.Fatalf("handled=%v err=%v out=%s stderr=%s", handled, err, &out, &errBuf)
			}
			if !json.Valid(out.Bytes()) {
				t.Fatalf("not a single JSON value: %s", &out)
			}
		})
	}
}

type noRead struct{ t *testing.T }

func (n noRead) Read([]byte) (int, error) {
	n.t.Fatal("non-interactive command tried to read stdin")
	return 0, fmt.Errorf("unexpected read")
}
func TestMenuRequiresTTY(t *testing.T) {
	for _, group := range []string{"models", "config"} {
		var out bytes.Buffer
		_, err := run([]string{group}, noRead{t}, &out, &out, false)
		if err == nil || !strings.Contains(err.Error(), "terminal") {
			t.Fatalf("missing TTY error: %v", err)
		}
		_, err = run([]string{group, "--json"}, noRead{t}, &out, &out, true)
		if err == nil {
			t.Fatal("bare group with --json accepted")
		}
	}
}
func TestModelSwitchClearsProviderSpecificSettings(t *testing.T) {
	path := consoleConfig(t)
	if err := config.Update(path, map[string]any{"provider.api_key_path": "old.key", "provider.thinking": "high"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := execute(t, path, "", false, "models", "set", "--provider", "openai")
	if err == nil {
		t.Fatal("provider change without a model accepted")
	}
	_, _, err = execute(t, path, "", false, "models", "set", "--provider", "openai", "--model", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.Name != "openai" || c.Provider.Model != "test-model" || c.Provider.BaseURL != "" || c.Provider.APIKeyPath != "" || c.Provider.APIKeyEnv != "OPENAI_API_KEY" || c.Provider.Thinking != "" {
		t.Fatalf("stale provider settings: %+v", c.Provider)
	}
	_, _, err = execute(t, path, "", false, "models", "set", "--api-key-path", "new.key")
	if err != nil {
		t.Fatal(err)
	}
	c, _ = config.Load(path)
	if c.Provider.APIKeyEnv != "" || c.Provider.APIKeyPath != filepath.Join(filepath.Dir(path), "new.key") {
		t.Fatalf("wrong credential source %+v", c.Provider)
	}
}
func TestInteractiveSaveCancelAndEOF(t *testing.T) {
	for _, test := range []struct {
		name, input string
		want        string
	}{
		{"save", "1\nlimits.max_steps\n12\ny\n0\n", "12"},
		{"cancel", "1\nlimits.max_steps\n12\nn\n0\n", "8"},
		{"eof", "1\nlimits.max_steps\n12\n", "8"},
		{"partial confirmation", "1\nlimits.max_steps\n12\ny", "8"},
		{"invalid value", "1\nlimits.max_steps\n0\ny\n0\n", "8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := consoleConfig(t)
			_, _, err := execute(t, path, test.input, true, "config")
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := execute(t, path, "", false, "config", "get", "limits.max_steps", "--json")
			if err != nil || strings.TrimSpace(out) != test.want {
				t.Fatalf("out=%q err=%v", out, err)
			}
		})
	}
	path := consoleConfig(t)
	_, stderr, err := execute(t, path, "1\nopenai\nmy-model\n\n\ny\n0\n", true, "models")
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	c, _ := config.Load(path)
	if c.Provider.Name != "openai" || c.Provider.Model != "my-model" || c.Provider.BaseURL != "" {
		t.Fatalf("interactive model changes failed %+v", c.Provider)
	}
}
func TestInvalidCommandsDoNotPromptOrWrite(t *testing.T) {
	path := consoleConfig(t)
	before, _ := os.ReadFile(path)
	for _, args := range [][]string{
		{"models", "unknown"}, {"config", "set"}, {"models", "set"}, {"models", "set", "--modle", "typo"},
		{"config", "get", "not-a-key"}, {"config", "show", "extra"}, {"models", "set", "--model", ""},
		{"config", "set", "limits.max_concurrent", "3"}, {"models", "show", "--unknown"},
		{"models", "set", "--api-key-env", "KEY", "--api-key-path", "key"},
	} {
		var out bytes.Buffer
		_, err := run(append(args, "--config", path), noRead{t}, &out, &out, true)
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatalf("invalid command changed file: %v", args)
		}
	}
}
func TestNoSecretsInOutputOrErrors(t *testing.T) {
	path := consoleConfig(t)
	t.Setenv("TEST_CONSOLE_KEY", "secret-env-value")
	if err := config.Update(path, map[string]any{"provider.api_key_env": "TEST_CONSOLE_KEY", "provider.base_url": "https://user:password@example.com/v1?api_key=secret-query#secret-fragment"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "show", "--json"}, {"models", "show"}, {"config", "get", "provider.base_url", "--json"}} {
		out, stderr, err := execute(t, path, "", false, args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secret-env-value", "password", "secret-query", "secret-fragment"} {
			if strings.Contains(out+stderr, secret) {
				t.Fatalf("secret leaked: %s", out)
			}
		}
	}
	// Invalid TOML must not cause source snippets to be echoed in errors.
	os.WriteFile(path, []byte("bad = 'secret-malformed-value'\n"), 0600)
	_, _, err := execute(t, path, "", false, "config", "show")
	if err == nil || strings.Contains(err.Error(), "secret-malformed") {
		t.Fatalf("unsafe parse error %v", err)
	}
}
func TestModelsListUsesProviderNotCompletion(t *testing.T) {
	for _, provider := range []string{"ollama", "openai"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if provider == "ollama" {
					if r.URL.Path != "/api/tags" {
						t.Errorf("unexpected endpoint %s", r.URL.Path)
					}
					fmt.Fprint(w, `{"models":[{"name":"z","capabilities":[]},{"name":"a","capabilities":[]}]}`)
				} else {
					if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer test-list-key" {
						t.Errorf("unexpected request %s", r.URL.Path)
					}
					fmt.Fprint(w, `{"data":[{"id":"z"},{"id":"a"}]}`)
				}
			}))
			defer server.Close()
			path := consoleConfig(t)
			t.Setenv("LIST_KEY", "test-list-key")
			if err := config.Update(path, map[string]any{"provider.name": provider, "provider.base_url": server.URL, "provider.api_key_env": "LIST_KEY"}); err != nil {
				t.Fatal(err)
			}
			out, _, err := execute(t, path, "", false, "models", "list", "--json")
			if err != nil || strings.TrimSpace(out) != `["a","z"]` || calls != 1 {
				t.Fatalf("out=%s err=%v calls=%d", out, err, calls)
			}
		})
	}
}
func TestModelListErrorDoesNotEchoProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "secret-provider-response")
	}))
	defer server.Close()
	path := consoleConfig(t)
	if err := config.Update(path, map[string]any{"provider.base_url": server.URL}); err != nil {
		t.Fatal(err)
	}
	_, _, err := execute(t, path, "", false, "models", "list")
	if err == nil || strings.Contains(err.Error(), "secret-provider-response") {
		t.Fatalf("unsafe network error %v", err)
	}
}

func TestOptionOrderingHelpAndLiteralValues(t *testing.T) {
	path := consoleConfig(t)
	for _, args := range [][]string{
		{"config", "--config", path, "get", "provider.model", "--json"},
		{"config", "get", "provider.model", "--config=" + path, "--json"},
	} {
		var out bytes.Buffer
		_, err := run(args, noRead{t}, &out, &out, false)
		if err != nil || strings.TrimSpace(out.String()) != `"before"` {
			t.Fatalf("%v: %v %s", args, err, &out)
		}
	}
	for _, value := range []string{"--json", "--help"} {
		var out bytes.Buffer
		_, err := run([]string{"config", "--config", path, "set", "provider.model", "--", value}, noRead{t}, &out, &out, false)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := config.Load(path)
		if c.Provider.Model != value {
			t.Fatalf("literal value was parsed as flag: %q", c.Provider.Model)
		}
	}
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"models", "--help"}, {"config", "set", "--help"}} {
		var out bytes.Buffer
		handled, err := run(args, noRead{t}, &out, &out, false)
		if !handled || err != nil || out.String() != Help {
			t.Fatalf("help %v: %v", args, err)
		}
	}
	var out bytes.Buffer
	if handled, _ := run([]string{"serve"}, noRead{t}, &out, &out, false); handled {
		t.Fatal("intercepted runtime command")
	}
}

func TestReadOnlyCommandsDoNotOpenCredentialFiles(t *testing.T) {
	path := consoleConfig(t)
	if err := config.Update(path, map[string]any{"provider.api_key_path": "missing.key", "telegram.token_path": "missing.token"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"models", "show"}, {"config", "show"}, {"config", "validate"}} {
		if _, _, err := execute(t, path, "", false, args...); err != nil {
			t.Fatalf("%v accessed credentials: %v", args, err)
		}
	}
}

func TestAdditionalProviderURLsAreSanitized(t *testing.T) {
	c := config.Defaults()
	c.Providers = map[string]config.Provider{"openai": {Name: "openai", Model: "remote", BaseURL: "https://user:password@example.test/v1?token=secret#private"}}
	values := safeValues(config.Values(c))
	if got := values["providers.openai.base_url"]; got != "https://example.test/v1" {
		t.Fatalf("unsafe URL: %v", got)
	}
	if got := values["providers.openai.model"]; got != "remote" {
		t.Fatalf("missing provider values: %v", got)
	}
}

func TestModelsSetDefaultAddUpdateRemove(t *testing.T) {
	path := consoleConfig(t)
	// set-default changes only the default.
	if _, _, err := execute(t, path, "", false, "models", "set-default", "--model", "after", "--json"); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(path)
	if c.Provider.Model != "after" || c.Provider.Name != "ollama" {
		t.Fatalf("set-default failed: %+v", c.Provider)
	}
	// add creates an extra without replacing the default.
	if _, _, err := execute(t, path, "", false, "models", "add", "--provider", "openai", "--model", "remote", "--json"); err != nil {
		t.Fatal(err)
	}
	c, _ = config.Load(path)
	if c.Provider.Name != "ollama" || c.Provider.Model != "after" {
		t.Fatalf("add replaced default: %+v", c.Provider)
	}
	if _, ok := c.Providers["openai"]; !ok {
		t.Fatalf("add missing extra: %+v", c.Providers)
	}
	// show filters.
	out, _, err := execute(t, path, "", false, "models", "show", "--json")
	if err != nil || !strings.Contains(out, `"provider.model":"after"`) || strings.Contains(out, "providers.openai") {
		t.Fatalf("show default leaked extras: %s %v", out, err)
	}
	out, _, err = execute(t, path, "", false, "models", "show", "--all", "--json")
	if err != nil || !strings.Contains(out, "providers.openai") || !strings.Contains(out, "provider.model") {
		t.Fatalf("show --all missing: %s %v", out, err)
	}
	out, _, err = execute(t, path, "", false, "models", "show", "--provider", "openai", "--json")
	if err != nil || !strings.Contains(out, "providers.openai") || strings.Contains(out, `"provider.model"`) {
		t.Fatalf("show --provider wrong: %s %v", out, err)
	}
	// update edits the extra.
	if _, _, err := execute(t, path, "", false, "models", "update", "--provider", "openai", "--model", "new-remote", "--json"); err != nil {
		t.Fatal(err)
	}
	c, _ = config.Load(path)
	if c.Providers["openai"].Model != "new-remote" || c.Provider.Model != "after" {
		t.Fatalf("update failed: %+v", c)
	}
	// set-default to an adapter that already exists as an extra must fail (duplicate).
	if _, _, err := execute(t, path, "", false, "models", "set-default", "--provider", "openai", "--model", "m1", "--json"); err == nil {
		t.Fatal("set-default duplicate extra accepted")
	}
	// set-default to openai must fail while [providers.openai] still exists (duplicate).
	// Use a fresh file to test the success path without duplicates.
	fresh := consoleConfig(t)
	if _, _, err := execute(t, fresh, "", false, "models", "set-default", "--provider", "openai", "--model", "m1", "--json"); err != nil {
		t.Fatal(err)
	}
	cf, _ := config.Load(fresh)
	if cf.Provider.Name != "openai" || cf.Provider.BaseURL != "" || cf.Provider.Thinking != "" {
		t.Fatalf("set-default switch did not clear: %+v", cf.Provider)
	}
	// remove deletes the extra.
	if _, _, err := execute(t, path, "", false, "models", "remove", "--provider", "openai", "--json"); err != nil {
		t.Fatal(err)
	}
	c, _ = config.Load(path)
	if _, ok := c.Providers["openai"]; ok {
		t.Fatalf("remove failed: %+v", c.Providers)
	}
	if c.Provider.Model != "after" {
		t.Fatalf("remove touched default: %+v", c.Provider)
	}
}

func TestModelsExtrasInvalidDoNotWrite(t *testing.T) {
	path := consoleConfig(t)
	if err := config.Update(path, map[string]any{"providers.openai.model": "remote"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, args := range [][]string{
		{"models", "add", "--provider", "openai", "--model", "dup"},
		{"models", "add", "--provider", "ollama", "--model", "dup"},
		{"models", "add", "--provider", "unknown", "--model", "x"},
		{"models", "add", "--provider", "openai"},
		{"models", "update", "--provider", "missing", "--model", "x"},
		{"models", "update", "--provider", "openai"},
		{"models", "update", "--provider", "openai", "--model", ""},
		{"models", "remove", "--provider", "missing"},
		{"models", "remove"},
		{"models", "show", "--provider", "openai", "--all"},
		{"models", "show", "--provider", "missing"},
		{"models", "add", "--provider", "opencode-go", "--model", "x", "--api-key-env", "A", "--api-key-path", "B"},
		{"models", "add", "--provider", "chatgpt", "--model", "m", "--api-key-env", "KEY"},
	} {
		var out, errBuf bytes.Buffer
		_, err := run(append(args, "--config", path), noRead{t}, &out, &errBuf, true)
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatalf("invalid command changed file: %v", args)
		}
	}
}

func TestModelsSetDeprecatedWarns(t *testing.T) {
	path := consoleConfig(t)
	var out, errBuf bytes.Buffer
	handled, err := run([]string{"models", "set", "--model", "dep", "--json", "--config", path}, noRead{t}, &out, &errBuf, true)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(errBuf.String(), "deprecated") {
		t.Fatalf("missing deprecation warning: %q", errBuf.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("stdout must stay single JSON value: %s", &out)
	}
	c, _ := config.Load(path)
	if c.Provider.Model != "dep" {
		t.Fatalf("deprecated set did not save: %+v", c.Provider)
	}
}

func TestModelsListExtraProvider(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer test-list-key" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"z"},{"id":"a"}]}`)
	}))
	defer server.Close()
	path := consoleConfig(t)
	t.Setenv("LIST_KEY", "test-list-key")
	if err := config.Update(path, map[string]any{"providers.openai.model": "remote", "providers.openai.base_url": server.URL, "providers.openai.api_key_env": "LIST_KEY"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := execute(t, path, "", false, "models", "list", "--provider", "openai", "--json")
	if err != nil || strings.TrimSpace(out) != `["a","z"]` || calls != 1 {
		t.Fatalf("out=%s err=%v calls=%d", out, err, calls)
	}
	if _, _, err := execute(t, path, "", false, "models", "list", "--provider", "missing", "--json"); err == nil {
		t.Fatal("unconfigured provider accepted for list")
	}
}

func TestModelsMenuManageExtras(t *testing.T) {
	path := consoleConfig(t)
	// Top menu 2 -> manage extras -> 1 add openai/remote -> defaults -> y -> back -> exit.
	input := "2\n1\nopenai\nremote-model\n\n\n\ny\n0\n0\n"
	if _, _, err := execute(t, path, input, true, "models"); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load(path)
	if c.Providers["openai"].Model != "remote-model" {
		t.Fatalf("menu add failed: %+v", c.Providers)
	}
	if c.Provider.Model != "before" {
		t.Fatalf("menu add touched default: %+v", c.Provider)
	}
}


func TestConsoleAutoMigratesLegacyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[provider]\nname = 'ollama'\nmodel = 'before'\n[telegram]\napproved_user_ids = [123456789]\napproved_chat_ids = [-1001234567890]\n[limits]\nmax_steps = 8\nmax_tool_calls = 5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := execute(t, path, "", false, "models", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "migrated config") || !strings.Contains(stderr, "backup:") {
		t.Fatalf("missing migration notice: %q", stderr)
	}
	if !strings.Contains(out, `"provider.model":"before"`) {
		t.Fatalf("unexpected show output: %s", out)
	}
	// Stdout stays a single JSON value; the notice goes to stderr.
	if !json.Valid([]byte(out)) {
		t.Fatalf("stdout is not JSON: %s", out)
	}
	after, _ := os.ReadFile(path)
	text := string(after)
	if strings.Contains(text, "approved_") || strings.Contains(text, "max_tool_calls") {
		t.Fatalf("legacy keys remain:\n%s", text)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Telegram.AllowedUsers) != 1 || len(c.Telegram.AllowedGroups) != 1 {
		t.Fatalf("migrated allowlist: %+v", c.Telegram)
	}
	// Second run is a no-op: no repeat notice.
	_, stderr, err = execute(t, path, "", false, "models", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "migrated config") {
		t.Fatalf("repeat migration notice: %q", stderr)
	}
}
