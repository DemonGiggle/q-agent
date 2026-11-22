package learning

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"
)

// KnowledgeCandidates derives public research leads from authorized, verified
// knowledge. Stable IDs keep the same inputs from producing duplicate leads.
func KnowledgeCandidates(snapshot KnowledgeSnapshot, now time.Time, allow func(string) bool) []FrontierEntry {
	result := make([]FrontierEntry, 0)
	if allow == nil {
		return result
	}
	seen := make(map[string]bool)
	add := func(origin, seed, text, reason, expected string, topics, refs []string, score Score) {
		if len(result) >= MaxFrontierCandidates || len(text) > 512 || !allow(text) {
			return
		}
		normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
		if seen[normalized] {
			return
		}
		seen[normalized] = true
		hash := sha256.Sum256([]byte(seed + "\n" + normalized + "\n" + strings.Join(refs, "\n")))
		score = RankQuestion(score)
		result = append(result, FrontierEntry{
			Question: Question{
				Record: Record{SchemaVersion: SchemaVersion, CreatedAt: now, UpdatedAt: now},
				ID:     fmt.Sprintf("%s-%x", origin, hash[:16]), Text: text, TopicIDs: topics,
				OriginType: origin, OriginRefs: refs, Reason: reason, ExpectedValue: expected,
				Status: "candidate", Depth: 1, Score: &score,
			},
			Transitions: []FrontierTransition{{At: now, Status: "candidate", Reason: origin}},
		})
	}
	// Leave room for at least one evidence-backed association even when
	// the snapshot has many unresolved claims.
	for _, claim := range snapshot.Claims {
		if len(result) >= MaxFrontierCandidates/2 {
			break
		}
		switch claim.Status {
		case "hypothesis", "unresolved", "disputed", "stale":
		default:
			continue
		}
		if len(claim.Evidence) == 0 {
			continue
		}
		proposition := strings.Join(strings.Fields(claim.Subject+" "+claim.Predicate+" "+claim.Object), " ")
		refs := []string{claim.ID}
		for _, link := range claim.Evidence[:min(len(claim.Evidence), maxCandidateOrigins-1)] {
			refs = append(refs, link.EvidenceID)
		}
		add("gap", fmt.Sprint(claim.Revision), "Which independent public sources can verify or clarify “"+proposition+"”?",
			"Public claim requires verification", "Find evidence to resolve a gap in the claim", nil, refs,
			Score{Interest: 0.4, Gap: 0.9, Utility: 0.7, Novelty: 0.7, Feasibility: 0.7})
	}
	topics := make(map[string]Topic, len(snapshot.Topics))
	for _, topic := range snapshot.Topics {
		topics[topic.ID] = topic
	}
	type neighbor struct {
		edge  Relation
		other Topic
	}
	neighbors := make(map[string][]neighbor)
	var hubOrder []string
	connect := func(hub Topic, n neighbor) {
		nearby := neighbors[hub.ID]
		if len(nearby) == 0 {
			hubOrder = append(hubOrder, hub.ID)
		}
		if len(nearby) < 4 && !slices.ContainsFunc(nearby, func(old neighbor) bool { return old.other.ID == n.other.ID }) {
			neighbors[hub.ID] = append(nearby, n)
		}
	}
	associationScore := Score{Interest: 0.5, Gap: 0.5, Utility: 0.6, Novelty: 0.9, Feasibility: 0.7}
	directLimit := min(len(result)+MaxFrontierCandidates/4, MaxFrontierCandidates)
	for _, edge := range snapshot.Relations {
		if len(hubOrder) >= 2*MaxFrontierCandidates {
			break
		}
		from, to := topics[edge.FromID], topics[edge.ToID]
		if from.ID == "" || to.ID == "" || from.ID == to.ID ||
			strings.TrimSpace(from.Title) == "" || strings.TrimSpace(to.Title) == "" || len(edge.EvidenceIDs) == 0 ||
			(edge.ValidFrom != nil && now.Before(*edge.ValidFrom)) || (edge.ValidTo != nil && now.After(*edge.ValidTo)) {
			continue
		}
		if len(result) < directLimit {
			refs := append([]string{edge.ID}, edge.EvidenceIDs[:min(len(edge.EvidenceIDs), maxCandidateOrigins-1)]...)
			add("association", edge.ID, "Which public sources explain the connection between “"+from.Title+"” and “"+to.Title+"”?",
				"Verified public evidence connects two topics", "Check the relationship and its limits",
				[]string{from.ID, to.ID}, refs, associationScore)
		}
		connect(from, neighbor{edge, to})
		connect(to, neighbor{edge, from})
	}
	for _, hubID := range hubOrder {
		if len(result) >= MaxFrontierCandidates {
			break
		}
		hub := topics[hubID]
		nearby := neighbors[hubID]
		for i, left := range nearby {
			for _, right := range nearby[i+1:] {
				// The shared topic connects the two cited edges, without asserting
				// any direct relationship between their endpoints.
				refs := []string{left.edge.ID, right.edge.ID}
				for _, edge := range []Relation{left.edge, right.edge} {
					for _, id := range edge.EvidenceIDs[:min(3, len(edge.EvidenceIDs))] {
						if !slices.Contains(refs, id) {
							refs = append(refs, id)
						}
					}
				}
				add("association", hubID, "What links “"+left.other.Title+"” and “"+right.other.Title+"” through “"+hub.Title+"”?",
					"Two cited relationships share a topic", "Check whether the two-step connection is meaningful",
					[]string{left.other.ID, hubID, right.other.ID}, refs, associationScore)
			}
		}
	}
	return result
}
