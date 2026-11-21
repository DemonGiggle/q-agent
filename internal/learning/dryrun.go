package learning

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

type EventType string

const (
	ResearchPlanned   EventType = "ResearchPlanned"
	ModelCallReserved EventType = "ModelCallReserved"
)

type Event struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"event_id"`
	Type          EventType `json:"type"`
	At            time.Time `json:"ts"`
	RunID         string    `json:"run_id"`
	ObjectRefs    []string  `json:"object_refs"`
	Outcome       string    `json:"outcome"`
}

func (e Event) Validate() error {
	if e.SchemaVersion != SchemaVersion || e.ID == "" || e.Type == "" ||
		e.At.IsZero() || e.RunID == "" || len(e.ObjectRefs) == 0 || e.Outcome == "" {
		return fmt.Errorf("invalid learning audit event")
	}
	for _, ref := range e.ObjectRefs {
		if ref == "" {
			return fmt.Errorf("invalid learning audit object reference")
		}
	}
	return nil
}

// AuditSink appends immutable events keyed by event ID. Replaying the same
// event is idempotent; conflicting contents under the same ID are rejected.
type AuditSink interface {
	Append(context.Context, Event) error
}

type Preview struct {
	Question Question `json:"question"`
	Plan     Plan     `json:"plan"`
	Run      Run      `json:"run"`
}

// DryRun previews a deterministic template with no provider, scheduler, or
// repository dependency. The only side effect is a metadata-only audit event.
func DryRun(ctx context.Context, cfg Config, question string, now time.Time, audit AuditSink) (Preview, error) {
	if err := ctx.Err(); err != nil {
		return Preview{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Preview{}, err
	}
	if !cfg.Enabled {
		return Preview{}, fmt.Errorf("learning is disabled")
	}
	question = strings.TrimSpace(question)
	if len(question) > 4096 {
		return Preview{}, fmt.Errorf("learning question exceeds 4096 bytes")
	}
	if cfg.Permissions.Excludes(ConversationTurn{Transcript: question}) {
		return Preview{}, ErrDenied
	}
	if audit == nil {
		return Preview{}, fmt.Errorf("learning dry-run requires an audit sink")
	}

	now = now.UTC()
	record := Record{SchemaVersion: SchemaVersion, CreatedAt: now, UpdatedAt: now}
	q := Question{Record: record, ID: rand.Text(), Text: question, OriginType: "manual", Status: "queued"}
	p := Plan{
		Record: record, ID: rand.Text(), QuestionID: q.ID, Goal: q.Text, Version: 1,
		Subquestions: []string{q.Text}, Steps: []string{"discover", "read", "validate_evidence", "distill"},
		EvidenceRequirements: EvidenceRequirements{MinIndependentSources: 2, AllowSingleAuthority: true},
		AllowedSourceClasses: []string{"public"},
		StopConditions:       []string{"goal_met", "no_qualified_evidence", "budget_exhausted", "cancelled"},
		Budget:               cfg.PerQuestion,
	}
	if err := q.Validate(); err != nil {
		return Preview{}, err
	}
	if err := p.Validate(); err != nil {
		return Preview{}, err
	}

	run := Run{Record: record, ID: rand.Text(), PlanID: p.ID, State: RunPlanned}
	event := Event{
		SchemaVersion: SchemaVersion, ID: rand.Text(), Type: ResearchPlanned, At: now,
		RunID: run.ID, ObjectRefs: []string{q.ID, p.ID}, Outcome: "dry_run",
	}
	if err := audit.Append(ctx, event); err != nil {
		return Preview{}, err
	}
	return Preview{Question: q, Plan: p, Run: run}, nil
}
