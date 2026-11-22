// learning-review prepares and scores an offline paired ACA evaluation.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	randv2 "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

type usage struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

type study struct {
	Model            string `json:"model"`
	AnswerTokenLimit int    `json:"answer_token_limit"`
	ResearchLimit    usage  `json:"research_limit"`
	Pairs            []pair `json:"pairs"`
}

type pair struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	Reference string `json:"reference"`
	RecallOff string `json:"recall_off"`
	RecallOn  string `json:"recall_on"`
	Research  usage  `json:"research"`
}

type packet struct {
	ID    string        `json:"packet_id"`
	Cases []blindedPair `json:"cases"`
}

type blindedPair struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	Reference string `json:"reference"`
	A         string `json:"a"`
	B         string `json:"b"`
}

type answerKey struct {
	ID               string          `json:"packet_id"`
	Model            string          `json:"model"`
	AnswerTokenLimit int             `json:"answer_token_limit"`
	ResearchLimit    usage           `json:"research_limit"`
	Research         usage           `json:"research"`
	RecallIsA        map[string]bool `json:"recall_is_a"`
}

type ratings struct {
	ID    string       `json:"packet_id"`
	Cases []caseRating `json:"cases"`
}

type caseRating struct {
	ID         string `json:"id"`
	A          rating `json:"a"`
	B          rating `json:"b"`
	Preference string `json:"preference"`
}

type rating struct {
	Accuracy       *int  `json:"accuracy"`
	Citations      *int  `json:"citations"`
	Uncertainty    *int  `json:"uncertainty"`
	HelpfulContext *int  `json:"helpful_context"`
	Unrelated      *bool `json:"unrelated_context"`
	Usable         *bool `json:"usable"`
	AcceptedClaims *int  `json:"accepted_claims"`
}

type totals struct {
	Accuracy       int `json:"accuracy"`
	Citations      int `json:"citations"`
	Uncertainty    int `json:"uncertainty"`
	HelpfulContext int `json:"helpful_context"`
	Unrelated      int `json:"unrelated_context"`
	Usable         int `json:"usable_answers"`
	AcceptedClaims int `json:"accepted_claims"`
	Preferred      int `json:"preferred"`
}

type ratio struct {
	Requests float64 `json:"requests"`
	Tokens   float64 `json:"tokens"`
}

type report struct {
	ID               string `json:"packet_id"`
	Model            string `json:"model"`
	AnswerTokenLimit int    `json:"answer_token_limit"`
	ResearchLimit    usage  `json:"research_limit"`
	Pairs            int    `json:"pairs"`
	RecallOff        totals `json:"recall_off"`
	RecallOn         totals `json:"recall_on"`
	Ties             int    `json:"ties"`
	Research         usage  `json:"research"`
	PerUsableAnswer  *ratio `json:"research_per_usable_recall_answer"`
	PerAcceptedClaim *ratio `json:"research_per_accepted_recall_claim"`
}

const maxPairs = 200
const maxReservation = 1_000_000_000

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: learning-review prepare --input FILE --output-dir DIR | score --key FILE --ratings FILE")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "prepare":
		input := flags.String("input", "", "paired answers JSON")
		dir := flags.String("output-dir", "", "new directory for packet, key, and ratings template")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *input == "" || *dir == "" {
			return errors.New("prepare requires --input FILE and --output-dir DIR")
		}
		s, err := readJSON[study](*input)
		if err != nil {
			return err
		}
		reviewer, key, template, err := prepare(s)
		if err != nil {
			return err
		}
		if err := os.Mkdir(*dir, 0700); err != nil {
			return err
		}
		for _, file := range []struct {
			name string
			data any
		}{{"packet.json", reviewer}, {"key.json", key}, {"ratings-template.json", template}} {
			if err := writeJSON(filepath.Join(*dir, file.name), file.data); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(output, filepath.Join(*dir, "packet.json"))
		return err
	case "score":
		keyPath := flags.String("key", "", "coordinator's assignment key")
		ratingPath := flags.String("ratings", "", "completed blind ratings")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *keyPath == "" || *ratingPath == "" {
			return errors.New("score requires --key FILE and --ratings FILE")
		}
		key, err := readJSON[answerKey](*keyPath)
		if err != nil {
			return err
		}
		r, err := readJSON[ratings](*ratingPath)
		if err != nil {
			return err
		}
		result, err := score(key, r)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(output)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	default:
		return errors.New("learning-review expects prepare or score")
	}
}

func prepare(s study) (packet, answerKey, ratings, error) {
	id := rand.Text()
	p := packet{ID: id}
	key := answerKey{ID: id, Model: s.Model, AnswerTokenLimit: s.AnswerTokenLimit,
		ResearchLimit: s.ResearchLimit, RecallIsA: make(map[string]bool, len(s.Pairs))}
	r := ratings{ID: id}
	if !validProtocol(s.Model, s.AnswerTokenLimit, s.ResearchLimit) || len(s.Pairs) < 1 || len(s.Pairs) > maxPairs {
		return p, key, r, errors.New("expected a model, answer budget, research limits, and 1–200 pairs")
	}
	for i, pair := range s.Pairs {
		_, seen := key.RecallIsA[pair.ID]
		if !validPairID(pair.ID) || seen || strings.TrimSpace(pair.Question) == "" || strings.TrimSpace(pair.Reference) == "" ||
			strings.TrimSpace(pair.RecallOff) == "" || strings.TrimSpace(pair.RecallOn) == "" || !pair.Research.fits(s.ResearchLimit) {
			return p, key, r, fmt.Errorf("invalid pair or research budget at row %d", i+1)
		}
		item := blindedPair{ID: pair.ID, Question: pair.Question, Reference: pair.Reference, A: pair.RecallOff, B: pair.RecallOn}
		onIsA := randv2.IntN(2) == 0
		if onIsA {
			item.A, item.B = item.B, item.A
		}
		key.RecallIsA[pair.ID] = onIsA
		key.Research.Requests += pair.Research.Requests
		key.Research.Tokens += pair.Research.Tokens
		p.Cases = append(p.Cases, item)
		r.Cases = append(r.Cases, caseRating{ID: pair.ID})
	}
	return p, key, r, nil
}

func score(key answerKey, r ratings) (report, error) {
	result := report{ID: key.ID, Model: key.Model, AnswerTokenLimit: key.AnswerTokenLimit,
		ResearchLimit: key.ResearchLimit, Pairs: len(key.RecallIsA), Research: key.Research}
	if key.ID == "" || r.ID != key.ID || result.Pairs < 1 || result.Pairs > maxPairs || len(r.Cases) != result.Pairs ||
		!validProtocol(key.Model, key.AnswerTokenLimit, key.ResearchLimit) ||
		!key.Research.fits(usage{Requests: key.ResearchLimit.Requests * int64(result.Pairs), Tokens: key.ResearchLimit.Tokens * int64(result.Pairs)}) {
		return result, errors.New("ratings must match the assignment key and cover every pair")
	}
	seen := make(map[string]bool, len(r.Cases))
	for i, item := range r.Cases {
		onIsA, ok := key.RecallIsA[item.ID]
		if !ok || !validPairID(item.ID) || seen[item.ID] || !item.A.valid() || !item.B.valid() ||
			(item.Preference != "A" && item.Preference != "B" && item.Preference != "tie") {
			return result, fmt.Errorf("incomplete, duplicate, or invalid rating at row %d", i+1)
		}
		seen[item.ID] = true
		a, b := &result.RecallOff, &result.RecallOn
		if onIsA {
			a, b = b, a
		}
		a.add(item.A)
		b.add(item.B)
		switch item.Preference {
		case "A":
			a.Preferred++
		case "B":
			b.Preferred++
		case "tie":
			result.Ties++
		}
	}
	result.PerUsableAnswer = key.Research.per(result.RecallOn.Usable)
	result.PerAcceptedClaim = key.Research.per(result.RecallOn.AcceptedClaims)
	return result, nil
}

func validPairID(id string) bool {
	return id != "" && len(id) <= 80 && !strings.ContainsFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
}

func validProtocol(model string, limit int, research usage) bool {
	return strings.TrimSpace(model) != "" && limit > 0 && research.fits(usage{Requests: maxReservation, Tokens: maxReservation})
}

func (u usage) fits(limit usage) bool {
	return u.Requests >= 0 && u.Tokens >= 0 && u.Requests <= limit.Requests && u.Tokens <= limit.Tokens
}

func (u usage) per(n int) *ratio {
	if n == 0 {
		return nil
	}
	return &ratio{Requests: float64(u.Requests) / float64(n), Tokens: float64(u.Tokens) / float64(n)}
}

func (r rating) valid() bool {
	for _, v := range []*int{r.Accuracy, r.Citations, r.Uncertainty, r.HelpfulContext} {
		if v == nil || *v < 0 || *v > 2 {
			return false
		}
	}
	return r.Unrelated != nil && r.Usable != nil && r.AcceptedClaims != nil && *r.AcceptedClaims >= 0 && *r.AcceptedClaims <= 1000
}

func (t *totals) add(r rating) {
	t.Accuracy += *r.Accuracy
	t.Citations += *r.Citations
	t.Uncertainty += *r.Uncertainty
	t.HelpfulContext += *r.HelpfulContext
	t.AcceptedClaims += *r.AcceptedClaims
	if *r.Unrelated {
		t.Unrelated++
	}
	if *r.Usable {
		t.Usable++
	}
}

func readJSON[T any](path string) (T, error) {
	var value T
	f, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer f.Close()
	const maxInput = 4 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return value, err
	}
	if len(data) > maxInput {
		return value, errors.New("review input exceeds 4 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return value, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return value, errors.New("review input must contain a single JSON object")
	}
	return value, nil
}

func writeJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return errors.Join(enc.Encode(value), f.Close())
}
