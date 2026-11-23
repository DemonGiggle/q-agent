package localtools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"q-agent/internal/learning"
)

func TestLearningSearchProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "source example" {
			t.Errorf("query = %q", got)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<a class="result-link" href="https://example.org/evidence">Lead</a>
			<div class="result-snippet">Snippet</div>
			<a class="result-link" href="https://blocked.example.org/skip">Skip</a>
			<a class="result-link" href="https://example.org.evil.org/skip">Lookalike</a>`)
	}))
	defer server.Close()
	// Non-empty AllowDomains restricts results without requiring the search host to be listed.
	s := LearningSearch{Permissions: learning.PermissionConfig{
		AllowDomains: []string{"example.org"},
		BlockDomains: []string{"blocked.example.org"},
	}}
	hits, usage, err := s.search(context.Background(), server.Client(), server.URL, "source example", 5)
	if err != nil || usage.Requests != 1 || len(hits) != 1 || hits[0].URI != "https://example.org/evidence" {
		t.Fatalf("search: %+v, %+v, %v", hits, usage, err)
	}
	// Empty AllowDomains allows all non-blocked results.
	open := LearningSearch{Permissions: learning.PermissionConfig{
		BlockDomains: []string{"blocked.example.org"},
	}}
	hits, usage, err = open.search(context.Background(), server.Client(), server.URL, "source example", 5)
	if err != nil || usage.Requests != 1 || len(hits) != 2 {
		t.Fatalf("open search: %+v, %+v, %v", hits, usage, err)
	}
	s.Permissions.BlockDomains = append(s.Permissions.BlockDomains, "127.0.0.1")
	if _, usage, err := s.search(context.Background(), server.Client(), server.URL, "blocked query", 5); !errors.Is(err, learning.ErrDenied) || usage.Requests != 0 {
		t.Fatalf("blocked provider: %+v, %v", usage, err)
	}
}

func TestLearningHTTPRetryClassification(t *testing.T) {
	const uri = "https://example.org/document"
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		want       bool
	}{
		{"bad gateway", http.StatusBadGateway, "", true},
		{"unavailable", http.StatusServiceUnavailable, "", true},
		{"gateway timeout", http.StatusGatewayTimeout, "", true},
		{"retry after", http.StatusServiceUnavailable, "30", false},
		{"rate limit", http.StatusTooManyRequests, "", false},
		{"forbidden", http.StatusForbidden, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			permissions := learning.PermissionConfig{AllowDomains: []string{"example.org"}}
			search := LearningSearch{Permissions: permissions}
			fetch := LearningFetch{Permissions: permissions}
			client := &http.Client{Transport: learningTransport(func(req *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: http.NoBody}
				if req.URL.Path == "/robots.txt" {
					response.StatusCode = http.StatusNotFound
				} else if tc.retryAfter != "" {
					response.Header.Set("Retry-After", tc.retryAfter)
				}
				return response, nil
			})}
			ctx := context.Background()
			_, usage, err := search.search(ctx, client, uri, "public question", 5)
			if err == nil || errors.Is(err, learning.ErrRetryableHTTP) != tc.want || usage.Requests != 1 {
				t.Fatalf("search usage = %+v, error = %v", usage, err)
			}
			_, usage, err = fetch.fetch(ctx, client, uri, 1024)
			if err == nil || errors.Is(err, learning.ErrRetryableHTTP) != tc.want || usage.Requests != 2 {
				t.Fatalf("fetch usage = %+v, error = %v", usage, err)
			}
		})
	}
}
