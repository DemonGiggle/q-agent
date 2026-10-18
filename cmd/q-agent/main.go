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
	"q-agent/internal/config"
	"q-agent/internal/console"
	"q-agent/internal/host"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
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
	"syscall"
	"time"
)

// Set by release builds via -ldflags; untagged source builds remain dev.
var version = "dev"

func main() {
	policy, _ := redaction.New(redaction.Config{}, redaction.EnvironmentValues())
	log.SetOutput(&redaction.DiagnosticWriter{Policy: policy, Sink: redaction.Terminal, Out: os.Stderr})
	if err := mainRun(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func mainRun() error {
	args := os.Args[1:]
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
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
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
	if runtime.GOOS != "linux" {
		return fmt.Errorf("q-agent runtime requires Linux")
	}
	if len(c.Telegram.ApprovedUsers) == 0 || len(c.Telegram.ApprovedChats) == 0 {
		return fmt.Errorf("approved_user_ids and approved_chat_ids are both required")
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
	store, err := state.Open(c.StateDir, policy)
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
	// Every provider instance has its own routing identity and state, while all
	// completions reuse this transport's connection pool.
	httpClient := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	rt := &run.Runtime{Config: c, Store: store, Workspaces: workspaces, Tools: tools, Sink: bot, Options: agent.Options{Model: c.Provider.Model, Thinking: c.Provider.Thinking, MaxSteps: c.Limits.MaxSteps, MaxToolCalls: c.Limits.MaxToolCalls, Redaction: policy, Logger: log.Default()}, Report: func(err error) { log.Print(err) }}
	management := &maintenance.Controller{StateDir: c.StateDir, Version: version, Notify: bot.Send}
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
				log.Print(err)
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
	go func() { defer close(deliveryDone); bot.Deliver(deliveryCtx, store, func(err error) { log.Print(err) }) }()
	defer func() { rt.Close(); cancelDelivery(); <-deliveryDone }()
	log.Printf("serving as @%s (%s execution)", identity.Username, mode)
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
			log.Print(err)
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
			log.Printf("telegram poll updates=%d cursor=%d", len(updates), store.Cursor())
		}
		for _, update := range updates {
			message, ok := telegram.Normalize(update, identity)
			if ok {
				err = rt.Handle(ctx, message)
			} else {
				// Console-only: group messages without a mention/reply and
				// non-text updates are filtered here and never reach Handle.
				log.Printf("telegram update=%d ignored (not addressed to @%s or non-text)", update.ID, identity.Username)
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
