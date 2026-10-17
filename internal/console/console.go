// Package console implements human menus and prompt-free automation commands.
package console

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
	"q-agent/internal/config"
	"q-agent/internal/llm"
)

const Help = `q-agent — Telegram agent and local configuration console

Human menus (require a terminal):
  q-agent models [--config FILE]
  q-agent config [--config FILE]

Automation (never prompts; --json for machine-readable output):
  q-agent models show|list|providers [--config FILE] [--json]
  q-agent models set --model NAME [--provider NAME] [--base-url URL]
                     [--thinking LEVEL] [--api-key-env NAME | --api-key-path FILE]
                     [--config FILE] [--json]
  q-agent config show|validate [--config FILE] [--json]
  q-agent config get KEY [--config FILE] [--json]
  q-agent config set KEY VALUE [--config FILE] [--json]

Runtime commands (non-interactive):
  q-agent serve [--config FILE]
  q-agent image build [--config FILE]
  q-agent update [--arch amd64|arm64]
  q-agent version

Config defaults to ` + config.DefaultPath + `.
Configuration edits apply at the next start; these commands never restart the
service. Use --help for this command list.
`

type console struct {
	in          *bufio.Reader
	out, errOut io.Writer
	terminal    bool
	path        string
	json        bool
}

// Run returns false for legacy runtime commands, leaving their behavior intact.
func Run(args []string, in *os.File, out, errOut io.Writer) (bool, error) {
	return run(args, in, out, errOut, term.IsTerminal(int(in.Fd())))
}

func run(args []string, in io.Reader, out, errOut io.Writer, terminal bool) (bool, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprint(out, Help)
		return true, err
	}
	group := args[0]
	if group != "models" && group != "config" {
		return false, nil
	}
	c := &console{in: bufio.NewReader(in), out: out, errOut: errOut, terminal: terminal, path: config.DefaultPath}
	rest, err := c.options(args[1:])
	if err != nil {
		return true, err
	}
	if len(rest) > 0 && (rest[0] == "--help" || rest[0] == "-h" ||
		(len(rest) == 2 && (rest[1] == "--help" || rest[1] == "-h"))) {
		_, err := fmt.Fprint(out, Help)
		return true, err
	}
	if len(rest) == 0 {
		if c.json {
			return true, fmt.Errorf("--json requires an explicit action (for example %s show --json)", group)
		}
		if !c.terminal {
			return true, fmt.Errorf("%s requires an interactive terminal; use '%s show' or '%s set' for automation", group, group, group)
		}
		return true, c.menu(group)
	}
	return true, c.action(group, rest[0], rest[1:])
}

// Global console options can precede or follow the action. '--' protects literal
// keys/values such as a model name or string that happens to begin with '--'.
func (c *console) options(args []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return append(rest, args[i+1:]...), nil
		case arg == "--json":
			c.json = true
		case arg == "--config":
			i++
			if i == len(args) || args[i] == "" {
				return nil, fmt.Errorf("--config requires a file")
			}
			c.path = args[i]
		case strings.HasPrefix(arg, "--config="):
			c.path = strings.TrimPrefix(arg, "--config=")
			if c.path == "" {
				return nil, fmt.Errorf("--config requires a file")
			}
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

func safeValues(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		if key == "provider.base_url" {
			if raw, ok := value.(string); ok && raw != "" {
				u, err := url.Parse(raw)
				if err != nil {
					value = "[invalid URL]"
				} else {
					u.User = nil
					u.RawQuery = ""
					u.ForceQuery = false
					u.Fragment = ""
					value = u.String()
				}
			}
		}
		result[key] = value
	}
	return result
}
func (c *console) output(value any) error {
	if c.json {
		return json.NewEncoder(c.out).Encode(value)
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			encoded, err := json.Marshal(v[key])
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(c.out, "%s = %s\n", key, encoded); err != nil {
				return err
			}
		}
	case []string:
		for _, item := range v {
			if _, err := fmt.Fprintf(c.out, "%q\n", item); err != nil {
				return err
			}
		}
	default:
		return json.NewEncoder(c.out).Encode(value)
	}
	return nil
}
func load(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	// TOML parser errors may quote input. Never print malformed config contents.
	if err != nil {
		return cfg, fmt.Errorf("cannot load configuration; check the file path, TOML syntax and values")
	}
	return cfg, nil
}
func (c *console) action(group, action string, args []string) error {
	if group == "models" && action == "set" {
		return c.setModels(args)
	}
	if group == "config" && action == "set" {
		if len(args) != 2 {
			return fmt.Errorf("usage: q-agent config set KEY VALUE [--config FILE] [--json]")
		}
		value, err := config.ParseValue(args[0], args[1])
		if err != nil {
			return err
		}
		return c.save(map[string]any{args[0]: value})
	}
	allowed := map[string]int{"show": 0}
	if group == "models" {
		allowed["providers"] = 0
		allowed["list"] = 0
	} else {
		allowed["get"] = 1
		allowed["validate"] = 0
	}
	count, ok := allowed[action]
	if !ok {
		return fmt.Errorf("unknown %s action %q; use --help", group, action)
	}
	if len(args) != count {
		return fmt.Errorf("%s %s expects %d arguments; use --help", group, action, count)
	}
	if action == "providers" {
		return c.output(llm.Names())
	}
	cfg, err := load(c.path)
	if err != nil {
		return err
	}
	switch action {
	case "validate":
		return c.output(map[string]any{"valid": true})
	case "get":
		values := safeValues(config.Values(cfg))
		value, ok := values[args[0]]
		if !ok {
			return fmt.Errorf("unknown configuration key %q", args[0])
		}
		if c.json {
			return c.output(value)
		}
		return c.output(map[string]any{args[0]: value})
	case "list":
		return c.listModels(cfg)
	default:
		values := safeValues(config.Values(cfg))
		if group == "models" {
			for key := range values {
				if !strings.HasPrefix(key, "provider.") {
					delete(values, key)
				}
			}
		}
		return c.output(values)
	}
}
func (c *console) listModels(cfg config.Config) error {
	key, err := cfg.Provider.APIKey()
	if err != nil {
		return fmt.Errorf("cannot read provider credential; check its reference and permissions")
	}
	if cfg.Provider.Name != "ollama" && key == "" {
		return fmt.Errorf("provider credential is not available")
	}
	p, err := llm.New(cfg.Provider.Name, llm.Config{BaseURL: cfg.Provider.BaseURL, APIKey: key, HTTP: &http.Client{Timeout: 15 * time.Second}})
	if err != nil {
		return fmt.Errorf("cannot initialize provider")
	}
	lister, ok := p.(llm.ModelLister)
	if !ok {
		return fmt.Errorf("provider does not support model listing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := lister.Models(ctx)
	if err != nil {
		return fmt.Errorf("cannot list provider models; check endpoint, credentials and connectivity")
	}
	sort.Strings(models)
	if models == nil {
		models = []string{}
	}
	return c.output(models)
}
func (c *console) setModels(args []string) error {
	fs := flag.NewFlagSet("models set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fields := map[string]*string{}
	for _, name := range []string{"provider", "model", "base-url", "thinking", "api-key-env", "api-key-path"} {
		fields[name] = fs.String(name, "", name)
	}
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid models set options; use --help")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("models set takes flags, not positional arguments")
	}
	changes := map[string]any{}
	names := map[string]string{"provider": "name", "model": "model", "base-url": "base_url", "thinking": "thinking", "api-key-env": "api_key_env", "api-key-path": "api_key_path"}
	fs.Visit(func(f *flag.Flag) { changes["provider."+names[f.Name]] = *fields[f.Name] })
	if len(changes) == 0 {
		return fmt.Errorf("models set requires at least one setting; use --model NAME")
	}
	cfg, err := load(c.path)
	if err != nil {
		return err
	}
	if name, ok := changes["provider.name"]; ok && name != cfg.Provider.Name {
		if model, ok := changes["provider.model"]; !ok || model == "" {
			return fmt.Errorf("changing provider also requires --model NAME")
		}
		// Do not carry another provider's endpoint or credential across providers.
		for _, key := range []string{"base_url", "api_key_env", "api_key_path", "thinking"} {
			full := "provider." + key
			if _, ok := changes[full]; !ok {
				changes[full] = ""
			}
		}
	}
	// Selecting one credential source clears the other unless both were explicitly
	// supplied, in which case ordinary configuration validation rejects it.
	if _, ok := changes["provider.api_key_env"]; ok {
		if _, both := changes["provider.api_key_path"]; !both {
			changes["provider.api_key_path"] = ""
		}
	}
	if _, ok := changes["provider.api_key_path"]; ok {
		if _, both := changes["provider.api_key_env"]; !both {
			changes["provider.api_key_env"] = ""
		}
	}
	return c.save(changes)
}
func (c *console) save(changes map[string]any) error {
	if err := config.Update(c.path, changes); err != nil {
		return err
	}
	return c.output(map[string]any{"saved": true, "restart_required": true})
}

func (c *console) prompt(label string) (string, error) {
	if _, err := fmt.Fprint(c.out, label); err != nil {
		return "", err
	}
	text, err := c.in.ReadString('\n')
	// EOF never submits a partially typed answer or confirmation.
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}
func (c *console) menu(group string) error {
	for {
		if _, err := fmt.Fprintf(c.out, "\nq-agent %s — saved configuration (not live service status)\n", group); err != nil {
			return err
		}
		if err := c.action(group, "show", nil); err != nil {
			return err
		}
		choices := "1) Edit a setting  2) Validate  0) Exit\n"
		if group == "models" {
			choices = "1) Change provider/model  2) List provider models  3) Supported providers  0) Exit\n"
		}
		fmt.Fprint(c.out, choices)
		choice, err := c.prompt("> ")
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "0", "q", "quit", "":
			return nil
		case "1":
			if group == "models" {
				err = c.editModels()
			} else {
				err = c.editConfig()
			}
		case "2":
			action := "validate"
			if group == "models" {
				action = "list"
			}
			err = c.action(group, action, nil)
		case "3":
			if group == "models" {
				err = c.action(group, "providers", nil)
			} else {
				err = fmt.Errorf("choose 0, 1 or 2")
			}
		default:
			err = fmt.Errorf("choose one of the listed actions")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			fmt.Fprintln(c.errOut, err)
		}
	}
}
func (c *console) confirm(changes map[string]any) error {
	fmt.Fprintln(c.out, "Proposed changes:")
	if err := c.output(safeValues(changes)); err != nil {
		return err
	}
	answer, err := c.prompt("Save for the next service start? [y/N] ")
	if err != nil {
		return err
	}
	if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		return c.save(changes)
	}
	_, err = fmt.Fprintln(c.out, "Cancelled; no changes saved.")
	return err
}
func (c *console) editConfig() error {
	key, err := c.prompt("Setting key (for example limits.max_steps; blank cancels): ")
	if err != nil {
		return err
	}
	if key == "" {
		return nil
	}
	if _, ok := config.Values(config.Defaults())[key]; !ok {
		return fmt.Errorf("unknown configuration key %q", key)
	}
	text, err := c.prompt("New value (strings as text; other values as TOML): ")
	if err != nil {
		return err
	}
	value, err := config.ParseValue(key, text)
	if err != nil {
		return err
	}
	return c.confirm(map[string]any{key: value})
}
func (c *console) editModels() error {
	cfg, err := load(c.path)
	if err != nil {
		return err
	}
	name, err := c.prompt(fmt.Sprintf("Provider [%s] (ollama/openai/opencode-go): ", cfg.Provider.Name))
	if err != nil {
		return err
	}
	if name == "" {
		name = cfg.Provider.Name
	}
	current := cfg.Provider.Model
	if name != cfg.Provider.Name {
		current = ""
	}
	model, err := c.prompt(fmt.Sprintf("Model [%s]: ", current))
	if err != nil {
		return err
	}
	if model == "" {
		model = current
	}
	if model == "" {
		return fmt.Errorf("model is required")
	}
	changes := map[string]any{"provider.name": name, "provider.model": model}
	if name != cfg.Provider.Name {
		for _, key := range []string{"base_url", "api_key_env", "api_key_path", "thinking"} {
			changes["provider."+key] = ""
		}
	}
	for _, key := range []string{"base_url", "thinking"} {
		value, err := c.prompt(key + " (Enter keeps current/default; '-' clears): ")
		if err != nil {
			return err
		}
		if value == "-" {
			changes["provider."+key] = ""
		} else if value != "" {
			changes["provider."+key] = value
		}
	}
	return c.confirm(changes)
}
