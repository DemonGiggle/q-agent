package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"q-agent/internal/agent"
	"q-agent/internal/chatgptoauth"
	"q-agent/internal/config"
	"q-agent/internal/console"
	"q-agent/internal/host"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/logging"
	"q-agent/internal/maintenance"
	"q-agent/internal/redaction"
	run "q-agent/internal/runtime"
	"q-agent/internal/sandbox"
	"q-agent/internal/service"
	"q-agent/internal/state"
	"q-agent/internal/telegram"
	"q-agent/internal/update"
	"q-agent/internal/workspace"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Set by release builds via -ldflags; untagged source builds remain dev.
var version = "dev"

func main() {
	policy, _ := redaction.New(redaction.Config{}, redaction.EnvironmentValues())
	log.SetOutput(&redaction.DiagnosticWriter{Policy: policy, Sink: redaction.Terminal, Out: os.Stderr})
	if err := mainRun(); err != nil {
		// mainRun may install the configured policy. Redact before quoting so
		// escaping cannot hide credential assignments from the diagnostic writer.
		if writer, ok := log.Writer().(*redaction.DiagnosticWriter); ok {
			policy = writer.Policy
		}
		log.Printf("[SERVICE] Command failed | error=%q", policy.Text(redaction.Terminal, err.Error()))
		os.Exit(1)
	}
}
func mainRun() error {
	args := os.Args[1:]
	if len(args) > 1 && args[0] == "service" && args[1] == "log-run" {
		flags := flag.NewFlagSet("service log-run", flag.ContinueOnError)
		path := flags.String("log-path", "", "absolute log path")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if !filepath.IsAbs(*path) || flags.NArg() == 0 {
			return fmt.Errorf("internal log supervisor requires an absolute --log-path and a command")
		}
		return logging.Run(*path, flags.Args())
	}
	if len(args) > 1 && args[0] == "service" && args[1] == "maintain" {
		flags := flag.NewFlagSet("service maintain", flag.ContinueOnError)
		dir := flags.String("state-dir", "", "absolute state directory")
		id := flags.String("id", "", "armed maintenance request ID")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if !filepath.IsAbs(*dir) || *id == "" || flags.NArg() != 0 {
			return fmt.Errorf("internal maintenance requires --state-dir and --id")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
		defer cancel()
		return (&maintenance.Controller{StateDir: *dir}).Run(ctx, *id)
	}
	if len(args) > 0 && args[0] == "service" {
		err := service.Run(args[1:], service.Options{Stdout: os.Stdout, Stderr: os.Stderr})
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(args) > 0 && args[0] == "update" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err := update.Run(ctx, args[1:], update.Options{
			CurrentVersion: version, Stdout: os.Stdout, Stderr: os.Stderr,
		})
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if handled, err := console.Run(args, os.Stdin, os.Stdout, os.Stderr); handled {
		return err
	}
	if len(args) >= 2 && args[0] == "worker" {
		return sandbox.Worker(args[1], os.Stdin, os.Stdout)
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if command == "version" || command == "--version" {
		if len(args) != 0 {
			return fmt.Errorf("q-agent version does not accept arguments")
		}
		fmt.Println(version)
		return nil
	}
	if command == "image" {
		if len(args) == 0 || args[0] != "build" {
			return fmt.Errorf("usage: q-agent image build [--config FILE]")
		}
		command = "image build"
		args = args[1:]
	}
	if command != "serve" && command != "image build" {
		return fmt.Errorf("unknown command %q; use q-agent --help", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "TOML configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	cfg, rep, err := config.LoadMigrated(*path)
	if err != nil {
		return err
	}
	if rep.Migrated {
		log.Printf("[SERVICE] Migrated config (%s); backup: %s", strings.Join(rep.Changes, "; "), rep.Backup)
	}
	c := cfg
	values := redaction.EnvironmentValues()
	values = append(values, os.Getenv(c.Telegram.TokenEnv))
	for _, provider := range configuredProviders(c) {
		values = append(values, os.Getenv(provider.APIKeyEnv))
		// Best effort for early diagnostics; serve enforces credential files.
		if fileKey, err := provider.APIKey(); err == nil && fileKey != "" {
			values = append(values, fileKey)
		}
	}
	if c.Telegram.TokenPath != "" {
		// Best effort for early diagnostics; serve() enforces the file strictly.
		if fileToken, err := c.Telegram.Token(); err == nil && fileToken != "" {
			values = append(values, fileToken)
		}
	}
	policy, _ := redaction.New(redaction.Config{}, values)
	log.SetOutput(&redaction.DiagnosticWriter{Policy: policy, Sink: redaction.Terminal, Out: os.Stderr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	unlock, err := state.Lock(c.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if command == "image build" {
		if err = workspace.BuildImage(ctx, c.Sandbox.BaseImage, c.Sandbox.MMdebstrap, &redaction.DiagnosticWriter{Policy: policy, Sink: redaction.Terminal, Out: os.Stderr}); err != nil {
			return err
		}
		fmt.Println("Base image ready:", c.Sandbox.BaseImage)
		return nil
	}
	return serve(ctx, c, policy, telegram.New(botToken(c)))
}

func botToken(c config.Config) string {
	// Best effort for client construction; serve() resolves strictly and
	// reports permission errors before any network work.
	if token, err := c.Telegram.Token(); err == nil {
		return token
	}
	return ""
}

func serve(ctx context.Context, c config.Config, policy *redaction.Policy, bot *telegram.Client) error {
	return serveWithMaintenance(ctx, c, policy, bot, &maintenance.Controller{})
}

// Keep transport/process injection internal so lifecycle integration tests can
// exercise the real serve loop against disposable units and offline endpoints.
func serveWithMaintenance(ctx context.Context, c config.Config, policy *redaction.Policy, bot *telegram.Client, management *maintenance.Controller) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("q-agent runtime requires Linux")
	}
	if len(c.Telegram.AllowedUsers) == 0 && len(c.Telegram.AllowedGroups) == 0 {
		return fmt.Errorf("telegram.allowed_users and/or telegram.allowed_groups are required")
	}
	token, err := c.Telegram.Token()
	if err != nil {
		return err
	}
	if token == "" {
		if c.Telegram.TokenPath != "" {
			return fmt.Errorf("missing Telegram credential file %s (empty)", c.Telegram.TokenPath)
		}
		return fmt.Errorf("missing Telegram credential environment variable %s", c.Telegram.TokenEnv)
	}
	providerKeys := map[string]string{}
	credentialEnvs := []string{c.Telegram.TokenEnv}
	for name, provider := range configuredProviders(c) {
		if provider.Name == "chatgpt" {
			// Subscription auth: no API key. Fail fast with a usable token
			// so the operator runs `q-agent login chatgpt` before serve.
			// Load alone only checks file shape; EnsureValid also honors
			// expiry by refreshing now and fails when no refresh token
			// can produce a usable access token.
			preflightCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			tokens, err := chatgptoauth.EnsureValid(preflightCtx, &http.Client{Timeout: 30 * time.Second}, c.ChatGPT.OAuthPath)
			cancel()
			if err != nil {
				return fmt.Errorf("chatgpt OAuth for provider %s: %w", name, err)
			}
			policy = policy.WithValues([]string{tokens.AccessToken, tokens.RefreshToken})
			redaction.AddSecrets(tokens.AccessToken, tokens.RefreshToken)
			providerKeys[name] = ""
			continue
		}
		key, err := provider.APIKey()
		if err != nil {
			return err
		}
		if provider.Name != "ollama" && key == "" {
			return fmt.Errorf("missing credential for provider %s", name)
		}
		providerKeys[name] = key
		credentialEnvs = append(credentialEnvs, provider.APIKeyEnv)
		if key != "" {
			policy = policy.WithValues([]string{key})
		}
	}
	mode := state.ExecutionHost
	var workspaces run.Workspaces = &workspace.HostManager{Dir: filepath.Join(c.StateDir, "host-workspaces")}
	environment, removed := redaction.FilterEnvironment(os.Environ(), credentialEnvs...)
	policy = policy.WithValues(removed)
	if token != "" {
		policy = policy.WithValues([]string{token})
	}

	var tools run.ToolFactory = func(root string) agent.Tools {
		return &host.Runner{Options: localtools.Options{WorkingDir: root, Environment: environment, ShellTimeout: c.ShellDuration()}}
	}
	if c.Sandbox.Enabled {
		mode = state.ExecutionSandbox
		if err := workspace.CheckImage(c.Sandbox.BaseImage); err != nil {
			return err
		}
		options, err := sandbox.Resolve(sandbox.Options{Bubblewrap: c.Sandbox.Bubblewrap, NewUIDMap: c.Sandbox.NewUIDMap, NewGIDMap: c.Sandbox.NewGIDMap, Worker: c.Sandbox.Worker, ShellTimeout: c.ShellDuration()})
		if err != nil {
			return err
		}
		if err = checkMapping(c.StateDir, options); err != nil {
			return err
		}
		workspaces = &workspace.Manager{Dir: filepath.Join(c.StateDir, "workspaces"), Image: c.Sandbox.BaseImage, Sandbox: options}
		tools = func(root string) agent.Tools { return &sandbox.Runner{Options: options, RootFS: root} }
	}
	store, err := state.Open(c.StateDir, policy, state.MemoryLimits{MaxBytes: c.Memory.MaxBytes, MaxPendingBytes: c.Memory.MaxPendingBytes})
	if err != nil {
		return err
	}
	if err = store.SetExecutionMode(mode); err != nil {
		return fmt.Errorf("prepare execution mode: %w", err)
	}
	identity, err := bot.Me(ctx)
	if err != nil {
		return err
	}
	if identity.Username == "" {
		return fmt.Errorf("Telegram bot username missing")
	}
	// Overwrite a stale command menu left by a previous bot implementation.
	// The menu is server-side per bot token; Telegram clients fetch it on
	// open. Best effort: polling still works with an empty menu.
	func() {
		syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := bot.SyncCommands(syncCtx); err != nil {
			log.Printf("[TELEGRAM] Command menu synchronization failed | error=%q", policy.Text(redaction.Terminal, err.Error()))
		}
	}()
	// Every provider instance has its own routing identity and state, while all
	// completions reuse this transport's connection pool.
	httpClient := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	rt := &run.Runtime{Config: c, Store: store, Workspaces: workspaces, Tools: tools, Sink: bot, Version: version, Options: agent.Options{Model: c.Provider.Model, Thinking: c.Provider.Thinking, MaxSteps: c.Limits.MaxSteps, Redaction: policy, Logger: log.Default()}, Report: func(err error) {
		log.Printf("[SERVICE] Runtime error reported | error=%q", policy.Text(redaction.Terminal, err.Error()))
	}}
	management.StateDir, management.Version, management.Notify = c.StateDir, version, bot.Send
	if len(management.ServeArgv) == 0 && len(os.Args) > 1 && os.Args[1] == "serve" {
		management.ServeArgv = append([]string(nil), os.Args[1:]...)
	}
	if c.Sandbox.Enabled {
		management.Worker = c.Sandbox.Worker
	}
	rt.Maintenance = management
	rt.Providers = configuredProviderFactory(c, providerKeys, httpClient)
	if err := rt.StartScheduler(ctx); err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
	if err := management.MarkReady(); err != nil {
		rt.Close()
		return fmt.Errorf("maintenance readiness: %w", err)
	}
	managementCtx, cancelManagement := context.WithCancel(ctx)
	managementDone := make(chan struct{})
	go func() {
		defer close(managementDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := management.Report(managementCtx, store); err != nil {
				log.Printf("[SERVICE] Maintenance status reporting failed | error=%q", policy.Text(redaction.Terminal, err.Error()))
			}
			select {
			case <-managementCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { cancelManagement(); <-managementDone }()
	deliveryCtx, cancelDelivery := context.WithCancel(context.Background())
	defer cancelDelivery()
	deliveryDone := make(chan struct{})
	go func() {
		defer close(deliveryDone)
		bot.Deliver(deliveryCtx, store, func(err error) {
			log.Printf("[TELEGRAM] Reply delivery failed | error=%q", policy.Text(redaction.Terminal, err.Error()))
		})
	}()
	defer func() { rt.Close(); cancelDelivery(); <-deliveryDone }()
	log.Printf("[SERVICE] Bot started | username=@%s execution=%s", identity.Username, mode)
	for ctx.Err() == nil {
		updates, err := bot.Updates(ctx, store.Cursor()+1)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var api *telegram.APIError
			if errors.As(err, &api) && (api.Code == 401 || api.Code == 409) {
				return err
			}
			log.Printf("[TELEGRAM] Update polling failed; retrying | retry_in=2s error=%q", policy.Text(redaction.Terminal, err.Error()))
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		sort.Slice(updates, func(i, j int) bool { return updates[i].ID < updates[j].ID })
		if len(updates) > 0 {
			log.Printf("[TELEGRAM] Updates received | updates=%d cursor=%d", len(updates), store.Cursor())
		}
		for _, update := range updates {
			// Tap every human message into the companion recent buffer so the
			// sweep sees N hours of group chatter, not only mentions. Tapping
			// is bounded (per-chat caps + recent_window expiry + consume after
			// processed sweeps) and never fails polling; unauthorized, private,
			// and group-policy-off taps are skipped inside ObserveTapMessage.
			if tapMsg, tapText, tapOK := telegram.ExtractTap(update); tapOK {
				rt.ObserveTapMessage(tapMsg, tapText, time.Now())
			}
			message, ok := telegram.Normalize(update, identity)
			if ok {
				err = rt.Handle(ctx, message)
			} else {
				// Console-only: group messages without a mention/reply and
				// non-text updates are filtered here and never reach Handle.
				// They are still tapped above for the companion recent buffer.
				log.Printf("[TELEGRAM] Update ignored | update=%d reason=%q", update.ID, "not addressed to @"+identity.Username+" or non-text")
				_, err = store.Accept(update.ID)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func checkMapping(dir string, o sandbox.Options) error {
	want := struct{ UID, GID, UIDBase, GIDBase int }{os.Getuid(), os.Getgid(), o.UIDBase, o.GIDBase}
	path := filepath.Join(dir, "mapping.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state.Atomic(path, want)
	}
	if err != nil {
		return err
	}
	saved := struct{ UID, GID, UIDBase, GIDBase int }{}
	if json.Unmarshal(data, &saved) != nil || saved != want {
		return fmt.Errorf("service-user/subordinate mapping changed; restore the original allocation")
	}
	return nil
}

func configuredProviders(c config.Config) map[string]config.Provider {
	providers := make(map[string]config.Provider, len(c.Providers)+1)
	for name := range c.Providers {
		p, _ := c.ProviderByName(name)
		providers[name] = p
	}
	providers[c.Provider.Name] = c.Provider
	return providers
}

func configuredProviderFactory(c config.Config, providerKeys map[string]string, httpClient *http.Client) run.ProviderFactory {
	return func(conversation state.Conversation) (llm.Provider, error) {
		settings, ok := c.ProviderByName(conversation.Provider)
		if !ok {
			return nil, fmt.Errorf("provider %q is not configured", conversation.Provider)
		}
		if settings.Name == "chatgpt" {
			p, err := llm.New(settings.Name, llm.Config{BaseURL: settings.BaseURL, ChatGPTOAuthPath: c.ChatGPT.OAuthPath, HTTP: httpClient})
			if err != nil {
				return nil, err
			}
			if conversation.Identity != "" {
				if identity, ok := p.(interface{ RestoreSessionIdentity(string) error }); ok {
					if err = identity.RestoreSessionIdentity(conversation.Identity); err != nil {
						return nil, err
					}
				}
			}
			return p, nil
		}
		p, err := llm.New(settings.Name, llm.Config{BaseURL: settings.BaseURL, APIKey: providerKeys[settings.Name], HTTP: httpClient})
		if err != nil {
			return nil, err
		}
		if conversation.Identity != "" {
			if identity, ok := p.(interface{ RestoreSessionIdentity(string) error }); ok {
				if err = identity.RestoreSessionIdentity(conversation.Identity); err != nil {
					return nil, err
				}
			}
		}
		return p, nil
	}
}
