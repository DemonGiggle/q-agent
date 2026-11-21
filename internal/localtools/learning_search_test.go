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
	s := LearningSearch{Permissions: learning.PermissionConfig{
		AllowDomains: []string{"example.org", "127.0.0.1"},
		BlockDomains: []string{"blocked.example.org"},
	}}
	hits, usage, err := s.search(context.Background(), server.Client(), server.URL, "source example", 5)
	if err != nil || usage.Requests != 1 || len(hits) != 1 || hits[0].URI != "https://example.org/evidence" {
		t.Fatalf("search: %+v, %+v, %v", hits, usage, err)
	}
	s.Permissions.BlockDomains = append(s.Permissions.BlockDomains, "127.0.0.1")
	if _, usage, err := s.search(context.Background(), server.Client(), server.URL, "blocked query", 5); !errors.Is(err, learning.ErrDenied) || usage.Requests != 0 {
		t.Fatalf("blocked provider: %+v, %v", usage, err)
	}
}
