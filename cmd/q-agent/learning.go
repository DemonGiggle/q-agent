package main

import (
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
	"q-agent/internal/trust"
)

func runLearning(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || !slices.Contains([]string{"configure", "trigger", "search", "enqueue", "schedule", "work", "recall", "note", "frontier", "status", "pause", "resume", "audit"}, args[0]) {
		return fmt.Errorf("usage: q-agent learning configure|trigger|search|enqueue|schedule|work|recall|note|frontier|status|pause|resume|audit [--config FILE]; pass input on stdin")
	}

	flags := flag.NewFlagSet("learning "+args[0], flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "q-agent TOML configuration")
	var summarize, claims, removeSchedule bool
	if args[0] == "schedule" {
		flags.BoolVar(&removeSchedule, "remove", false, "remove a run from the overnight queue")
	}
	var sources int
	if args[0] == "enqueue" {
		flags.IntVar(&sources, "sources", 1, "verified source target (1 or 2)")
	}
	if args[0] == "work" {
		flags.BoolVar(&summarize, "summarize", false, "summarize a public excerpt through local Ollama")
		flags.BoolVar(&claims, "claims", false, "draft a claim from verified excerpts through local Ollama")
	}
	var frontier frontierOptions
	if args[0] == "frontier" {
		flags.Int64Var(&frontier.chatID, "chat-id", 0, "authorized Telegram chat ID")
		flags.Int64Var(&frontier.topicID, "topic-id", 0, "Telegram topic ID")
		flags.BoolVar(&frontier.public, "public", false, "use the public knowledge frontier")
		flags.BoolVar(&frontier.selectOnly, "select", false, "show the policy-selected shortlist")
		flags.StringVar(&frontier.origin, "origin", "", "filter by candidate origin")
		flags.StringVar(&frontier.status, "status", "candidate", "filter by status or use all")
		flags.StringVar(&frontier.enqueue, "enqueue", "", "queue a selected ID (or next in the public frontier)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if summarize && claims {
		return fmt.Errorf("choose either --summarize or --claims")
	}

	app, err := config.Load(*path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	if args[0] == "configure" {
		cfg, err := state.ReadLearningPolicy(input)
		if err != nil {
			return err
		}
		if err := state.Atomic(learningPolicyPath(app.StateDir), cfg); err != nil {
			return err
		}
		return enc.Encode(cfg)
	}
	cfg, err := state.LoadLearningPolicy(app.StateDir)
	if err != nil {
		return err
	}

	if args[0] == "frontier" {
		result, err := collectLearningFrontier(ctx, app, cfg, frontier, input)
		if err != nil {
			return err
		}
		return enc.Encode(result)
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
	runIDInput := slices.Contains([]string{"work", "status", "pause", "resume", "audit", "note", "schedule"}, args[0])
	if !runIDInput && cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: string(data)}) {
		return learning.ErrDenied
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return err
	}
	coordinator := state.NewLearningCoordinator(app.StateDir)
	allow := func(s string) bool { return !policy.ContainsSecret(s) }
	if args[0] == "schedule" {
		unlock, err := state.Lock(filepath.Join(app.StateDir, "learning"))
		if err != nil {
			return err
		}
		defer unlock()
		id := strings.TrimSpace(string(data))
		if removeSchedule {
			err = coordinator.RemoveScheduledResearch(id)
		} else {
			err = coordinator.ScheduleResearch(ctx, cfg, id, func(s string) bool {
				return allow(s) && !strings.Contains(s, redaction.Marker) && !trust.Suspicious(s)
			})
		}
		if err != nil {
			return err
		}
		return enc.Encode(struct {
			RunID     string `json:"run_id"`
			Scheduled bool   `json:"scheduled"`
		}{RunID: id, Scheduled: !removeSchedule})
	}
	writeResearchResult := func(id string, result any) error {
		// Checkpoints already committed at this boundary. Cancellation and
		// audit I/O errors must not change the reported research outcome.
		if _, err := coordinator.ExportResearchAudit(context.WithoutCancel(ctx), cfg, id, allow); err != nil {
			log.Printf("learning audit pending for run %s (replay with q-agent learning audit)", id)
		}
		return enc.Encode(result)
	}
	if args[0] == "status" {
		result, err := coordinator.ResearchStatus(ctx, cfg, strings.TrimSpace(string(data)), allow)
		if err != nil {
			return err
		}
		return enc.Encode(result)
	}
	if args[0] == "audit" {
		events, err := coordinator.ExportResearchAudit(ctx, cfg, strings.TrimSpace(string(data)), allow)
		if err != nil {
			return err
		}
		return enc.Encode(events)
	}

	if args[0] == "pause" || args[0] == "resume" {
		unlock, err := state.Lock(filepath.Join(app.StateDir, "learning"))
		if err != nil {
			return err
		}
		defer unlock()
		result, err := coordinator.SetResearchPaused(
			ctx, cfg, strings.TrimSpace(string(data)), allow, args[0] == "pause")
		if err != nil {
			return err
		}
		return writeResearchResult(result.RunID, result)
	}

	if args[0] == "recall" {
		results, err := state.NewLearningRepository(app.StateDir, nil).RecallPublic(
			ctx, cfg, string(data), func(s string) bool { return !policy.ContainsSecret(s) }, 5)
		if err != nil {
			return err
		}
		return enc.Encode(results)
	}
	if args[0] == "note" {
		note, err := state.NewLearningRepository(app.StateDir, nil).ReadPublicNote(
			ctx, cfg, strings.TrimSpace(string(data)), allow)
		if err != nil {
			return err
		}
		body := note.Markdown(cfg.SourcePolicy, time.Now(), 32<<10)
		if body == "" {
			return fmt.Errorf("rendered learning note exceeds 32 KiB")
		}
		_, err = io.WriteString(output, body)
		return err
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
		search := localtools.LearningSearch{Permissions: cfg.Permissions}
		switch args[0] {
		case "enqueue":
			id, err := coordinator.EnqueueResearch(ctx, cfg, string(data), sources)
			if err != nil {
				return err
			}
			return writeResearchResult(id, struct {
				RunID string `json:"run_id"`
			}{RunID: id})
		case "work":
			var model learning.ModelProvider
			if summarize || claims {
				if app.Provider.Name != "ollama" {
					return fmt.Errorf("work --summarize and --claims require the configured default provider to be Ollama")
				}
				model, err = llm.NewLearningOllama(app.Provider.BaseURL, app.Provider.Model)
				if err != nil {
					return err
				}
			}
			fetch := localtools.LearningFetch{Permissions: cfg.Permissions}
			work := coordinator.WorkResearch
			if claims {
				work = coordinator.WorkResearchWithClaims
			}
			result, err := work(ctx, cfg, strings.TrimSpace(string(data)), func(s string) bool {
				return !policy.ContainsSecret(s) &&
					!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: s})
			}, search, fetch, localtools.LearningDocumentReader{}, model)
			if result.RunID != "" {
				err = errors.Join(err, writeResearchResult(result.RunID, result))
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

type frontierOptions struct {
	chatID, topicID int64
	public          bool
	selectOnly      bool
	origin, status  string
	enqueue         string
}

func collectLearningFrontier(ctx context.Context, app config.Config, cfg learning.Config, options frontierOptions, input io.Reader) (any, error) {
	if !cfg.Enabled {
		return nil, learning.ErrDenied
	}
	if (options.public && (options.chatID != 0 || options.topicID != 0)) ||
		(options.selectOnly && options.enqueue != "") ||
		((options.selectOnly || options.enqueue != "") && (options.status != "candidate" || options.origin != "")) ||
		!slices.Contains([]string{"", "conversation", "gap", "association"}, options.origin) ||
		!slices.Contains([]string{"candidate", "queued", "resolved", "unresolved", "dismissed", "all"}, options.status) {
		return nil, fmt.Errorf("invalid learning frontier options")
	}
	partition := learning.Partition{Public: options.public}
	if !options.public {
		if !app.Memory.Enabled || options.chatID == 0 || options.topicID < 0 {
			return nil, learning.ErrDenied
		}
		visibility := learning.GroupChat
		if options.chatID > 0 {
			if !cfg.Permissions.IncludePrivateChats || !slices.Contains(app.Telegram.AllowedUsers, options.chatID) {
				return nil, learning.ErrDenied
			}
			visibility = learning.PrivateChat
		} else if !cfg.Permissions.IncludeGroupChats || !slices.Contains(app.Telegram.AllowedGroups, options.chatID) {
			return nil, learning.ErrDenied
		}
		partition.Scope = &learning.Scope{Platform: "telegram", ChatID: options.chatID, TopicID: &options.topicID, Visibility: visibility}
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return nil, err
	}
	allow := func(text string) bool {
		return !strings.Contains(text, redaction.Marker) && !policy.ContainsSecret(text) &&
			!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
	}
	unlock, err := state.Lock(filepath.Join(app.StateDir, "learning"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	now := time.Now().UTC()
	repo := state.NewLearningRepository(app.StateDir, nil)
	var entries []learning.FrontierEntry
	if options.public {
		entries, err = repo.PublicFrontier(ctx, cfg, now, func(text string) bool {
			return allow(text) && !trust.Suspicious(text)
		})
	} else {
		scope := *partition.Scope
		reader := state.NewFileLearningReader(app.StateDir, policy, func(_ context.Context, requested learning.Scope) bool {
			return requested.Platform == scope.Platform && requested.ChatID == scope.ChatID && requested.Visibility == scope.Visibility &&
				requested.TopicID != nil && *requested.TopicID == *scope.TopicID
		}, func(_ learning.Scope, turn learning.ConversationTurn) bool {
			return !allow(turn.Transcript)
		})
		query := learning.ConversationQuery{Scope: scope, From: now.Add(-48 * time.Hour), Until: now}
		entries, err = repo.ConversationFrontier(ctx, reader, query, now, allow)
	}
	if err != nil {
		return nil, err
	}
	coordinator := state.NewLearningCoordinator(app.StateDir)
	entries, err = coordinator.RefreshFrontier(ctx, partition, entries)
	if err != nil {
		return nil, err
	}
	if options.enqueue != "" {
		var approved string
		if !options.public {
			data, err := io.ReadAll(io.LimitReader(input, 2001))
			if err != nil {
				return nil, err
			}
			if len(data) > 2000 {
				return nil, fmt.Errorf("public question exceeds 2000 bytes")
			}
			approved = string(data)
		}
		return coordinator.QueueFrontier(ctx, cfg, partition, entries, options.enqueue, approved, now, func(text string) bool {
			return allow(text) && !trust.Suspicious(text)
		})
	}
	if options.selectOnly {
		return learning.SelectFrontier(entries, cfg.Selection, now, allow), nil
	}
	return state.ListLearningFrontier(entries, now, allow, options.origin, options.status), nil
}

func learningPolicyPath(stateDir string) string {
	return filepath.Join(stateDir, "learning", "policy.json")
}

// Load initial opt-ins for learning features when the service starts.
func startupLearningConfig(app config.Config) learning.Config {
	if !app.Memory.Enabled {
		return learning.Config{}
	}
	cfg, err := state.LoadLearningPolicy(app.StateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Print("[LEARNING] Invalid or unreadable policy; automatic learning disabled")
		}
		return learning.Config{}
	}
	return cfg
}

func learningMemoryLimits(app config.Config, cfg learning.Config) state.MemoryLimits {
	return state.MemoryLimits{
		MaxBytes: app.Memory.MaxBytes, MaxPendingBytes: app.Memory.MaxPendingBytes,
		LearnFromDMs:    app.Memory.Enabled && cfg.Enabled && cfg.Permissions.IncludePrivateChats,
		LearnFromGroups: app.Memory.Enabled && cfg.Enabled && cfg.Permissions.IncludeGroupChats,
	}
}
