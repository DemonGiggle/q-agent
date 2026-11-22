package state

import (
	"crypto/rand"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"q-agent/internal/learning"
)

const maxClaimLinks = 32
const conflictQualifier = "evidence_conflict"

func normalizedClaimText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func claimTextsAllowed(cfg learning.Config, allow func(string) bool, claim learning.Claim) bool {
	safe := func(s string) bool {
		return allow(s) && !cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: s})
	}
	if !safe(claim.Subject+" "+claim.Predicate+" "+claim.Object) ||
		!safe(claim.ConfidenceReason) || slices.ContainsFunc(claim.Aliases, func(s string) bool { return !safe(s) }) {
		return false
	}
	for key, value := range claim.Qualifiers {
		if !safe(key + " " + value) {
			return false
		}
	}
	return true
}

func claimReferences(snapshot learning.KnowledgeSnapshot, cfg learning.Config, allow func(string) bool) map[string]learning.ResearchReference {
	sources := make(map[string]learning.Source, len(snapshot.Sources))
	safe := func(text string) bool {
		return allow(text) && !cfg.Permissions.Excludes(learning.ConversationTurn{Transcript: text})
	}
	for _, source := range snapshot.Sources {
		if cfg.Permissions.ResearchURI(source.CanonicalURI) && safe(source.CanonicalURI) &&
			safe(source.Title) && safe(source.Publisher) {
			sources[source.ID] = source
		}
	}
	references := make(map[string]learning.ResearchReference, len(snapshot.Evidence))
	for _, evidence := range snapshot.Evidence {
		if source, found := sources[evidence.SourceID]; found && evidence.MatchesSourceExcerpt(source) &&
			safe(evidence.ExcerptOrSummary) {
			references[evidence.ID] = learning.ResearchReference{Source: source, Evidence: &evidence}
		}
	}
	return references
}

func allClaimReferencesAllowed(refs map[string]learning.ResearchReference, links []learning.EvidenceLink) bool {
	return !slices.ContainsFunc(links, func(link learning.EvidenceLink) bool {
		_, ok := refs[link.EvidenceID]
		return !ok
	})
}

// Only an exact proposition or an operator-recorded subject alias is a
// candidate for automatic evolution. Scoped or ambiguous matches need review.
func sameClaim(draft, prior learning.Claim) bool {
	if prior.Status == "superseded" || prior.ValidFrom != nil || prior.ValidTo != nil {
		return false
	}
	for k := range prior.Qualifiers {
		if k != conflictQualifier {
			return false
		}
	}
	subject := normalizedClaimText(draft.Subject)
	return (normalizedClaimText(prior.Subject) == subject || slices.ContainsFunc(prior.Aliases, func(alias string) bool {
		return normalizedClaimText(alias) == subject
	})) && normalizedClaimText(prior.Predicate) == normalizedClaimText(draft.Predicate) &&
		normalizedClaimText(prior.Object) == normalizedClaimText(draft.Object)
}

func evolveResearchClaim(snapshot *learning.KnowledgeSnapshot, cfg learning.Config, allow func(string) bool,
	draft learning.Claim, now time.Time) string {
	refs := claimReferences(*snapshot, cfg, allow)
	if !allClaimReferencesAllowed(refs, draft.Evidence) {
		return ""
	}
	index := -1
	for i, prior := range snapshot.Claims {
		if sameClaim(draft, prior) {
			if index >= 0 {
				index = -1
				break
			}
			index = i
		}
	}
	if index >= 0 {
		prior := snapshot.Claims[index]
		if claimTextsAllowed(cfg, allow, prior) && allClaimReferencesAllowed(refs, prior.Evidence) {
			next := prior
			next.Evidence = mergeClaimEvidence(refs, prior.Evidence, draft.Evidence)
			if len(next.Evidence) == len(prior.Evidence) {
				return prior.ID // NO_OP: an already cited or non-independent source.
			}
			applyClaimAssessment(&next, refs, cfg, now, false)
			decision := "REINFORCE"
			if next.Status == "disputed" {
				decision = "CONTRADICT"
			}
			appendClaimRevision(snapshot, index, next, decision, "Research added independent cited evidence", now)
			syncClaimRelation(snapshot, snapshot.Claims[index], refs)
			return prior.ID
		}
	}
	applyClaimAssessment(&draft, refs, cfg, now, false)
	appendClaimRevision(snapshot, -1, draft, "ADD", "Research recorded a cited proposition", now)
	return draft.ID
}

func mergeClaimEvidence(refs map[string]learning.ResearchReference, prior, added []learning.EvidenceLink) []learning.EvidenceLink {
	var known []learning.ResearchReference
	for _, link := range prior {
		if ref := refs[link.EvidenceID]; ref.Source.Available() {
			known = append(known, ref)
		}
	}
	result := slices.Clone(prior)
	for _, link := range added {
		ref := refs[link.EvidenceID]
		if len(result) < maxClaimLinks && ref.Source.Available() &&
			!slices.ContainsFunc(result, func(old learning.EvidenceLink) bool { return old.EvidenceID == link.EvidenceID }) &&
			independentResearchReference(ref, known) {
			result = append(result, link)
			known = append(known, ref)
		}
	}
	return result
}

func applyClaimAssessment(claim *learning.Claim, refs map[string]learning.ResearchReference, cfg learning.Config, now time.Time, reviewed bool) {
	var support, opposition []learning.ResearchReference
	var fresh, trusted int
	for _, link := range claim.Evidence {
		ref, ok := refs[link.EvidenceID]
		if !ok || !ref.Source.Available() {
			continue
		}
		switch {
		case link.Relation == "supports" && independentResearchReference(ref, support):
			support = append(support, ref)
			if cfg.SourcePolicy.Accepts(ref.Source, now) && !ref.Source.LastVerifiedAt().Before(now.AddDate(0, 0, -365)) {
				fresh++
			}
			if ref.Source.TrustClass == "public_reference" {
				trusted++
			}
		case link.Relation == "refutes" && independentResearchReference(ref, opposition):
			opposition = append(opposition, ref)
		}
	}
	claim.Confidence = 0
	claim.ConfidenceReason = fmt.Sprintf("%d independent supporting, %d opposing, %d fresh and %d public-reference supporting sources",
		len(support), len(opposition), fresh, trusted)
	claim.Qualifiers = maps.Clone(claim.Qualifiers)
	delete(claim.Qualifiers, conflictQualifier)
	switch {
	case len(support) > 0 && len(opposition) > 0:
		claim.Status = "disputed"
		if claim.Qualifiers == nil {
			claim.Qualifiers = make(map[string]string)
		}
		claim.Qualifiers[conflictQualifier] = "Sources report conflicting evidence"
	case len(support) == 0:
		claim.Status = "unresolved"
	default:
		claim.Confidence = float64(min(90, 35+min(len(support), 3)*12+min(fresh, 3)*4+min(trusted, 3)*2)) / 100
		switch {
		case reviewed && fresh >= 2:
			claim.Status = "supported"
		case reviewed && claim.Status == "supported":
			claim.Status, claim.Confidence = "stale", 0
		case claim.Status == "stale":
			claim.Confidence = 0
		case claim.Status == "supported":
			// Approved status only changes through conflict or review.
		default:
			claim.Status = "hypothesis"
		}
	}
}

func appendClaimRevision(snapshot *learning.KnowledgeSnapshot, index int, next learning.Claim, decision, reason string, now time.Time) {
	var prior *learning.Claim
	var old uint64
	now = now.UTC()
	if index >= 0 {
		last := snapshot.Claims[index]
		prior, old = &last, last.Revision
		now = maxTime(now, last.UpdatedAt)
	} else {
		now = maxTime(now, next.CreatedAt)
	}
	next.UpdatedAt, next.RevisedAt, next.Revision = now, now, old+1
	event := learning.RevisionEvent{
		Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
		ID:     rand.Text(), EntityType: "claim", EntityID: next.ID, OldRevision: old,
		NewRevision: next.Revision, Decision: decision, Reason: reason, OccurredAt: now, Before: prior,
	}
	for _, link := range next.Evidence {
		event.EvidenceIDs = append(event.EvidenceIDs, link.EvidenceID)
	}
	if index < 0 {
		snapshot.Claims = append(snapshot.Claims, next)
	} else {
		snapshot.Claims[index] = next
	}
	snapshot.Revisions = append(snapshot.Revisions, event)
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
