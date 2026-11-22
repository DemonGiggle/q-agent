package state

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

type ClaimEdit struct {
	ClaimID          string            `json:"claim_id"`
	ExpectedRevision uint64            `json:"expected_revision"`
	Action           string            `json:"action"`
	Reason           string            `json:"reason"`
	Qualifiers       map[string]string `json:"qualifiers,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	ValidFrom        *time.Time        `json:"valid_from,omitempty"`
	ValidTo          *time.Time        `json:"valid_to,omitempty"`
	ReplacementID    string            `json:"replacement_id,omitempty"`
	RefutingEvidence []string          `json:"refuting_evidence_ids,omitempty"`
}

type ClaimHistoryEntry struct {
	Current   learning.Claim           `json:"current"`
	Revisions []learning.RevisionEvent `json:"revisions"`
}

// ClaimHistory follows replacements and reads one consistent public snapshot.
func (r *LearningRepository) ClaimHistory(ctx context.Context, cfg learning.Config, id string,
	allow func(string) bool) ([]ClaimHistoryEntry, error) {
	if !cfg.Enabled || allow == nil {
		return nil, learning.ErrDenied
	}
	snapshot, err := r.Snapshot(ctx, learning.Partition{Public: true})
	if err != nil {
		return nil, err
	}
	refs := claimReferences(snapshot, cfg, allow)
	byID := make(map[string]learning.Claim, len(snapshot.Claims))
	for _, claim := range snapshot.Claims {
		byID[claim.ID] = claim
	}
	seen := make(map[string]bool)
	history := make([]ClaimHistoryEntry, 0, 1)
	for id != "" {
		claim, ok := byID[id]
		if !ok || seen[id] || !claimTextsAllowed(cfg, allow, claim) || !allClaimReferencesAllowed(refs, claim.Evidence) {
			return nil, learning.ErrDenied
		}
		seen[id] = true
		entry := ClaimHistoryEntry{Current: claim, Revisions: []learning.RevisionEvent{}}
		for _, event := range snapshot.Revisions {
			if event.EntityType != "claim" || event.EntityID != id {
				continue
			}
			if !allow(event.Reason) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: event.Reason}) ||
				slices.ContainsFunc(event.EvidenceIDs, func(id string) bool { _, ok := refs[id]; return !ok }) ||
				(event.Before != nil && (!claimTextsAllowed(cfg, allow, *event.Before) ||
					!allClaimReferencesAllowed(refs, event.Before.Evidence))) {
				return nil, learning.ErrDenied
			}
			entry.Revisions = append(entry.Revisions, event)
		}
		history = append(history, entry)
		id = claim.SupersededBy
	}
	if len(history) == 0 {
		return nil, learning.ErrDenied
	}
	return history, nil
}

// EditPublicClaim applies one explicitly reviewed change. The caller
// holds the learning-directory lock. Revision and snapshot advance together.
func (r *LearningRepository) EditPublicClaim(ctx context.Context, cfg learning.Config, edit ClaimEdit,
	allow func(string) bool, now time.Time) (learning.Claim, error) {
	var empty learning.Claim
	edit.Reason = strings.TrimSpace(edit.Reason)
	if !cfg.Enabled || allow == nil || len(edit.Reason) == 0 || len(edit.Reason) > 256 ||
		!allow(edit.Reason) || cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: edit.Reason}) {
		return empty, learning.ErrDenied
	}
	if edit.Action != "qualify" && (edit.Aliases != nil || edit.Qualifiers != nil || edit.ValidFrom != nil || edit.ValidTo != nil) ||
		edit.Action != "supersede" && edit.ReplacementID != "" ||
		edit.Action != "contradict" && len(edit.RefutingEvidence) != 0 {
		return empty, fmt.Errorf("claim fields do not match the requested action")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.load("public")
	if err != nil {
		return empty, err
	}
	next := data.Snapshot
	next.Claims = slices.Clone(next.Claims)
	index := slices.IndexFunc(next.Claims, func(claim learning.Claim) bool { return claim.ID == edit.ClaimID })
	if index < 0 {
		return empty, learning.ErrDenied
	}
	prior := next.Claims[index]
	if prior.Revision != edit.ExpectedRevision || prior.Revision == math.MaxUint64 || next.Version == math.MaxUint64 {
		return empty, learning.ErrVersionConflict
	}
	refs := claimReferences(next, cfg, allow)
	if !claimTextsAllowed(cfg, allow, prior) || !allClaimReferencesAllowed(refs, prior.Evidence) {
		return empty, learning.ErrDenied
	}
	claim := prior
	decision := strings.ToUpper(edit.Action)
	if prior.Status == "superseded" && edit.Action != "rollback" {
		return empty, learning.ErrDenied
	}
	switch edit.Action {
	case "contradict":
		if len(edit.RefutingEvidence) == 0 || len(edit.RefutingEvidence) > 8 {
			return empty, fmt.Errorf("contradiction requires one to eight evidence IDs")
		}
		links := make([]learning.EvidenceLink, 0, len(edit.RefutingEvidence))
		for _, id := range edit.RefutingEvidence {
			links = append(links, learning.EvidenceLink{EvidenceID: id, Relation: "refutes"})
		}
		if !allClaimReferencesAllowed(refs, links) {
			return empty, learning.ErrDenied
		}
		claim.Evidence = mergeClaimEvidence(refs, prior.Evidence, links)
		if len(claim.Evidence) > len(prior.Evidence) {
			applyClaimAssessment(&claim, refs, cfg, now, false)
		}
	case "review":
		applyClaimAssessment(&claim, refs, cfg, now, true)
		decision = "REINFORCE"
		if claim.Status == "disputed" {
			decision = "CONTRADICT"
		}
	case "qualify":
		if len(edit.Aliases) > 8 || len(edit.Qualifiers) > 8 || edit.Qualifiers[conflictQualifier] != "" {
			return empty, fmt.Errorf("invalid claim qualifiers or aliases")
		}
		for _, alias := range edit.Aliases {
			if len(alias) > 160 || strings.TrimSpace(alias) == "" {
				return empty, fmt.Errorf("invalid claim alias")
			}
		}
		for key, value := range edit.Qualifiers {
			if len(key) > 64 || len(value) > 160 || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
				return empty, fmt.Errorf("invalid claim qualifier")
			}
		}
		claim.Qualifiers, claim.Aliases = edit.Qualifiers, edit.Aliases
		claim.ValidFrom, claim.ValidTo = edit.ValidFrom, edit.ValidTo
		applyClaimAssessment(&claim, refs, cfg, now, prior.Status == "supported")
	case "stale":
		if prior.Status != "supported" {
			return empty, learning.ErrDenied
		}
		claim.Status, claim.Confidence = "stale", 0
		claim.ConfidenceReason = "Operator marked the supporting evidence stale"
	case "supersede":
		if prior.Status != "supported" && prior.Status != "stale" {
			return empty, learning.ErrDenied
		}
		replacement := slices.IndexFunc(next.Claims, func(item learning.Claim) bool {
			return item.ID == edit.ReplacementID && item.ID != prior.ID && item.Status == "supported"
		})
		if replacement < 0 || !claimTextsAllowed(cfg, allow, next.Claims[replacement]) ||
			!allClaimReferencesAllowed(refs, next.Claims[replacement].Evidence) ||
			normalizedClaimText(next.Claims[replacement].Subject) != normalizedClaimText(prior.Subject) ||
			normalizedClaimText(next.Claims[replacement].Predicate) != normalizedClaimText(prior.Predicate) {
			return empty, learning.ErrDenied
		}
		claim.Status, claim.SupersededBy = "superseded", edit.ReplacementID
	case "rollback":
		at := slices.IndexFunc(next.Revisions, func(event learning.RevisionEvent) bool {
			return event.EntityType == "claim" && event.EntityID == prior.ID && event.NewRevision == prior.Revision
		})
		if at < 0 || next.Revisions[at].Before == nil {
			return empty, learning.ErrDenied
		}
		claim = *next.Revisions[at].Before
		claim.RevisedAt, claim.UpdatedAt, claim.Revision = prior.RevisedAt, prior.UpdatedAt, prior.Revision
		claim.Evidence = prior.Evidence
		if claim.Status != "superseded" {
			applyClaimAssessment(&claim, refs, cfg, now, claim.Status == "supported")
		}
	default:
		return empty, fmt.Errorf("unknown claim edit action")
	}
	if claim.Validate() != nil || !claimTextsAllowed(cfg, allow, claim) {
		return empty, learning.ErrDenied
	}
	changed := !reflect.DeepEqual(prior, claim)
	if changed {
		appendClaimRevision(&next, index, claim, decision, edit.Reason, now)
	}
	if !syncClaimRelation(&next, next.Claims[index], refs) && !changed {
		return prior, nil
	}
	next.Version++
	if err := r.savePublicKnowledge(ctx, data, next); err != nil {
		return empty, err
	}
	return next.Claims[index], nil
}

func (r *LearningRepository) savePublicKnowledge(ctx context.Context, data learningPartition, next learning.KnowledgeSnapshot) error {
	if err := next.ValidateReferences(); err != nil {
		return err
	}
	if err := next.ValidateClaimHistory(data.Snapshot); err != nil {
		return err
	}
	data.Snapshot = next
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(body) > maxLearningPartitionBytes {
		return ErrLearningPartitionFull
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicBytes(filepath.Join(r.dir, "public.json"), body)
}
