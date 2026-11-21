package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/learning"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
)

func runLearning(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || (args[0] != "configure" && args[0] != "trigger") {
		return fmt.Errorf("usage: q-agent learning configure|trigger [--config FILE]; pass input on stdin")
	}

	flags := flag.NewFlagSet("learning "+args[0], flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "q-agent TOML configuration")
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
	policyPath := filepath.Join(app.StateDir, "learning", "policy.json")
	cfg := learning.DefaultConfig()
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	if args[0] == "configure" {
		if err := decodeLearningConfig(input, &cfg); err != nil {
			return err
		}
		if cfg.Schedule.Enabled {
			return fmt.Errorf("M0 supports manual dry-run only; set schedule.enabled to false")
		}
		if err := state.Atomic(policyPath, cfg); err != nil {
			return err
		}
		return enc.Encode(cfg)
	}
	f, err := os.Open(policyPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := decodeLearningConfig(f, &cfg); err != nil {
		return err
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
	if cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: string(data)}) {
		return learning.ErrDenied
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return err
	}
	preview, err := learning.DryRun(ctx, cfg, policy.Redact(string(data)), time.Now(),
		state.LearningAudit{Dir: filepath.Join(app.StateDir, "learning", "audit")})
	if err != nil {
		return err
	}
	return enc.Encode(preview)
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
