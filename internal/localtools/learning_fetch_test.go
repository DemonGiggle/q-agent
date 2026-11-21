package localtools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"q-agent/internal/learning"
)

type learningTransport func(*http.Request) (*http.Response, error)

func (tr learningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return tr(req)
}

func TestLearningFetchPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		robots string
		body   string
		status int
		calls  int64
		ok     bool
	}{
		{"allowed", "User-agent: *\nDisallow:", "<title>Public document</title>", 200, 2, true},
		{"robots exclusion", "User-agent: *\nDisallow: /doc", "", 200, 1, false},
		{"crawl delay", "User-agent: *\nCrawl-delay: 5", "", 200, 1, false},
		{"page opt out", "", "<meta name='robots' content='noindex'>", 200, 2, false},
		{"redirect", "", "", 302, 2, false},
		{"too large", "", strings.Repeat("x", 1025), 200, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
				Transport: learningTransport(func(r *http.Request) (*http.Response, error) {
					header := make(http.Header)
					status, text := tc.status, tc.body
					switch r.URL.Path {
					case "/robots.txt":
						status, text = http.StatusOK, tc.robots
						header.Set("Content-Type", "text/plain")
					case "/doc":
						header.Set("Content-Type", "text/html")
						header.Set("Location", "https://example.org/other")
					default:
						return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
					}
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(text))}, nil
				}),
			}
			f := LearningFetch{Permissions: learning.PermissionConfig{AllowDomains: []string{"example.org"}}}
			doc, usage, err := f.fetch(context.Background(), client, "https://example.org/doc", 1024)
			if (err == nil) != tc.ok || usage.Requests != tc.calls {
				t.Fatalf("fetch: %+v, %+v, err=%v", doc, usage, err)
			}
			if tc.ok && (doc.Source.Title != "Public document" || len(doc.Source.ContentHash) != 64 || len(doc.Body) != 0) {
				t.Fatalf("unexpected source: %+v", doc)
			}
		})
	}

	for _, raw := range []string{"10.0.0.1", "100.100.100.200", "127.0.0.1", "169.254.169.254", "192.168.1.1", "198.18.0.1", "::1"} {
		if publicLearningIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted non-public address %q", raw)
		}
	}
	if !publicLearningIP(netip.MustParseAddr("1.1.1.1")) {
		t.Error("rejected public IPv4 address")
	}
}
