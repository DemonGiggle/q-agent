package learning

import (
	"context"
	"errors"
	"math/bits"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RankNoteKeywords returns the IDs of matching notes in score and recency
// order. Searches are bounded by the repository snapshot and need no index
// files, model calls, or background updates.
func RankNoteKeywords(ctx context.Context, snapshot KnowledgeSnapshot, query string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(query) > 2000 || limit < 1 || limit > 20 {
		return nil, errors.New("invalid learning keyword query or limit")
	}
	terms := learningTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	evidence := make(map[string]string, len(snapshot.Evidence))
	for _, item := range snapshot.Evidence {
		evidence[item.ID] = strings.ToLower(item.ExcerptOrSummary)
	}
	claims := make(map[string]string, len(snapshot.Claims))
	for _, item := range snapshot.Claims {
		claims[item.ID] = strings.ToLower(item.Subject + " " + item.Predicate + " " + item.Object)
	}
	type match struct {
		index int
		score int
	}
	top := make([]match, 0, limit)
	termMask := func(text string) uint32 {
		var mask uint32
		for j, term := range terms {
			if noteKeywordMatch(text, term) {
				mask |= 1 << j
			}
		}
		return mask
	}
	for i, note := range snapshot.Notes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		title := termMask(strings.ToLower(note.Title))
		var body uint32
		summary := strings.ToLower(note.Summary)
		if !strings.HasPrefix(summary, "public source retrieved") && !strings.HasPrefix(summary, "public sources retrieved") {
			body = termMask(summary)
		}
		for _, id := range note.EvidenceIDs {
			body |= termMask(evidence[id])
		}
		for _, id := range note.ClaimIDs {
			body |= termMask(claims[id])
		}
		score := 2*bits.OnesCount32(title) + bits.OnesCount32(body&^title)
		if score == 0 {
			continue
		}
		pos := sort.Search(len(top), func(j int) bool {
			current := top[j]
			older := snapshot.Notes[current.index]
			if current.score != score {
				return current.score < score
			}
			if (len(note.EvidenceIDs) > 0) != (len(older.EvidenceIDs) > 0) {
				return len(note.EvidenceIDs) > 0
			}
			if !note.UpdatedAt.Equal(older.UpdatedAt) {
				return note.UpdatedAt.After(older.UpdatedAt)
			}
			return note.ID < older.ID
		})
		if pos == limit {
			continue
		}
		if len(top) < limit {
			top = append(top, match{})
		}
		copy(top[pos+1:], top[pos:])
		top[pos] = match{index: i, score: score}
	}
	ids := make([]string, 0, len(top))
	for _, m := range top {
		ids = append(ids, snapshot.Notes[m.index].ID)
	}
	return ids, nil
}

func noteKeywordMatch(text, term string) bool {
	first, _ := utf8.DecodeRuneInString(term)
	if unicode.Is(unicode.Han, first) {
		return strings.Contains(text, term)
	}
	for offset := 0; offset < len(text); {
		at := strings.Index(text[offset:], term)
		if at < 0 {
			return false
		}
		at += offset
		before, _ := utf8.DecodeLastRuneInString(text[:at])
		after, _ := utf8.DecodeRuneInString(text[at+len(term):])
		if !unicode.IsLetter(before) && !unicode.IsNumber(before) &&
			!unicode.IsLetter(after) && !unicode.IsNumber(after) {
			return true
		}
		_, width := utf8.DecodeRuneInString(text[at:])
		offset = at + width
	}
	return false
}
