package learning

import (
	"math"
	"testing"
	"time"
)

func TestRankQuestionAndConversationDedup(t *testing.T) {
	ranked := RankQuestion(Score{Interest: 1, Gap: 0.5, Utility: 0.5, Novelty: 0.5, Feasibility: 0.5, Penalty: 0.1, Final: 1})
	if math.Abs(ranked.Final-0.525) > 1e-9 {
		t.Fatalf("weighted score: %+v", ranked)
	}
	if got := RankQuestion(Score{Interest: math.NaN(), Gap: 9, Penalty: 9}); got.Interest != 0 || got.Gap != 1 || got.Final != 0 {
		t.Fatalf("unbounded score: %+v", got)
	}
	now := time.Now().UTC()
	allow := func(string) bool { return true }
	turns := []ConversationTurn{
		{OriginRef: "one", Transcript: "user: Which sources discuss Fujiwara regents and court politics?\nassistant: not a new question?"},
		{OriginRef: "two", Transcript: "user: Could you discuss court politics and Fujiwara regents?"},
		{OriginRef: "three", Transcript: "user: A statement\nassistant: example question?"},
	}
	entries, changed := ConversationCandidates(nil, turns, now, allow)
	if !changed || len(entries) != 1 || len(entries[0].Question.OriginRefs) != 2 ||
		entries[0].Question.Text == "Which sources discuss Fujiwara regents and court politics?" {
		t.Fatalf("candidates: %+v", entries)
	}
	if _, changed := ConversationCandidates(entries, turns, now, allow); changed {
		t.Fatal("replaying the same origins changed the frontier")
	}
	cooldown := now.Add(time.Hour)
	entries[0].Question.CooldownUntil = &cooldown
	if _, changed := ConversationCandidates(entries, []ConversationTurn{
		{OriginRef: "four", Transcript: turns[0].Transcript},
	}, now, allow); changed {
		t.Fatal("bypassed cooldown on a repeated topic")
	}
}
