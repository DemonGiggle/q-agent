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
	"q-agent/internal/redaction"
	run "q-agent/internal/runtime"
	"q-agent/internal/sandbox"
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
	values = append(values, os.Getenv(c.Telegram.TokenEnv), os.Getenv(c.Provider.APIKeyEnv))
	if c.Provider.APIKeyPath != "" {
		// Best effort for early diagnostics; serve() enforces the file strictly.
		if fileKey, err := c.Provider.APIKey(); err == nil && fileKey != "" {
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
	providerKey, err := c.Provider.APIKey()
	if err != nil {
		return err
	}
	if c.Provider.Name != "ollama" && providerKey == "" {
		if c.Provider.APIKeyPath != "" {
			return fmt.Errorf("missing provider credential file %s (empty)", c.Provider.APIKeyPath)
		}
		return fmt.Errorf("missing provider credential environment variable %s", c.Provider.APIKeyEnv)
	}
	mode := state.ExecutionHost
	var workspaces run.Workspaces = &workspace.HostManager{Dir: filepath.Join(c.StateDir, "host-workspaces")}
	environment, removed := redaction.FilterEnvironment(os.Environ(), c.Telegram.TokenEnv, c.Provider.APIKeyEnv)
	policy = policy.WithValues(removed)
	if token != "" {
		policy = policy.WithValues([]string{token})
	}
	if providerKey != "" {
		policy = policy.WithValues([]string{providerKey})
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
	providerConfig := llm.Config{BaseURL: c.Provider.BaseURL, APIKey: providerKey, HTTP: httpClient}
	rt := &run.Runtime{Config: c, Store: store, Workspaces: workspaces, Tools: tools, Sink: bot, Options: agent.Options{Model: c.Provider.Model, Thinking: c.Provider.Thinking, MaxSteps: c.Limits.MaxSteps, MaxToolCalls: c.Limits.MaxToolCalls, Redaction: policy, Logger: log.Default()}, Report: func(err error) { log.Print(err) }}
	rt.Providers = func(conversation state.Conversation) (llm.Provider, error) {
		p, err := llm.New(c.Provider.Name, providerConfig)
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
	if err := rt.StartScheduler(ctx); err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
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
