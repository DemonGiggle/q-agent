package state

import (
	"reflect"
	"slices"
	"strings"

	"q-agent/internal/learning"
)

// Only operator-reviewed claims reach supported status. Each edge carries the
// exact reviewed revision and the supporting excerpts visible at that revision.
func syncClaimRelation(snapshot *learning.KnowledgeSnapshot, claim learning.Claim, refs map[string]learning.ResearchReference) bool {
	if claim.Status != "supported" {
		return false
	}
	from, to := strings.Join(strings.Fields(claim.Subject), " "), strings.Join(strings.Fields(claim.Object), " ")
	if len(from) > 160 || len(to) > 160 || strings.EqualFold(from, to) {
		return false
	}
	edge := learning.Relation{
		Record: claim.Record, ID: frontierResearchID("edge", "public", claim.ID),
		ClaimID: claim.ID, ClaimRevision: claim.Revision, Predicate: claim.Predicate,
		Qualifiers: claim.Qualifiers, ValidFrom: claim.ValidFrom, ValidTo: claim.ValidTo,
	}
	for _, link := range claim.Evidence {
		if ref, ok := refs[link.EvidenceID]; link.Relation == "supports" && ok && ref.Source.Available() {
			edge.EvidenceIDs = append(edge.EvidenceIDs, link.EvidenceID)
		}
	}
	if len(edge.EvidenceIDs) == 0 {
		return false
	}
	topic := func(title string) string {
		id := frontierResearchID("topic", "public", normalizedClaimText(title))
		if !slices.ContainsFunc(snapshot.Topics, func(existing learning.Topic) bool { return existing.ID == id }) {
			snapshot.Topics = append(snapshot.Topics, learning.Topic{Record: claim.Record, ID: id, Title: title, Status: "active"})
		}
		return id
	}
	edge.FromID, edge.ToID = topic(from), topic(to)
	index := slices.IndexFunc(snapshot.Relations, func(item learning.Relation) bool { return item.ID == edge.ID })
	if index < 0 {
		snapshot.Relations = append(snapshot.Relations, edge)
		return true
	}
	if reflect.DeepEqual(snapshot.Relations[index], edge) {
		return false
	}
	snapshot.Relations = slices.Clone(snapshot.Relations)
	snapshot.Relations[index] = edge
	return true
}
