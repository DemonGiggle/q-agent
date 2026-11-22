package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
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
	if len(args) == 0 || !slices.Contains([]string{"configure", "trigger", "search", "enqueue", "schedule", "work", "recall", "note", "claim-history", "claim-edit", "frontier", "status", "pause", "resume", "cancel", "audit", "export", "delete", "archive", "metrics", "digest"}, args[0]) {
		return fmt.Errorf("usage: q-agent learning configure|trigger|search|enqueue|schedule|work|recall|note|claim-history|claim-edit|frontier|status|pause|resume|cancel|audit|export|delete|archive|metrics|digest [--config FILE]; pass input on stdin")
	}

	flags := flag.NewFlagSet("learning "+args[0], flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "q-agent TOML configuration")
	var digestDays, digestLimit int
	if args[0] == "digest" {
		flags.IntVar(&digestDays, "days", 1, "number of 24-hour periods to summarize")
		flags.IntVar(&digestLimit, "limit", 5, "maximum notes")
	}
	var metricDays int
	if args[0] == "metrics" {
		flags.IntVar(&metricDays, "days", 7, "include this many days of audit events")
	}
	var summarize, claims, removeSchedule, restoreNote bool
	if args[0] == "archive" {
		flags.BoolVar(&restoreNote, "restore", false, "restore an archived note to recall")
	}
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
	if slices.Contains([]string{"frontier", "export", "delete"}, args[0]) {
		flags.Int64Var(&frontier.chatID, "chat-id", 0, "authorized Telegram chat ID")
		flags.Int64Var(&frontier.topicID, "topic-id", 0, "Telegram topic ID")
		flags.BoolVar(&frontier.public, "public", false, "use public knowledge")
	}
	var deleteCandidate, requestID string
	if args[0] == "delete" {
		flags.StringVar(&deleteCandidate, "candidate", "", "erase only this conversation candidate")
		flags.StringVar(&requestID, "request-id", "", "reuse a receipt ID for an idempotent retry")
	}
	if args[0] == "frontier" {
		flags.StringVar(&frontier.pin, "pin", "", "prioritize a visible candidate")
		flags.StringVar(&frontier.mute, "mute", "", "skip a candidate during selection")
		flags.StringVar(&frontier.dismiss, "dismiss", "", "dismiss a candidate")
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
	if slices.Contains([]string{"frontier", "work", "enqueue", "search", "schedule", "pause", "resume", "claim-edit", "archive", "delete", "export"}, args[0]) {
		unlock, err := state.Lock(filepath.Join(app.StateDir, "learning"))
		if err != nil {
			return err
		}
		defer unlock()
	}
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	if args[0] == "configure" {
		cfg, err := state.ReadLearningPolicy(input)
		if err != nil {
			return err
		}
		if prior, err := state.LoadLearningPolicy(app.StateDir); err == nil && reflect.DeepEqual(prior, cfg) {
			return enc.Encode(cfg)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := state.Atomic(learningPolicyPath(app.StateDir), cfg); err != nil {
			return err
		}
		id := rand.Text()
		audit := state.LearningAudit{Dir: filepath.Join(app.StateDir, "learning", "audit")}
		if err := audit.Append(context.WithoutCancel(ctx), learning.Event{SchemaVersion: learning.SchemaVersion,
			Type: learning.PolicyConfigured, ID: id, RunID: id, At: time.Now().UTC(), Outcome: "applied", ObjectRefs: []string{id}}); err != nil {
			log.Print("learning policy applied; failed to write audit metadata")
		}
		return enc.Encode(cfg)
	}
	if args[0] == "metrics" {
		if metricDays < 1 || metricDays > 365 {
			return fmt.Errorf("learning metrics days must be between 1 and 365")
		}
		now := time.Now().UTC()
		audit := state.LearningAudit{Dir: filepath.Join(app.StateDir, "learning", "audit")}
		metrics, err := audit.Metrics(ctx, now.AddDate(0, 0, -metricDays), now)
		if err != nil {
			return err
		}
		return enc.Encode(metrics)
	}
	cfg, err := state.LoadLearningPolicy(app.StateDir)
	if args[0] == "delete" {
		// Erasure remains available after opting out or removing the policy.
		partition, err := authorizedLearningPartition(app, cfg, frontier, true)
		if err != nil || partition.Scope == nil {
			return learning.ErrDenied
		}
		if requestID == "" {
			requestID = rand.Text()
		}
		receipt, err := state.NewLearningRepository(app.StateDir, nil).DeletePrivateLearning(
			ctx, *partition.Scope, deleteCandidate, requestID, time.Now())
		if err != nil {
			return err
		}
		return enc.Encode(receipt)
	}
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

	if args[0] == "digest" {
		return writeLearningDigest(ctx, app, cfg, output, time.Now().UTC(), digestDays, digestLimit)
	}
	if args[0] == "export" {
		partition, err := authorizedLearningPartition(app, cfg, frontier, false)
		if err != nil {
			return err
		}
		policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
		if err != nil {
			return err
		}
		allow := func(text string) bool { return !policy.ContainsSecret(text) && !trust.Suspicious(text) }
		repo := state.NewLearningRepository(app.StateDir, nil)
		if partition.Public {
			result, err := repo.ExportPublicLearning(ctx, cfg, allow)
			if err != nil {
				return err
			}
			return enc.Encode(result)
		}
		now := time.Now().UTC()
		cutoff := privateLearningCutoff(cfg, now)
		result, err := repo.ExportPrivateLearning(ctx, *partition.Scope, cutoff, func(text string) bool {
			return allow(text) && !cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
		})
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
	runIDInput := slices.Contains([]string{"work", "status", "pause", "resume", "cancel", "audit", "note", "schedule", "claim-history", "claim-edit", "archive"}, args[0])
	if !runIDInput && cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: string(data)}) {
		return learning.ErrDenied
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return err
	}
	coordinator := state.NewLearningCoordinator(app.StateDir)
	allow := func(s string) bool { return !policy.ContainsSecret(s) && !trust.Suspicious(s) }
	if args[0] == "archive" {
		note, err := state.NewLearningRepository(app.StateDir, nil).SetLearningNoteArchived(ctx, cfg,
			strings.TrimSpace(string(data)), !restoreNote, allow, time.Now())
		if err != nil {
			return err
		}
		return enc.Encode(note)
	}
	if args[0] == "cancel" {
		result, err := coordinator.RequestResearchCancel(ctx, cfg, strings.TrimSpace(string(data)), allow)
		if err != nil {
			return err
		}
		if !result.Requested {
			if _, err := coordinator.ExportResearchAudit(ctx, cfg, result.RunID, allow); err != nil {
				log.Printf("learning audit pending for run %s (replay with q-agent learning audit)", result.RunID)
			}
		}
		return enc.Encode(result)
	}
	if args[0] == "claim-history" || args[0] == "claim-edit" {
		repo := state.NewLearningRepository(app.StateDir, nil)
		if args[0] == "claim-history" {
			history, err := repo.ClaimHistory(ctx, cfg, strings.TrimSpace(string(data)), allow)
			if err != nil {
				return err
			}
			return enc.Encode(history)
		}
		var edit state.ClaimEdit
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&edit); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return fmt.Errorf("claim edit requires exactly one JSON object")
		}
		claim, err := repo.EditPublicClaim(ctx, cfg, edit, allow, time.Now())
		if err != nil {
			return err
		}
		return enc.Encode(claim)
	}
	if args[0] == "schedule" {
		id := strings.TrimSpace(string(data))
		if removeSchedule {
			err = coordinator.RemoveScheduledResearch(id)
		} else {
			err = coordinator.ScheduleResearch(ctx, cfg, id, func(s string) bool {
				return allow(s) && !strings.Contains(s, redaction.Marker)
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
		result, err := coordinator.SetResearchPaused(
			ctx, cfg, strings.TrimSpace(string(data)), allow, args[0] == "pause")
		if err != nil {
			return err
		}
		return writeResearchResult(result.RunID, result)
	}

	if args[0] == "recall" {
		results, err := state.NewLearningRepository(app.StateDir, nil).RecallPublic(
			ctx, cfg, string(data), allow, 5)
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
			id := strings.TrimSpace(string(data))
			if len(cfg.Permissions.AllowDomains) == 0 || !cfg.Permissions.AllowsDomain(localtools.LearningSearchDomain) {
				result, err := coordinator.ResearchStatus(ctx, cfg, id, allow)
				if err != nil {
					return err
				}
				switch result.State {
				case learning.RunPaused, learning.RunCancelled, learning.RunCompleted, learning.RunFailed, learning.RunUnresolved, learning.RunBudgetExhausted:
					return writeResearchResult(id, result)
				default:
					return learning.ErrDenied
				}
			}
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
			result, err := work(ctx, cfg, id, func(s string) bool {
				return allow(s) && !strings.Contains(s, redaction.Marker) &&
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
	chatID, topicID    int64
	public             bool
	selectOnly         bool
	origin, status     string
	enqueue            string
	pin, mute, dismiss string
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
	partition, err := authorizedLearningPartition(app, cfg, options, false)
	if err != nil {
		return nil, err
	}
	var action, target string
	for _, choice := range []struct{ action, id string }{{"pin", options.pin}, {"mute", options.mute}, {"dismiss", options.dismiss}} {
		if choice.id == "" {
			continue
		}
		if action != "" || options.enqueue != "" || options.selectOnly || options.origin != "" || options.status != "candidate" {
			return nil, fmt.Errorf("choose one frontier operation")
		}
		action, target = choice.action, choice.id
	}

	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return nil, err
	}
	allow := func(text string) bool {
		return !strings.Contains(text, redaction.Marker) && !policy.ContainsSecret(text) &&
			!cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
	}
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
		query := learning.ConversationQuery{Scope: scope, From: now.Add(-state.ConversationFrontierLookback), Until: now}
		entries, err = repo.ConversationFrontier(ctx, reader, query, now, privateLearningCutoff(cfg, now), cfg.TelemetryEnabled, allow)
	}
	if err != nil {
		return nil, err
	}
	coordinator := state.NewLearningCoordinator(app.StateDir)
	entries, err = coordinator.RefreshFrontier(ctx, partition, entries)
	if err != nil {
		return nil, err
	}
	if action != "" {
		result, err := repo.EditLearningFrontier(ctx, partition, entries, target, action, now)
		if err == nil && cfg.TelemetryEnabled {
			if err := repo.ExportFrontierAudit(ctx, []learning.FrontierEntry{result}); err != nil {
				log.Print("learning frontier audit pending")
			}
		}
		return result, err
	}
	if cfg.TelemetryEnabled && options.enqueue == "" {
		if err := repo.ExportFrontierAudit(ctx, entries); err != nil {
			log.Print("learning frontier audit pending")
		}
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

func authorizedLearningPartition(app config.Config, cfg learning.Config, options frontierOptions, erasing bool) (learning.Partition, error) {
	var empty learning.Partition
	if options.public {
		if options.chatID != 0 || options.topicID != 0 || !cfg.Enabled || erasing {
			return empty, learning.ErrDenied
		}
		return learning.Partition{Public: true}, nil
	}
	if options.chatID == 0 || options.topicID < 0 || (!erasing && (!cfg.Enabled || !app.Memory.Enabled ||
		cfg.Permissions.ExcludesTopic(options.chatID, options.topicID))) {
		return empty, learning.ErrDenied
	}
	visibility := learning.GroupChat
	if options.chatID > 0 {
		if !slices.Contains(app.Telegram.AllowedUsers, options.chatID) || (!erasing && !cfg.Permissions.IncludePrivateChats) {
			return empty, learning.ErrDenied
		}
		visibility = learning.PrivateChat
	} else if !slices.Contains(app.Telegram.AllowedGroups, options.chatID) || (!erasing && !cfg.Permissions.IncludeGroupChats) {
		return empty, learning.ErrDenied
	}
	return learning.Partition{Scope: &learning.Scope{Platform: "telegram", ChatID: options.chatID, TopicID: &options.topicID, Visibility: visibility}}, nil
}

func privateLearningCutoff(cfg learning.Config, now time.Time) time.Time {
	if cfg.Permissions.PrivateFrontierDays == 0 {
		return time.Time{}
	}
	return now.AddDate(0, 0, -cfg.Permissions.PrivateFrontierDays)
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
