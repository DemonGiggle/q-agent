package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"q-agent/internal/config"
	"q-agent/internal/learning"
	"q-agent/internal/redaction"
	"q-agent/internal/state"
	"q-agent/internal/trust"
)

func writeLearningDigest(ctx context.Context, app config.Config, cfg learning.Config, output io.Writer,
	now time.Time, days, limit int) error {
	if days < 1 || days > 30 || limit < 1 || limit > 10 {
		return fmt.Errorf("learning digest requires 1–30 days and a limit of 1–10 notes")
	}
	policy, err := redaction.New(redaction.Config{}, configuredSecrets(app))
	if err != nil {
		return err
	}
	allow := func(text string) bool {
		return !strings.Contains(text, redaction.Marker) && !policy.ContainsSecret(text) && !trust.Suspicious(text)
	}
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	notes, err := state.NewLearningRepository(app.StateDir, nil).RecentPublicLearning(ctx, cfg, from, now, allow, limit)
	if err != nil {
		return err
	}
	const maxBytes = 32 << 10
	heading := "# Research digest\n\n"
	if _, err := io.WriteString(output, heading); err != nil {
		return err
	}
	remaining := maxBytes - len(heading)
	shown := 0
	for _, note := range notes {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := note.Markdown(cfg.SourcePolicy, now, remaining)
		if entry == "" {
			continue
		}
		if _, err := io.WriteString(output, entry); err != nil {
			return err
		}
		remaining -= len(entry)
		shown++
	}
	if shown == 0 {
		message := "No authorized research notes in this interval.\n"
		if len(notes) > 0 {
			message = "The selected notes exceed the digest size limit. Read them with learning note.\n"
		}
		_, err := io.WriteString(output, message)
		return err
	}
	return nil
}
