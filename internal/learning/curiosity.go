package learning

import (
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	candidateHeading      = "Which independent public sources explain the background of “"
	MaxFrontierCandidates = 64
	maxFrontierTurns      = 24
	maxCandidateOrigins   = 8
)

func RankQuestion(score Score) Score {
	unit := func(value float64) float64 {
		if math.IsNaN(value) {
			return 0
		}
		return min(1, max(0, value))
	}
	score.Interest, score.Gap, score.Utility = unit(score.Interest), unit(score.Gap), unit(score.Utility)
	score.Novelty, score.Feasibility, score.Penalty = unit(score.Novelty), unit(score.Feasibility), unit(score.Penalty)
	score.Final = unit(0.25*score.Interest + 0.25*score.Gap + 0.20*score.Utility +
		0.15*score.Novelty + 0.15*score.Feasibility - score.Penalty)
	return score
}

// ConversationCandidates adds related leads from recent explicit user questions.
// Existing entries keep their lifecycle state. The caller provides a scoped
// reader and keeps the returned leads in the same authorized partition.
func ConversationCandidates(entries []FrontierEntry, turns []ConversationTurn, now time.Time, allow func(string) bool) ([]FrontierEntry, bool) {
	entries = slices.Clone(entries)
	changed := false
	for _, turn := range turns[max(0, len(turns)-maxFrontierTurns):] {
		line, _, _ := strings.Cut(turn.Transcript, "\n")
		ask, ok := strings.CutPrefix(line, "user: ")
		ask = strings.TrimSpace(ask)
		if !ok || len(ask) > 256 || turn.OriginRef == "" || allow == nil || !allow(ask) ||
			(!strings.HasSuffix(ask, "?") && !strings.HasSuffix(ask, "？")) {
			continue
		}
		ask = strings.TrimSpace(strings.TrimRight(ask, "?？"))
		normalized := strings.ToLower(strings.Join(strings.Fields(ask), " "))
		terms := learningTerms(normalized)
		if len(terms) == 0 {
			continue
		}
		text := candidateHeading + ask + "”?"
		if !allow(text) {
			continue
		}
		sum := sha256.Sum256([]byte(normalized))
		key := fmt.Sprintf("conversation-%x", sum[:16])
		duplicate := slices.IndexFunc(entries, func(entry FrontierEntry) bool {
			return entry.Question.OriginType == "conversation" && (entry.Question.ID == key ||
				similarQuestionTerms(terms, learningTerms(strings.TrimPrefix(entry.Question.Text, candidateHeading))))
		})
		if duplicate >= 0 {
			q := &entries[duplicate].Question
			if q.Status != "candidate" || q.Score == nil ||
				(q.CooldownUntil != nil && now.Before(*q.CooldownUntil)) ||
				len(q.OriginRefs) >= maxCandidateOrigins || slices.Contains(q.OriginRefs, turn.OriginRef) {
				continue
			}
			q.OriginRefs = append(slices.Clone(q.OriginRefs), turn.OriginRef)
			q.UpdatedAt = now
			score := *q.Score
			score.Interest = 0.5 + 0.1*float64(len(q.OriginRefs)-1)
			ranked := RankQuestion(score)
			q.Score = &ranked
			changed = true
			continue
		}
		if len(entries) >= MaxFrontierCandidates {
			continue
		}
		score := RankQuestion(Score{Interest: 0.5, Gap: 0.6, Utility: 0.5, Novelty: 1, Feasibility: 0.8})
		entries = append(entries, FrontierEntry{
			Question: Question{
				Record: Record{SchemaVersion: SchemaVersion, CreatedAt: now, UpdatedAt: now},
				ID:     key, Text: text, OriginType: "conversation", OriginRefs: []string{turn.OriginRef},
				Reason:        "Explicit question in a recent authorized exchange",
				ExpectedValue: "Find independent sources and clarify the background",
				Status:        "candidate", Depth: 1, Score: &score,
			},
			Transitions: []FrontierTransition{{At: now, Status: "candidate", Reason: "conversation"}},
		})
		changed = true
	}
	return entries, changed
}

func similarQuestionTerms(a, b []string) bool {
	shared, total := 0, len(a)
	for _, term := range b {
		if slices.Contains(a, term) {
			shared++
		} else {
			total++
		}
	}
	return shared >= 2 && shared*3 >= total*2
}
