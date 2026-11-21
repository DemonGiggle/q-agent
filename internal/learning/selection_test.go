package learning

import (
	"fmt"
	"testing"
	"time"
)

func TestFrontierSelectionUsesQuotaAndReallocates(t *testing.T) {
	p := DefaultConfig().Selection
	p.MaxTopics = 10
	now := time.Now().UTC()
	allow := func(string) bool { return true }
	var entries []FrontierEntry
	for i, origin := range []string{"conversation", "gap", "association"} {
		for j := range 10 {
			entries = append(entries, FrontierEntry{Question: Question{
				ID: fmt.Sprintf("%d-%d", i, j), OriginType: origin, Status: "candidate", Depth: 1,
				Score: &Score{Final: 0.1 * float64(3-i)},
			}})
		}
	}
	count := func(selected []SelectedCandidate) [3]int {
		var result [3]int
		for _, hit := range selected {
			result[selectionOrigin(hit.Question.OriginType)]++
		}
		return result
	}
	selected := SelectFrontier(entries, p, now, allow)
	if got := count(selected); got != ([3]int{7, 2, 1}) || selected[0].Reason != "weekly_gap" {
		t.Fatalf("default shortlist: %v, %+v", got, selected)
	}
	p.Conversation, p.Gap, p.Association = 20, 70, 10
	if got := count(SelectFrontier(entries, p, now, allow)); got != ([3]int{2, 7, 1}) {
		t.Fatalf("updated policy: %v", got)
	}
	if got := count(SelectFrontier(entries[:10], p, now, allow)); got != ([3]int{10, 0, 0}) {
		t.Fatalf("empty origins didn't reallocate: %v", got)
	}
}

func TestFrontierSelectionPolicyWinsOverRanking(t *testing.T) {
	p := DefaultConfig().Selection
	p.MaxTopics, p.MaxDepth = 4, 2
	now := time.Now().UTC()
	candidate := func(id, origin string, depth int64, score float64, topics ...string) FrontierEntry {
		return FrontierEntry{Question: Question{
			ID: id, Text: id, OriginType: origin, Status: "candidate", Depth: depth,
			Score: &Score{Final: score}, TopicIDs: topics,
		}}
	}
	entries := []FrontierEntry{
		candidate("best", "conversation", 1, 1, "shared"),
		candidate("pin", "conversation", 1, 0.1, "shared"),
		candidate("override", "association", 1, 0.1, "different"),
		candidate("muted", "conversation", 1, 1),
		candidate("banned", "conversation", 1, 1, "excluded"),
		candidate("excluded-text", "gap", 1, 1),
		candidate("too-deep", "gap", 3, 1),
		candidate("cooldown", "gap", 1, 1),
		candidate("gap", "gap", 1, 0.1),
		candidate("ordinary", "conversation", 1, 0.1),
	}
	cooldown := now.Add(time.Hour)
	entries[7].Question.CooldownUntil = &cooldown
	p.PinnedIDs = []string{"pin", "muted", "too-deep", "cooldown", "excluded-text", "banned"}
	p.MutedIDs = []string{"muted"}
	p.BannedTopicIDs = []string{"excluded"}
	p.PriorityOverrides = map[string]int{"override": 2}
	allow := func(text string) bool { return text != "excluded-text" }
	got := SelectFrontier(entries, p, now, allow)
	if len(got) != 4 || got[0].Question.ID != "pin" ||
		got[1].Question.ID != "override" || got[2].Question.ID != "gap" || got[3].Question.ID != "ordinary" {
		t.Fatalf("shortlist: %+v", got)
	}
	entries[8].Transitions = []FrontierTransition{{Status: "queued", At: now.Add(-time.Hour)}}
	got = SelectFrontier(entries, p, now, allow)
	if len(got) != 4 || got[2].Question.ID != "ordinary" {
		t.Fatalf("a recent queued gap should satisfy weekly preference: %+v", got)
	}
}
