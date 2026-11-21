package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	SourceBytesSelector   = "source-bytes-v1"
	MaxSourceExcerptBytes = 240
	MaxDocumentSpans      = 128
)

var ErrInvalidSourceContent = errors.New("fetched learning document failed provenance validation")

// SelectSourceExcerpt checks the fetched bytes and selects one keyword-matched
// passage supplied by a reader. It establishes provenance, not factual support.
func SelectSourceExcerpt(doc FetchedDocument, spans []DocumentSpan, question string, allow func(string) bool) (*Evidence, error) {
	hash := sha256.Sum256(doc.Body)
	if len(doc.Body) == 0 || !utf8.Valid(doc.Body) || doc.Source.ContentHash != hex.EncodeToString(hash[:]) {
		return nil, ErrInvalidSourceContent
	}
	terms := excerptTerms(question)
	var selected DocumentSpan
	best := 0
	for _, span := range spans[:min(len(spans), MaxDocumentSpans)] {
		loc := span.Locator
		if loc.Selector != SourceBytesSelector || !validExcerptShape(loc, span.Text) ||
			loc.End > len(doc.Body) || string(doc.Body[loc.Start:loc.End]) != span.Text {
			return nil, ErrInvalidSourceContent
		}

		score := 0
		folded := strings.ToLower(span.Text)
		for _, term := range terms {
			if strings.Contains(folded, term) {
				score++
			}
		}
		if score > best && allow(span.Text) {
			best, selected = score, span
		}
	}
	if best == 0 {
		return nil, nil
	}
	return &Evidence{
		Record: doc.Source.Record, ID: sourceExcerptID(doc.Source, selected.Locator, selected.Text),
		SourceID: doc.Source.ID, Locator: selected.Locator, ExcerptOrSummary: selected.Text,
		ExtractedAt: doc.Source.FetchedAt,
	}, nil
}

// MatchesSourceExcerpt checks the identity and shape of checkpointed evidence.
// Verification against the document bytes happens before checkpointing.
func (e Evidence) MatchesSourceExcerpt(source Source) bool {
	return e.SourceID == source.ID && e.Locator.Selector == SourceBytesSelector &&
		validExcerptShape(e.Locator, e.ExcerptOrSummary) &&
		e.ID == sourceExcerptID(source, e.Locator, e.ExcerptOrSummary)
}

func validExcerptShape(loc Locator, quote string) bool {
	return loc.Start >= 0 && loc.End > loc.Start && loc.End-loc.Start == len(quote) &&
		len(quote) <= MaxSourceExcerptBytes && strings.TrimSpace(quote) == quote && utf8.ValidString(quote)
}

func sourceExcerptID(source Source, loc Locator, quote string) string {
	key := fmt.Sprintf("%s\n%s\n%s\n%d:%d\n%s",
		source.ID, source.ContentHash, loc.Selector, loc.Start, loc.End, quote)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func excerptTerms(question string) []string {
	const limit = 24
	terms := make([]string, 0, limit)
	add := func(term string) {
		if len(terms) == limit {
			return
		}
		for _, prior := range terms {
			if term == prior {
				return
			}
		}
		terms = append(terms, term)
	}
	var prior rune
	var word strings.Builder
	finishWord := func() {
		term := word.String()
		word.Reset()
		if utf8.RuneCountInString(term) < 3 {
			return
		}
		switch term {
		case "about", "and", "are", "can", "could", "did", "does", "find", "for", "from", "had", "has", "have", "how", "into", "not", "please", "should", "the", "was", "what", "when", "where", "which", "who", "why", "with", "would":
			return
		}
		add(term)
	}
	for _, r := range strings.ToLower(question) {
		switch {
		case unicode.Is(unicode.Han, r):
			finishWord()
			if prior != 0 {
				add(string([]rune{prior, r}))
			}
			prior = r
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			prior = 0
			word.WriteRune(r)
		default:
			prior = 0
			finishWord()
		}
	}
	finishWord()
	return terms
}
