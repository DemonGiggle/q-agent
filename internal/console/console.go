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
  q-agent login chatgpt [--config FILE]

Automation (never prompts; --json for machine-readable output):
  q-agent models show [--provider NAME | --all] [--config FILE] [--json]
  q-agent models list [--provider NAME] [--config FILE] [--json]
  q-agent models providers [--config FILE] [--json]
  q-agent models set-default --model NAME [--provider NAME] [--base-url URL]
                             [--thinking LEVEL] [--api-key-env NAME | --api-key-path FILE]
                             [--config FILE] [--json]
  q-agent models add --provider NAME --model NAME [--base-url URL]
                    [--thinking LEVEL] [--api-key-env NAME | --api-key-path FILE]
                    [--config FILE] [--json]
  q-agent models update --provider NAME [--model NAME] [--base-url URL]
                       [--thinking LEVEL] [--api-key-env NAME | --api-key-path FILE]
                       [--config FILE] [--json]
  q-agent models remove --provider NAME [--config FILE] [--json]
  q-agent models set ... (deprecated alias for set-default)
  q-agent config show|validate [--config FILE] [--json]
  q-agent config get KEY [--config FILE] [--json]
  q-agent config set KEY VALUE [--config FILE] [--json]

Runtime commands (non-interactive):
  q-agent serve [--config FILE]
  q-agent image build [--config FILE]
  q-agent update [--arch amd64|arm64]
  q-agent service install|print [--config FILE]
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
	if group != "models" && group != "config" && group != "login" {
		return false, nil
	}
	c := &console{in: bufio.NewReader(in), out: out, errOut: errOut, terminal: terminal, path: config.DefaultPath}
	rest, err := c.options(args[1:])
	if err != nil {
		return true, err
	}
	if group == "login" {
		if len(rest) > 0 && (rest[0] == "--help" || rest[0] == "-h") {
			_, err := fmt.Fprint(out, Help)
			return true, err
		}
		// Login is always interactive; it never has a menu or JSON mode.
		return true, c.login(rest)
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
		if key == "provider.base_url" || strings.HasPrefix(key, "providers.") && strings.HasSuffix(key, ".base_url") {
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
func (c *console) load() (config.Config, error) {
	cfg, rep, err := config.LoadMigrated(c.path)
	if rep.Migrated {
		fmt.Fprintf(c.errOut, "migrated config (%s); backup: %s\n", strings.Join(rep.Changes, "; "), rep.Backup)
	}
	// TOML parser errors may quote input. Never print malformed config contents.
	if err != nil {
		return cfg, fmt.Errorf("cannot load configuration; check the file path, TOML syntax and values")
	}
	return cfg, nil
}
func (c *console) action(group, action string, args []string) error {
	if group == "models" {
		switch action {
		case "set":
			return c.setModelsDeprecated(args)
		case "set-default":
			return c.setDefaultModels(args)
		case "add":
			return c.addProvider(args)
		case "update":
			return c.updateProvider(args)
		case "remove":
			return c.removeProvider(args)
		case "show":
			return c.showModels(args)
		case "list":
			return c.listModelsDispatch(args)
		case "providers":
			if len(args) != 0 {
				return fmt.Errorf("models providers expects no arguments; use --help")
			}
			return c.output(llm.Names())
		default:
			return fmt.Errorf("unknown models action %q; use --help", action)
		}
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
	cfg, err := c.load()
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
	default:
		values := safeValues(config.Values(cfg))
		return c.output(values)
	}
}

func (c *console) showModels(args []string) error {
	fs := flag.NewFlagSet("models show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	provider := fs.String("provider", "", "provider")
	all := fs.Bool("all", false, "all")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid models show options; use --help")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("models show takes flags, not positional arguments")
	}
	if *provider != "" && *all {
		return fmt.Errorf("models show accepts either --provider NAME or --all, not both")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	values := safeValues(config.Values(cfg))
	if *all {
		filtered := map[string]any{}
		for k, v := range values {
			if strings.HasPrefix(k, "provider.") || strings.HasPrefix(k, "providers.") {
				filtered[k] = v
			}
		}
		return c.output(filtered)
	}
	if *provider != "" {
		if *provider == cfg.Provider.Name {
			return c.output(filterDefault(values))
		}
		if _, ok := cfg.Providers[*provider]; !ok {
			return fmt.Errorf("provider %q is not configured; use `models show --all` to list configured providers", *provider)
		}
		prefix := "providers." + *provider + "."
		filtered := map[string]any{}
		for k, v := range values {
			if strings.HasPrefix(k, prefix) {
				filtered[k] = v
			}
		}
		return c.output(filtered)
	}
	return c.output(filterDefault(values))
}

func filterDefault(values map[string]any) map[string]any {
	filtered := map[string]any{}
	for k, v := range values {
		if strings.HasPrefix(k, "provider.") {
			filtered[k] = v
		}
	}
	return filtered
}

func (c *console) listModelsDispatch(args []string) error {
	fs := flag.NewFlagSet("models list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	providerName := fs.String("provider", "", "provider")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid models list options; use --help")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("models list takes flags, not positional arguments")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	var selected config.Provider
	if *providerName == "" {
		selected = cfg.Provider
	} else {
		p, ok := cfg.ProviderByName(*providerName)
		if !ok {
			return fmt.Errorf("provider %q is not configured; use `models show --all` to list configured providers", *providerName)
		}
		selected = p
	}
	return c.listProviderModels(cfg, selected)
}

func (c *console) listModels(cfg config.Config) error {
	return c.listProviderModels(cfg, cfg.Provider)
}

func (c *console) listProviderModels(cfg config.Config, provider config.Provider) error {
	if provider.Name == "chatgpt" {
		p, err := llm.New(provider.Name, llm.Config{BaseURL: provider.BaseURL, ChatGPTOAuthPath: cfg.ChatGPT.OAuthPath, HTTP: &http.Client{Timeout: 15 * time.Second}})
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
			return fmt.Errorf("cannot list provider models; check login, endpoint and connectivity (run `q-agent login chatgpt`)")
		}
		sort.Strings(models)
		if models == nil {
			models = []string{}
		}
		return c.output(models)
	}
	key, err := provider.APIKey()
	if err != nil {
		return fmt.Errorf("cannot read provider credential; check its reference and permissions")
	}
	if provider.Name != "ollama" && key == "" {
		return fmt.Errorf("provider credential is not available")
	}
	p, err := llm.New(provider.Name, llm.Config{BaseURL: provider.BaseURL, APIKey: key, HTTP: &http.Client{Timeout: 15 * time.Second}})
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

func parseModelFlags(action string, args []string) (map[string]*string, *flag.FlagSet, error) {
	fs := flag.NewFlagSet("models "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fields := map[string]*string{}
	for _, name := range []string{"provider", "model", "base-url", "thinking", "api-key-env", "api-key-path"} {
		fields[name] = fs.String(name, "", name)
	}
	if err := fs.Parse(args); err != nil {
		return nil, nil, fmt.Errorf("invalid models %s options; use --help", action)
	}
	if fs.NArg() != 0 {
		return nil, nil, fmt.Errorf("models %s takes flags, not positional arguments", action)
	}
	return fields, fs, nil
}

func modelFlagChanges(fields map[string]*string, fs *flag.FlagSet, prefix string) map[string]any {
	changes := map[string]any{}
	names := map[string]string{"provider": "name", "model": "model", "base-url": "base_url", "thinking": "thinking", "api-key-env": "api_key_env", "api-key-path": "api_key_path"}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "provider" && prefix != "provider." {
			// Provider flag selects the table for add/update/remove; not a field.
			return
		}
		changes[prefix+names[f.Name]] = *fields[f.Name]
	})
	return changes
}

func clearOtherCredential(changes map[string]any, prefix string) {
	if _, ok := changes[prefix+"api_key_env"]; ok {
		if _, both := changes[prefix+"api_key_path"]; !both {
			changes[prefix+"api_key_path"] = ""
		}
	}
	if _, ok := changes[prefix+"api_key_path"]; ok {
		if _, both := changes[prefix+"api_key_env"]; !both {
			changes[prefix+"api_key_env"] = ""
		}
	}
}

func (c *console) setDefaultModels(args []string) error {
	fields, fs, err := parseModelFlags("set-default", args)
	if err != nil {
		return err
	}
	_ = fields
	changes := modelFlagChanges(fields, fs, "provider.")
	if len(changes) == 0 {
		return fmt.Errorf("models set-default requires at least one setting; use --model NAME")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	if name, ok := changes["provider.name"]; ok && name != cfg.Provider.Name {
		if !config.ValidProviderAdapter(name.(string)) {
			return fmt.Errorf("unknown provider %q", name)
		}
		if _, dup := cfg.Providers[name.(string)]; dup {
			return fmt.Errorf("providers.%s already exists as an extra; remove it first or choose another default", name)
		}
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
	clearOtherCredential(changes, "provider.")
	return c.save(changes)
}

func (c *console) addProvider(args []string) error {
	fields, fs, err := parseModelFlags("add", args)
	if err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	name := *fields["provider"]
	if !visited["provider"] || name == "" {
		return fmt.Errorf("models add requires --provider NAME and --model NAME")
	}
	if !config.ValidProviderAdapter(name) {
		return fmt.Errorf("unknown provider %q", name)
	}
	model := ""
	if visited["model"] {
		model = *fields["model"]
	}
	if !visited["model"] || model == "" {
		return fmt.Errorf("models add requires --provider NAME and --model NAME")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	if name == cfg.Provider.Name {
		return fmt.Errorf("providers.%s duplicates the default provider; use models set-default to change the default", name)
	}
	if _, exists := cfg.Providers[name]; exists {
		return fmt.Errorf("provider %q already exists; use models update to edit it", name)
	}
	if visited["api-key-env"] && visited["api-key-path"] && *fields["api-key-env"] != "" && *fields["api-key-path"] != "" {
		return fmt.Errorf("provider.api_key_env and provider.api_key_path are mutually exclusive")
	}
	if name == "chatgpt" && ((visited["api-key-env"] && *fields["api-key-env"] != "") || (visited["api-key-path"] && *fields["api-key-path"] != "")) {
		return fmt.Errorf("provider chatgpt uses ChatGPT OAuth (q-agent login chatgpt); clear api_key_env and api_key_path")
	}
	changes := map[string]any{}
	prefix := "providers." + name + "."
	if visited["model"] {
		changes[prefix+"model"] = *fields["model"]
	}
	for _, flagName := range []string{"base-url", "thinking", "api-key-env", "api-key-path"} {
		if visited[flagName] {
			suffix := map[string]string{"base-url": "base_url", "thinking": "thinking", "api-key-env": "api_key_env", "api-key-path": "api_key_path"}[flagName]
			// Skip empty clears on add to keep the new table minimal; empty means default.
			if *fields[flagName] == "" {
				continue
			}
			changes[prefix+suffix] = *fields[flagName]
		}
	}
	if len(changes) == 0 {
		return fmt.Errorf("models add requires --provider NAME and --model NAME")
	}
	return c.save(changes)
}

func (c *console) updateProvider(args []string) error {
	fields, fs, err := parseModelFlags("update", args)
	if err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	name := *fields["provider"]
	if !visited["provider"] || name == "" {
		return fmt.Errorf("models update requires --provider NAME and at least one setting")
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	if name == cfg.Provider.Name && !func() bool { _, ok := cfg.Providers[name]; return ok }() {
		return fmt.Errorf("provider %q is the default provider; use models set-default to change it", name)
	}
	if _, ok := cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q is not configured as an extra; use models add to create it", name)
	}
	prefix := "providers." + name + "."
	changes := map[string]any{}
	for _, flagName := range []string{"model", "base-url", "thinking", "api-key-env", "api-key-path"} {
		if visited[flagName] {
			suffix := map[string]string{"model": "model", "base-url": "base_url", "thinking": "thinking", "api-key-env": "api_key_env", "api-key-path": "api_key_path"}[flagName]
			val := *fields[flagName]
			if flagName == "model" && val == "" {
				return fmt.Errorf("provider.model is required")
			}
			changes[prefix+suffix] = val
		}
	}
	if len(changes) == 0 {
		return fmt.Errorf("models update requires --provider NAME and at least one setting")
	}
	clearOtherCredential(changes, prefix)
	return c.save(changes)
}

func (c *console) removeProvider(args []string) error {
	fs := flag.NewFlagSet("models remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("provider", "", "provider")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid models remove options; use --help")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("models remove takes flags, not positional arguments")
	}
	visited := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "provider" {
			visited = true
		}
	})
	if !visited || *name == "" {
		return fmt.Errorf("models remove requires --provider NAME")
	}
	if err := config.RemoveProvider(c.path, *name); err != nil {
		return err
	}
	return c.output(map[string]any{"saved": true, "restart_required": true})
}

func (c *console) setModelsDeprecated(args []string) error {
	fields, fs, err := parseModelFlags("set", args)
	if err != nil {
		return err
	}
	_ = fields
	changes := modelFlagChanges(fields, fs, "provider.")
	if len(changes) == 0 {
		return fmt.Errorf("models set requires at least one setting; use --model NAME")
	}
	cfg, err := c.load()
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
	clearOtherCredential(changes, "provider.")
	fmt.Fprintln(c.errOut, "warning: `models set` is deprecated; use `models set-default` to change the default or `models add`/`models update` for extra providers")
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
		if group == "models" {
			if err := c.action(group, "show", []string{"--all"}); err != nil {
				return err
			}
		} else if err := c.action(group, "show", nil); err != nil {
			return err
		}
		choices := "1) Edit a setting  2) Validate  0) Exit\n"
		if group == "models" {
			choices = "1) Set default provider/model  2) Manage extra providers  3) List models for a provider  4) Supported providers  0) Exit\n"
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
		case "0", "q", "quit", "\x1b":
			return nil
		case "1":
			if group == "models" {
				err = c.editDefaultModels()
			} else {
				err = c.editConfig()
			}
		case "2":
			if group == "models" {
				err = c.manageExtras()
			} else {
				err = c.action(group, "validate", nil)
			}
		case "3":
			if group == "models" {
				err = c.listModelsInteractive()
			} else {
				err = fmt.Errorf("choose 0, 1 or 2")
			}
		case "4":
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
	if !config.IsValidKey(key) {
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
	return c.editDefaultModels()
}

func (c *console) editDefaultModels() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	name, err := c.prompt(fmt.Sprintf("Provider [%s] (ollama/openai/opencode-go/chatgpt): ", cfg.Provider.Name))
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

func (c *console) manageExtras() error {
	for {
		cfg, err := c.load()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		summary := "(none)"
		if len(names) > 0 {
			summary = strings.Join(names, ", ")
		}
		fmt.Fprintf(c.out, "Default: %s (model %s)\nExtra providers: %s\n", cfg.Provider.Name, cfg.Provider.Model, summary)
		fmt.Fprint(c.out, "1) Add extra provider  2) Edit extra provider  3) Remove extra provider  0) Back\n")
		choice, err := c.prompt("> ")
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "0", "q", "quit", "\x1b", "b", "back":
			return nil
		case "1":
			if err := c.addExtraInteractive(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				fmt.Fprintln(c.errOut, err)
			}
		case "2":
			if err := c.editExtraInteractive(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				fmt.Fprintln(c.errOut, err)
			}
		case "3":
			if err := c.removeExtraInteractive(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				fmt.Fprintln(c.errOut, err)
			}
		default:
			fmt.Fprintln(c.errOut, fmt.Errorf("choose one of the listed actions"))
		}
	}
}

func (c *console) addExtraInteractive() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	name, err := c.prompt("Extra provider name (ollama/openai/opencode-go/chatgpt; blank cancels): ")
	if err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	if !config.ValidProviderAdapter(name) {
		return fmt.Errorf("unknown provider %q", name)
	}
	if name == cfg.Provider.Name {
		return fmt.Errorf("providers.%s duplicates the default provider; use menu 1 to change the default", name)
	}
	if _, exists := cfg.Providers[name]; exists {
		return fmt.Errorf("provider %q already exists; choose edit instead", name)
	}
	model, err := c.prompt("Model (required): ")
	if err != nil {
		return err
	}
	if model == "" {
		return fmt.Errorf("model is required")
	}
	changes := map[string]any{"providers." + name + ".model": model}
	for _, key := range []string{"base_url", "thinking"} {
		value, err := c.prompt(key + " (Enter uses default; '-' leaves empty): ")
		if err != nil {
			return err
		}
		if value != "" && value != "-" {
			changes["providers."+name+"."+key] = value
		}
	}
	if name != "chatgpt" {
		cred, err := c.prompt("Credential: api_key_env variable name, or path for api_key_path (Enter skips; '-' leaves empty): ")
		if err != nil {
			return err
		}
		if cred != "" && cred != "-" {
			// Heuristic: values containing a slash or dot look like paths.
			if strings.Contains(cred, "/") || strings.Contains(cred, ".") {
				changes["providers."+name+".api_key_path"] = cred
			} else {
				changes["providers."+name+".api_key_env"] = cred
			}
		}
	}
	return c.confirm(changes)
}

func (c *console) editExtraInteractive() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	name, err := c.prompt("Extra provider to edit (blank cancels): ")
	if err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	current, ok := cfg.Providers[name]
	if !ok {
		if name == cfg.Provider.Name {
			return fmt.Errorf("provider %q is the default provider; use menu 1 to change it", name)
		}
		return fmt.Errorf("provider %q is not configured as an extra", name)
	}
	// Ensure the adapter name is visible even when the table omits it.
	current.Name = name
	model, err := c.prompt(fmt.Sprintf("Model [%s]: ", current.Model))
	if err != nil {
		return err
	}
	changes := map[string]any{}
	prefix := "providers." + name + "."
	if model != "" {
		changes[prefix+"model"] = model
	}
	for _, key := range []string{"base_url", "thinking"} {
		currentVal := ""
		if key == "base_url" {
			currentVal = current.BaseURL
		} else {
			currentVal = current.Thinking
		}
		value, err := c.prompt(fmt.Sprintf("%s [%s] (Enter keeps; '-' clears): ", key, currentVal))
		if err != nil {
			return err
		}
		if value == "-" {
			changes[prefix+key] = ""
		} else if value != "" {
			changes[prefix+key] = value
		}
	}
	if name != "chatgpt" {
		currentCred := current.APIKeyEnv
		if current.APIKeyPath != "" {
			currentCred = "path:" + current.APIKeyPath
		}
		cred, err := c.prompt(fmt.Sprintf("Credential [%s] (Enter keeps; '-' clears both; variable name for env, path for file): ", currentCred))
		if err != nil {
			return err
		}
		if cred == "-" {
			changes[prefix+"api_key_env"] = ""
			changes[prefix+"api_key_path"] = ""
		} else if cred != "" {
			if strings.Contains(cred, "/") || strings.Contains(cred, ".") {
				changes[prefix+"api_key_path"] = cred
				changes[prefix+"api_key_env"] = ""
			} else {
				changes[prefix+"api_key_env"] = cred
				changes[prefix+"api_key_path"] = ""
			}
		}
	}
	if len(changes) == 0 {
		return fmt.Errorf("no changes entered")
	}
	return c.confirm(changes)
}

func (c *console) removeExtraInteractive() error {
	name, err := c.prompt("Extra provider to remove (blank cancels): ")
	if err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Providers[name]; !ok {
		if name == cfg.Provider.Name {
			return fmt.Errorf("provider %q is the default provider; it cannot be removed", name)
		}
		return fmt.Errorf("provider %q is not configured as an extra", name)
	}
	answer, err := c.prompt(fmt.Sprintf("Remove extra provider %q for the next service start? [y/N] ", name))
	if err != nil {
		return err
	}
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		_, err = fmt.Fprintln(c.out, "Cancelled; no changes saved.")
		return err
	}
	if err := config.RemoveProvider(c.path, name); err != nil {
		return err
	}
	return c.output(map[string]any{"saved": true, "restart_required": true})
}

func (c *console) listModelsInteractive() error {
	cfg, err := c.load()
	if err != nil {
		return err
	}
	names := []string{cfg.Provider.Name}
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	name, err := c.prompt(fmt.Sprintf("Provider [%s] (blank uses default; available: %s): ", cfg.Provider.Name, strings.Join(names, ", ")))
	if err != nil {
		return err
	}
	if name == "" {
		name = cfg.Provider.Name
	}
	selected, ok := cfg.ProviderByName(name)
	if !ok {
		return fmt.Errorf("provider %q is not configured", name)
	}
	return c.listProviderModels(cfg, selected)
}
