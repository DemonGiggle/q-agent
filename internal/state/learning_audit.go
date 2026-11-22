package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"q-agent/internal/learning"
)

type LearningAudit struct{ Dir string }

var auditID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func (a LearningAudit) Append(ctx context.Context, event learning.Event) error {
	return a.appendEvents(ctx, []learning.Event{event})
}

// AppendRecall optionally keeps one validated, metadata-only event for local
// selection. Frontier reads never scan the audit directory.
func (a LearningAudit) AppendRecall(ctx context.Context, event learning.Event, feedback bool) error {
	if event.Type != learning.RecallUsed {
		return learning.ErrDenied
	}
	if err := a.Append(ctx, event); err != nil {
		return err
	}
	if feedback && ctx.Err() == nil && len(event.ClaimRefs) > 0 {
		return Atomic(filepath.Join(a.Dir, "latest-recall.json"), event)
	}
	return nil
}

func (a LearningAudit) appendEvents(ctx context.Context, events []learning.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		return err
	}
	for _, event := range events {
		if err := a.writeEvent(ctx, event); err != nil {
			return err
		}
	}
	return SyncDir(a.Dir)
}

func (a LearningAudit) writeEvent(ctx context.Context, event learning.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if !auditID.MatchString(event.ID) || !auditID.MatchString(event.RunID) || !validAuditMetadata(event) {
		return fmt.Errorf("invalid learning audit event metadata")
	}
	for _, ref := range event.ObjectRefs {
		if !auditID.MatchString(ref) {
			return fmt.Errorf("invalid learning audit object reference")
		}
	}
	for _, ref := range event.ClaimRefs {
		if !auditID.MatchString(ref) {
			return fmt.Errorf("invalid learning audit claim reference")
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	dst := filepath.Join(a.Dir, event.ID+".json")
	if err := checkAuditEvent(dst, data); !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(a.Dir, ".event-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.Link(f.Name(), dst); err != nil {
		if !os.IsExist(err) {
			return err
		}
		return checkAuditEvent(dst, data)
	}
	return nil
}

func checkAuditEvent(path string, expected []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	previous, err := io.ReadAll(io.LimitReader(f, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, previous) {
		return learning.ErrVersionConflict
	}
	return nil
}

func validAuditMetadata(event learning.Event) bool {
	if event.Type == learning.RecallUsed {
		return event.Outcome == "included" && event.Reason == "" && event.Spent == nil &&
			len(event.ObjectRefs) <= 2 && len(event.ClaimRefs) <= 20
	}
	if len(event.ClaimRefs) != 0 {
		return false
	}
	if event.Type == learning.ResearchTransition {
		return knownResearchState(learning.RunState(event.Outcome)) && safeResearchReason(event.Reason) == event.Reason
	}
	return event.Reason == "" && event.Spent == nil &&
		((event.Type == learning.PolicyConfigured && event.Outcome == "applied" && len(event.ObjectRefs) == 1) ||
			(event.Type == learning.ResearchPlanned && event.Outcome == "dry_run") ||
			(event.Type == learning.ModelCallReserved && event.Outcome == "reserved"))
}

func knownResearchState(state learning.RunState) bool {
	return pausableResearchState(state) || terminalLearningRun(state) ||
		state == learning.RunPaused || state == learning.RunCommitting
}

func safeResearchReason(reason string) string {
	switch reason {
	case "operator_paused", "operator_resumed", "cancelled", "policy_changed",
		"interrupted_provider_call", "no_qualified_source", "invalid_source_content", "provider_failed",
		"insufficient_independent_sources", "source_recorded", "independent_excerpts_recorded", "authority_source_recorded",
		"source_identity_conflict", "storage_limit", "provider_budget", "insufficient_claim_evidence":
		return reason
	}
	if knownResearchState(learning.RunState(reason)) {
		return reason
	}
	return ""
}

// ExportResearchAudit publishes the ordered transitions of one durable
// checkpoint. Concurrent or repeated exports use the same immutable IDs.
func (c *LearningCoordinator) ExportResearchAudit(ctx context.Context, cfg learning.Config, id string,
	allow func(string) bool) ([]learning.Event, error) {
	record, err := c.readAuthorizedResearch(ctx, cfg, id, allow)
	if err != nil {
		return nil, err
	}
	events := make([]learning.Event, 0, len(record.Transitions))
	for i, transition := range record.Transitions {
		eventID := transition.EventID
		if eventID == "" {
			eventID = researchTransitionID(id, i)
		}
		events = append(events, learning.Event{
			SchemaVersion: learning.SchemaVersion, ID: eventID, Type: learning.ResearchTransition,
			At: transition.At, RunID: id, ObjectRefs: []string{record.Run.PlanID}, Outcome: string(transition.State),
			Reason: safeResearchReason(transition.Reason), Spent: transition.Usage,
		})
	}
	audit := LearningAudit{Dir: filepath.Join(filepath.Dir(c.dir), "audit")}
	if err := audit.appendEvents(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}
