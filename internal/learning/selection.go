package learning

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

type SelectionConfig struct {
	Conversation      int            `json:"conversation_percent"`
	Gap               int            `json:"gap_percent"`
	Association       int            `json:"association_percent"`
	MaxTopics         int            `json:"max_topics"`
	MaxDepth          int64          `json:"max_depth"`
	PinnedIDs         []string       `json:"pinned_ids,omitempty"`
	MutedIDs          []string       `json:"muted_ids,omitempty"`
	BannedTopicIDs    []string       `json:"banned_topic_ids,omitempty"`
	PriorityOverrides map[string]int `json:"priority_overrides,omitempty"`
}

func (p SelectionConfig) Validate() error {
	for _, weight := range p.weights() {
		if weight < 0 || weight > 100 {
			return fmt.Errorf("learning selection percentage must be between 0 and 100")
		}
	}
	if p.Conversation+p.Gap+p.Association != 100 ||
		p.MaxTopics < 1 || p.MaxTopics > MaxFrontierCandidates || p.MaxDepth < 1 {
		return fmt.Errorf("invalid learning selection percentages or limits")
	}
	return nil
}

func (p SelectionConfig) weights() [3]int {
	return [3]int{p.Conversation, p.Gap, p.Association}
}

func selectionOrigin(origin string) int {
	switch origin {
	case "conversation":
		return 0
	case "gap":
		return 1
	case "association":
		return 2
	}
	return -1
}

type SelectedCandidate struct {
	Question Question `json:"question"`
	Reason   string   `json:"selection_reason"`
}

// SelectFrontier builds a deterministic shortlist. A selected topic occupies
// one slot. Untagged questions each occupy their own topic slot.
func SelectFrontier(entries []FrontierEntry, policy SelectionConfig, now time.Time, allow func(string) bool) []SelectedCandidate {
	result := make([]SelectedCandidate, 0)
	if policy.Validate() != nil || allow == nil {
		return result
	}
	type candidate struct {
		q        Question
		origin   int
		pinned   bool
		priority int
	}
	topics := make(map[string]bool)
	seen := make(map[string]bool)
	var eligible []candidate
	gapServed := false
	weekStart := now.AddDate(0, 0, -7)
	weights := policy.weights()
	for _, entry := range entries {
		q := entry.Question
		origin := selectionOrigin(q.OriginType)
		if origin == 1 {
			for _, tr := range entry.Transitions {
				if tr.Status == "queued" && !tr.At.Before(weekStart) && !tr.At.After(now) {
					gapServed = true
				}
			}
		}
		if q.ID == "" || seen[q.ID] || origin < 0 || q.Status != "candidate" || q.Score == nil ||
			q.Depth > policy.MaxDepth || !allow(q.Text) ||
			(q.CooldownUntil != nil && now.Before(*q.CooldownUntil)) ||
			slices.Contains(policy.MutedIDs, q.ID) || slices.ContainsFunc(q.TopicIDs, func(id string) bool {
			return slices.Contains(policy.BannedTopicIDs, id)
		}) {
			continue
		}
		if slices.ContainsFunc(entry.DependsOn, func(id string) bool {
			return !slices.ContainsFunc(entries, func(dependency FrontierEntry) bool {
				return dependency.Question.ID == id && dependency.Question.Status == "resolved"
			})
		}) {
			continue
		}
		pinned := slices.Contains(policy.PinnedIDs, q.ID)
		priority := q.Priority
		if override, ok := policy.PriorityOverrides[q.ID]; ok {
			priority = override
		}
		if weights[origin] == 0 && !pinned && priority <= 0 {
			continue
		}
		seen[q.ID] = true
		eligible = append(eligible, candidate{q, origin, pinned, priority})
	}
	slices.SortFunc(eligible, func(a, b candidate) int {
		switch {
		case a.pinned && !b.pinned:
			return -1
		case b.pinned && !a.pinned:
			return 1
		}
		return cmp.Or(cmp.Compare(b.priority, a.priority), cmp.Compare(b.q.Score.Final, a.q.Score.Final),
			b.q.UpdatedAt.Compare(a.q.UpdatedAt), cmp.Compare(a.q.ID, b.q.ID))
	})
	counts := [3]int{}
	canSelect := func(c candidate) bool {
		return !slices.ContainsFunc(c.q.TopicIDs, func(id string) bool { return topics[id] })
	}
	for len(result) < policy.MaxTopics {
		index, deficit, reason := -1, 0, "origin_quota"
		for i, c := range eligible {
			if !canSelect(c) {
				continue
			}
			if c.pinned || c.priority > 0 {
				index, reason = i, "manual_priority"
				break
			}
			if c.origin == 1 && !gapServed && policy.Gap > 0 {
				index, reason = i, "weekly_gap"
				break
			}
			want := (len(result)+1)*weights[c.origin] - 100*counts[c.origin]
			if index < 0 || want > deficit {
				index, deficit = i, want
			}
		}
		if index < 0 {
			break
		}
		chosen := eligible[index]
		eligible = slices.Delete(eligible, index, index+1)
		for _, id := range chosen.q.TopicIDs {
			topics[id] = true
		}
		counts[chosen.origin]++
		gapServed = gapServed || chosen.origin == 1
		result = append(result, SelectedCandidate{Question: chosen.q, Reason: reason})
	}
	return result
}
