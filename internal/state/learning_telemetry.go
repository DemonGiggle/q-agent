package state

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

// Project milestones from saved state. IDs depend on the run and a fixed
// stage, never on source text, URLs, byte hashes or Telegram identifiers.
func researchMilestones(record storedLearningRun) []learning.Event {
	run := record.Run
	var events []learning.Event
	add := func(kind learning.EventType, at time.Time, outcome, ref string, ordinal int) {
		events = append(events, learning.Event{
			SchemaVersion: learning.SchemaVersion, Type: kind,
			ID: researchTransitionID(run.ID+":"+string(kind), ordinal), At: at,
			RunID: run.ID, ObjectRefs: []string{ref}, Outcome: outcome,
		})
	}
	for i, tr := range record.Transitions {
		switch tr.State {
		case learning.RunPlanned:
			if tr.Reason != "operator_resumed" {
				add(learning.ResearchPlanned, tr.At, "planned", run.PlanID, i)
			}
		case learning.RunRunning:
			if tr.Reason != "operator_resumed" {
				add(learning.ResearchStarted, tr.At, "started", run.PlanID, i)
			}
		case learning.RunPaused:
			add(learning.ResearchPaused, tr.At, "paused", run.PlanID, i)
		case learning.RunFailed:
			add(learning.ResearchFailed, tr.At, "failed", run.PlanID, i)
		case learning.RunCompleted:
			add(learning.ResearchCompleted, tr.At, "completed", run.ID, i)
		case learning.RunBudgetExhausted:
			add(learning.BudgetExceeded, tr.At, "exhausted", run.PlanID, i)
		}
	}
	cp := run.Checkpoint
	if cp == nil {
		return events
	}
	for i, ref := range checkpointResearchReferences(cp) {
		opaque := researchTransitionID(run.ID+":reference", i)
		add(learning.SourceFetched, ref.Source.FetchedAt, "recorded", opaque, i)
		if ref.Evidence != nil && ref.Evidence.MatchesSourceExcerpt(ref.Source) {
			add(learning.EvidenceValidated, ref.Evidence.ExtractedAt, "verified", opaque, i)
		}
	}
	if cp.ResearchClaim != nil {
		add(learning.ClaimProposed, cp.ResearchClaim.RevisedAt, "proposed", run.ID, 0)
	}
	if run.State != learning.RunCompleted || run.EndedAt == nil {
		return events
	}
	outcome, disputed := "note", false
	if cp.ResearchClaim != nil {
		outcome, disputed = cp.PublishedClaimChange, cp.PublishedConflict
		if outcome == "" {
			outcome = "claim_unclassified"
		}
	}
	add(learning.KnowledgeCommitted, *run.EndedAt, outcome, run.ID, 0)
	if disputed {
		add(learning.ConflictDetected, *run.EndedAt, "disputed", run.ID, 0)
	}
	return events
}

// ExportFrontierAudit projects only explicitly observed entries. The opaque
// reference is generated locally and saved with the candidate. Candidate IDs
// can be derived from private text and must not enter the audit directory.
func (r *LearningRepository) ExportFrontierAudit(ctx context.Context, entries []learning.FrontierEntry) error {
	var events []learning.Event
	for _, entry := range entries {
		if !auditID.MatchString(entry.AuditRef) {
			continue
		}
		opaque := entry.AuditRef
		add := func(kind learning.EventType, at time.Time, outcome, runID string, ordinal int) {
			events = append(events, learning.Event{
				SchemaVersion: learning.SchemaVersion, ID: researchTransitionID(opaque+":"+string(kind), ordinal),
				Type: kind, At: at, Outcome: outcome, RunID: runID, ObjectRefs: []string{opaque},
			})
		}
		add(learning.CandidateGenerated, entry.Question.CreatedAt, entry.Question.OriginType, opaque, 0)
		for i, tr := range entry.Transitions {
			if tr.Status == "queued" && auditID.MatchString(entry.RunID) {
				add(learning.QuestionSelected, tr.At, "queued", entry.RunID, i)
			}
			add(learning.FrontierUpdated, tr.At, tr.Status, opaque, i)
		}
	}
	return (LearningAudit{Dir: filepath.Join(filepath.Dir(r.dir), "audit")}).appendEvents(ctx, events)
}

func validMilestone(event learning.Event) bool {
	if len(event.ObjectRefs) != 1 || event.Reason != "" || event.Spent != nil || len(event.ClaimRefs) != 0 {
		return false
	}
	allowed := ""
	switch event.Type {
	case learning.CandidateGenerated:
		allowed = "conversation gap association"
	case learning.QuestionSelected:
		allowed = "queued"
	case learning.ResearchPlanned:
		allowed = "planned"
	case learning.ResearchStarted:
		allowed = "started"
	case learning.SourceFetched:
		allowed = "recorded"
	case learning.EvidenceValidated:
		allowed = "verified"
	case learning.ClaimProposed:
		allowed = "proposed"
	case learning.KnowledgeCommitted:
		allowed = "note new_claim revised_claim reused_claim claim_unclassified"
	case learning.ConflictDetected:
		allowed = "disputed"
	case learning.FrontierUpdated:
		allowed = "candidate dismissed queued resolved unresolved"
	case learning.ResearchPaused:
		allowed = "paused"
	case learning.ResearchFailed:
		allowed = "failed"
	case learning.ResearchCompleted:
		allowed = "completed"
	case learning.BudgetExceeded:
		allowed = "exhausted"
	case learning.SafetyBlocked:
		allowed = "model_tool_request"
	}
	return allowed != "" && slices.Contains(strings.Fields(allowed), event.Outcome)
}

func validateAuditEvent(event learning.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if !auditID.MatchString(event.ID) || !auditID.MatchString(event.RunID) || !validAuditMetadata(event) {
		return fmt.Errorf("invalid learning audit event metadata")
	}
	for _, refs := range [][]string{event.ObjectRefs, event.ClaimRefs} {
		for _, ref := range refs {
			if !auditID.MatchString(ref) {
				return fmt.Errorf("invalid learning audit object reference")
			}
		}
	}
	return nil
}
