package localtools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
)

type learningTransport func(*http.Request) (*http.Response, error)

func (tr learningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return tr(req)
}

func TestLearningFetchDeclaredPublication(t *testing.T) {
	const og = `<meta property="article:published_time" content="2026-01-02T12:30:00+08:00">`
	const same = `<meta itemprop="datePublished" content="2026-01-02T04:30:00Z">`
	for _, tc := range []struct {
		name, html, want string
	}{
		{"same instant", "<head>" + og + same + "</head>", "2026-01-02T04:30:00Z"},
		{"schema date", `<head><meta itemprop="datePublished" content="2026-01-02"></head>`, "2026-01-02T00:00:00Z"},
		{"conflict", `<head>` + og + `<meta itemprop="datePublished" content="2025-01-02"></head>`, ""},
		{"malformed", `<head>` + og + `<meta itemprop="datePublished" content="yesterday"></head>`, ""},
		{"future", `<head><meta itemprop="datePublished" content="2999-01-02"></head>`, ""},
		{"body only", "<head></head><body>" + og + "</body>", ""},
		{"head after body", "<body><head>" + og + "</head></body>", ""},
		{"modification only", `<head><meta property="article:modified_time" content="2026-01-02"></head>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: learningTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/robots.txt" {
					return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}},
					Body: io.NopCloser(strings.NewReader(tc.html))}, nil
			})}
			fetcher := LearningFetch{Permissions: learning.PermissionConfig{AllowDomains: []string{"example.org"}}}
			doc, usage, err := fetcher.fetch(context.Background(), client, "https://example.org/document", 1024)
			if err != nil || usage.Requests != 2 {
				t.Fatalf("fetch = %+v, %v", usage, err)
			}
			got := ""
			if doc.Source.PublishedAt != nil {
				got = doc.Source.PublishedAt.Format(time.RFC3339)
			}
			if got != tc.want {
				t.Fatalf("declared publication = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLearningFetchPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		robots  string
		body    string
		status  int
		calls   int64
		ok      bool
		outcome error
	}{
		{"allowed", "User-agent: *\nDisallow:", "<title>Public document</title><link rel='canonical' href='https://news.original.example.net/path'>", 200, 2, true, nil},
		{"robots exclusion", "User-agent: *\nDisallow: /doc", "", 200, 1, false, nil},
		{"crawl delay", "User-agent: *\nCrawl-delay: 5", "", 200, 1, false, nil},
		{"page opt out", "", "<meta name='robots' content='noindex'>", 200, 2, false, learning.ErrSourceWithdrawn},
		{"restricted excerpt", "", "<meta name='robots' content='max-snippet:40'>", 200, 2, false, learning.ErrSourceWithdrawn},
		{"redirect", "", "", 302, 2, false, nil},
		{"too large", "", strings.Repeat("x", 1025), 200, 2, false, nil},
		{"missing document", "", "", 404, 2, false, learning.ErrSourceGone},
		{"gone", "", "", 410, 2, false, learning.ErrSourceGone},
		{"rate limited", "", "", 429, 2, false, nil},
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
			for _, outcome := range []error{learning.ErrSourceGone, learning.ErrSourceWithdrawn} {
				if errors.Is(err, outcome) != (tc.outcome == outcome) {
					t.Fatalf("fetch classified %v as %v, expected %v", err, outcome, tc.outcome)
				}
			}
			if tc.ok && (doc.Source.Title != "Public document" || doc.Source.SyndicationOrigin != "example.net" || len(doc.Source.ContentHash) != 64 || string(doc.Body) != tc.body) {
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
