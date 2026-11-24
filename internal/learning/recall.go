package learning

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

var ErrRecallQuery = errors.New("invalid learning recall query or filter")

type RecallQuery struct {
	Text   string
	Domain string
	After  time.Time
	Before time.Time
	Recent bool
}

func ParseRecallQuery(query string) (RecallQuery, error) {
	var result RecallQuery
	if len(query) > 2000 {
		return result, ErrRecallQuery
	}
	var words []string
	for word := range strings.FieldsSeq(query) {
		key, value, filtered := strings.Cut(word, ":")
		if !filtered {
			words = append(words, word)
			continue
		}
		switch key {
		case "site":
			value = strings.ToLower(value)
			if result.Domain != "" || !validSourcePolicyHost(value) {
				return RecallQuery{}, ErrRecallQuery
			}
			result.Domain = value
		case "after", "before":
			date, err := time.Parse(time.DateOnly, value)
			if err != nil || (key == "after" && !result.After.IsZero()) ||
				(key == "before" && !result.Before.IsZero()) || date.IsZero() {
				return RecallQuery{}, ErrRecallQuery
			}
			if key == "after" {
				result.After = date
			} else {
				result.Before = date
			}
		default:
			words = append(words, word)
		}
	}
	if !result.After.IsZero() && !result.Before.IsZero() && !result.After.Before(result.Before) {
		return RecallQuery{}, ErrRecallQuery
	}
	result.Text = strings.Join(words, " ")
	if result.Text == "" {
		result.Recent = result.Domain != "" || !result.After.IsZero() || !result.Before.IsZero()
	}
	return result, nil
}

func (q RecallQuery) MatchesSource(source Source) bool {
	if q.Domain != "" {
		u, err := url.Parse(source.CanonicalURI)
		if err != nil {
			return false
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host != q.Domain && !strings.HasSuffix(host, "."+q.Domain) {
			return false
		}
	}
	date := source.FetchedAt
	if source.PublishedAt != nil {
		date = *source.PublishedAt
	}
	return (q.After.IsZero() || !date.Before(q.After)) && (q.Before.IsZero() || date.Before(q.Before))
}
