package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlindReviewRoundTrip(t *testing.T) {
	root := t.TempDir()
	input, output := filepath.Join(root, "answers.json"), filepath.Join(root, "review")
	s := study{
		Model: "fixture", AnswerTokenLimit: 300, ResearchLimit: usage{Requests: 6, Tokens: 200},
		Pairs: []pair{
			{ID: "q1", Question: "Which year do the reports give?", Reference: "The archive date is 1950.",
				RecallOff: "I cannot confirm the year.", RecallOn: "The archive gives 1950.", Research: usage{5, 120}},
			{ID: "q2", Question: "Where was it held?", Reference: "The venue is unknown.",
				RecallOff: "The location is unconfirmed.", RecallOn: "The location is unconfirmed."},
		},
	}
	if err := writeJSON(input, s); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"prepare", "--input", input, "--output-dir", output}
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	if err := run(args, &out); !os.IsExist(err) {
		t.Fatalf("preparing twice must preserve the output directory: %v", err)
	}
	reviewerPath := filepath.Join(output, "packet.json")
	reviewer, err := readJSON[packet](reviewerPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(reviewerPath)
	if err != nil || strings.Contains(string(data), "recall_") || strings.Contains(string(data), "research") ||
		strings.Contains(string(data), "fixture") {
		t.Fatalf("review packet contains treatment or budget: %s, %v", data, err)
	}
	keyPath := filepath.Join(output, "key.json")
	key, err := readJSON[answerKey](keyPath)
	if err != nil || key.ID != reviewer.ID || len(reviewer.Cases) != 2 {
		t.Fatalf("key = %+v, %v", key, err)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("assignment key permissions: %v, %v", info, err)
	}
	filled, err := readJSON[ratings](filepath.Join(output, "ratings-template.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := score(key, filled); err == nil {
		t.Fatal("an unscored template counted as human ratings")
	}
	hit, miss := fixtureRating(2, false, true, 1), fixtureRating(0, true, false, 0)
	for i, item := range reviewer.Cases {
		onIsA := key.RecallIsA[item.ID]
		if onIsA {
			if item.A != s.Pairs[i].RecallOn || item.B != s.Pairs[i].RecallOff {
				t.Fatalf("blinded answers disagree with the key: %+v", item)
			}
			filled.Cases[i].A, filled.Cases[i].B, filled.Cases[i].Preference = hit, miss, "A"
		} else {
			if item.B != s.Pairs[i].RecallOn || item.A != s.Pairs[i].RecallOff {
				t.Fatalf("blinded answers disagree with the key: %+v", item)
			}
			filled.Cases[i].A, filled.Cases[i].B, filled.Cases[i].Preference = miss, hit, "B"
		}
	}
	filled.Cases[1].A, filled.Cases[1].B, filled.Cases[1].Preference = hit, hit, "tie"
	ratingPath := filepath.Join(root, "ratings.json")
	if err := writeJSON(ratingPath, filled); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"score", "--key", keyPath, "--ratings", ratingPath}, &out); err != nil {
		t.Fatal(err)
	}
	result, err := score(key, filled)
	if err != nil || result.Pairs != 2 || result.RecallOn.Accuracy != 4 || result.RecallOff.Accuracy != 2 ||
		result.RecallOn.Preferred != 1 || result.Ties != 1 || result.RecallOff.Unrelated != 1 ||
		result.PerUsableAnswer == nil || *result.PerUsableAnswer != (ratio{2.5, 60}) ||
		result.PerAcceptedClaim == nil || *result.PerAcceptedClaim != (ratio{2.5, 60}) ||
		strings.Contains(out.String(), "archive") {
		t.Fatalf("decoded review: %+v, %v", result, err)
	}
	filled.Cases[1].ID = filled.Cases[0].ID
	if _, err := score(key, filled); err == nil {
		t.Fatal("duplicate ratings accepted")
	}
	filled.ID = "a different packet"
	if _, err := score(key, filled); err == nil {
		t.Fatal("ratings for a different packet accepted")
	}
	s.Pairs[0].Research.Requests = s.ResearchLimit.Requests + 1
	if _, _, _, err := prepare(s); err == nil {
		t.Fatal("a pair exceeding the research limit was accepted")
	}
}

func fixtureRating(value int, unrelated, usable bool, accepted int) rating {
	return rating{Accuracy: &value, Citations: &value, Uncertainty: &value, HelpfulContext: &value,
		Unrelated: &unrelated, Usable: &usable, AcceptedClaims: &accepted}
}
