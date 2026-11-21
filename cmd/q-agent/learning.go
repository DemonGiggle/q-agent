package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/learning"
	"q-agent/internal/llm"
	"q-agent/internal/localtools"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

func runLearning(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || (args[0] != "configure" && args[0] != "trigger" && args[0] != "search" && args[0] != "enqueue" && args[0] != "work" && args[0] != "recall" && args[0] != "frontier") {
		return fmt.Errorf("usage: q-agent learning configure|trigger|search|enqueue|work|recall|frontier [--config FILE]; pass input on stdin")
	}

	flags := flag.NewFlagSet("learning "+args[0], flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "q-agent TOML configuration")
	var summarize bool
	if args[0] == "work" {
		flags.BoolVar(&summarize, "summarize", false, "summarize a public excerpt through local Ollama")
	}
	var chatID, topicID int64
	var selectFrontier bool
	if args[0] == "frontier" {
		flags.Int64Var(&chatID, "chat-id", 0, "authorized Telegram chat ID")
		flags.Int64Var(&topicID, "topic-id", 0, "Telegram topic ID")
		flags.BoolVar(&selectFrontier, "select", false, "show the policy-selected shortlist")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}

	app, err := config.Load(*path)
	if err != nil {
		return err
	}
	policyPath := learningPolicyPath(app.StateDir)
	cfg := learning.DefaultConfig()
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	if args[0] == "configure" {
		if err := decodeLearningConfig(input, &cfg); err != nil {
			return err
		}
		if cfg.Schedule.Enabled {
			return fmt.Errorf("learning supports manual commands only; set schedule.enabled to false")
		}
		if err := state.Atomic(policyPath, cfg); err != nil {
			return err
		}
		return enc.Encode(cfg)
	}
	cfg, err = loadLearningConfig(policyPath)
	if err != nil {
		return err
	}

	if args[0] == "frontier" {
		frontier, err := collectLearningFrontier(ctx, app, cfg, chatID, topicID, selectFrontier)
		if err != nil {
			return err
		}
		return enc.Encode(frontier)
	}

	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return fmt.Errorf("learning question exceeds 4096 bytes")
	}
	// Check against the caller's raw text before redaction and apply the
	// same exclusion policy again to the sanitized planning input.
	if args[0] != "work" && cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: string(data)}) {
		return learning.ErrDenied
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return err
	}
	if args[0] == "recall" {
		results, err := state.NewLearningRepository(app.StateDir, nil).RecallPublic(
			ctx, cfg, string(data), func(s string) bool { return !policy.ContainsSecret(s) }, 5)
		if err != nil {
			return err
		}
		return enc.Encode(results)
	}
	if args[0] == "search" || args[0] == "enqueue" || args[0] == "work" {
		if !cfg.Enabled || (args[0] != "work" && policy.ContainsSecret(string(data))) ||
			(args[0] == "search" && !cfg.Permissions.AllowsDomain(localtools.LearningSearchDomain)) {
			return learning.ErrDenied
		}
		unlock, err := state.Lock(filepath.Join(app.StateDir, "learning"))
		if err != nil {
			return err
		}
		defer unlock()
		coordinator := state.NewLearningCoordinator(app.StateDir)
		search := localtools.LearningSearch{Permissions: cfg.Permissions}
		switch args[0] {
		case "enqueue":
			id, err := coordinator.EnqueueResearch(ctx, cfg, string(data))
			if err != nil {
				return err
			}
			return enc.Encode(struct {
				RunID string `json:"run_id"`
			}{RunID: id})
		case "work":
			var model learning.ModelProvider
			if summarize {
				if app.Provider.Name != "ollama" {
					return fmt.Errorf("work --summarize requires the configured default provider to be Ollama")
				}
				model, err = llm.NewLearningOllama(app.Provider.BaseURL, app.Provider.Model)
				if err != nil {
					return err
				}
			}
			fetch := localtools.LearningFetch{Permissions: cfg.Permissions}
			result, err := coordinator.WorkResearch(ctx, cfg, strings.TrimSpace(string(data)), func(s string) bool {
				return !policy.ContainsSecret(s) &&
					!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: s})
			}, search, fetch, localtools.LearningDocumentReader{}, model)
			if result.RunID != "" {
				err = errors.Join(err, enc.Encode(result))
			}
			return err
		}
		discovery, err := coordinator.Discover(ctx, cfg, search, string(data))
		if discovery.RunID != "" {
			err = errors.Join(err, enc.Encode(discovery))
		}
		return err
	}
	preview, err := learning.DryRun(ctx, cfg, policy.Redact(string(data)), time.Now(),
		state.LearningAudit{Dir: filepath.Join(app.StateDir, "learning", "audit")})
	if err != nil {
		return err
	}
	return enc.Encode(preview)
}

func collectLearningFrontier(ctx context.Context, app config.Config, cfg learning.Config, chatID, topicID int64, selectFrontier bool) (any, error) {
	if !cfg.Enabled || !app.Memory.Enabled || chatID == 0 || topicID < 0 {
		return nil, learning.ErrDenied
	}
	visibility := learning.GroupChat
	if chatID > 0 {
		if !cfg.Permissions.IncludePrivateChats || !slices.Contains(app.Telegram.AllowedUsers, chatID) {
			return nil, learning.ErrDenied
		}
		visibility = learning.PrivateChat
	} else if !cfg.Permissions.IncludeGroupChats || !slices.Contains(app.Telegram.AllowedGroups, chatID) {
		return nil, learning.ErrDenied
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return nil, err
	}
	allow := func(text string) bool {
		return !strings.Contains(text, redaction.Marker) && !policy.ContainsSecret(text) &&
			!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
	}
	const platformName = "telegram"
	reader := state.NewFileLearningReader(app.StateDir, policy, func(_ context.Context, scope learning.Scope) bool {
		return scope.Platform == platformName && scope.ChatID == chatID && scope.Visibility == visibility &&
			scope.TopicID != nil && *scope.TopicID == topicID
	}, func(_ learning.Scope, turn learning.ConversationTurn) bool {
		return !allow(turn.Transcript)
	})
	unlock, err := state.Lock(filepath.Join(app.StateDir, "learning"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	now := time.Now().UTC()
	repo := state.NewLearningRepository(app.StateDir, nil)
	query := learning.ConversationQuery{
		Scope: learning.Scope{Platform: platformName, ChatID: chatID, TopicID: &topicID, Visibility: visibility},
		From:  now.Add(-48 * time.Hour), Until: now,
	}
	if selectFrontier {
		return repo.SelectConversationFrontier(ctx, reader, query, now, allow, cfg.Selection)
	}
	return repo.CollectConversationFrontier(ctx, reader, query, now, allow)
}

func learningPolicyPath(stateDir string) string {
	return filepath.Join(stateDir, "learning", "policy.json")
}

func loadLearningConfig(path string) (learning.Config, error) {
	cfg := learning.DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	err = decodeLearningConfig(f, &cfg)
	return cfg, err
}

// Read the opt-in once at startup. Chat never polls the learning policy, and
// a missing or invalid learning policy leaves history capture disabled.
func learningMemoryLimits(app config.Config) state.MemoryLimits {
	limits := state.MemoryLimits{
		MaxBytes: app.Memory.MaxBytes, MaxPendingBytes: app.Memory.MaxPendingBytes,
	}
	if !app.Memory.Enabled {
		return limits
	}
	cfg, err := loadLearningConfig(learningPolicyPath(app.StateDir))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Print("[LEARNING] Invalid or unreadable policy; conversation history capture disabled")
		}
		return limits
	}
	limits.LearnFromDMs = cfg.Enabled && cfg.Permissions.IncludePrivateChats
	limits.LearnFromGroups = cfg.Enabled && cfg.Permissions.IncludeGroupChats
	return limits
}

func decodeLearningConfig(input io.Reader, cfg *learning.Config) error {
	const maxSize = 64 << 10
	data, err := io.ReadAll(io.LimitReader(input, maxSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxSize {
		return fmt.Errorf("learning configuration exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("invalid learning configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("learning configuration must contain one JSON object")
	}
	if cfg == nil {
		return fmt.Errorf("learning configuration requires an object")
	}
	return cfg.Validate()
}
