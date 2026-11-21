package localtools

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"q-agent/internal/learning"
)

type LearningSearch struct {
	Permissions learning.PermissionConfig
}

var _ learning.SearchProvider = LearningSearch{}

func (s LearningSearch) Search(ctx context.Context, query string, limit int) ([]learning.SearchHit, learning.Budget, error) {
	client := newWebClient()
	// One reservation covers one outbound request, including error paths.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return s.search(ctx, client, duckDuckGoURL, query, limit)
}

func (s LearningSearch) search(ctx context.Context, client *http.Client, endpoint, query string, limit int) ([]learning.SearchHit, learning.Budget, error) {
	u, err := webURL(endpoint)
	if err != nil {
		return nil, learning.Budget{}, err
	}
	if !s.Permissions.AllowsDomain(u.Hostname()) {
		return nil, learning.Budget{}, learning.ErrDenied
	}
	results, err := findWebResults(ctx, client, endpoint, query, limit)
	usage := learning.Budget{Requests: 1}
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return nil, usage, err
	}
	hits := make([]learning.SearchHit, 0, len(results))
	for _, result := range results {
		link, err := url.Parse(result.url)
		if err == nil && s.Permissions.AllowsDomain(link.Hostname()) {
			hits = append(hits, learning.SearchHit{URI: result.url, Title: Limit(result.title), Snippet: Limit(result.snippet)})
		}
	}
	return hits, usage, nil
}
