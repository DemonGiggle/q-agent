package localtools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"q-agent/internal/learning"
)

type LearningFetch struct {
	Permissions learning.PermissionConfig
}

func (f LearningFetch) Fetch(ctx context.Context, uri string, maxBytes int64) (learning.FetchedDocument, learning.Budget, error) {
	client := newWebClient()
	client.Transport.(*http.Transport).DialContext = dialPublicLearning
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return f.fetch(ctx, client, uri, maxBytes)
}

func (f LearningFetch) fetch(ctx context.Context, client *http.Client, uri string, maxBytes int64) (doc learning.FetchedDocument, usage learning.Budget, err error) {
	if !f.Permissions.ResearchURI(uri) || maxBytes <= 0 || maxBytes > webBodyLimit {
		return doc, usage, learning.ErrDenied
	}
	u, err := url.Parse(uri)
	if err != nil {
		return doc, usage, learning.ErrDenied
	}
	request := func(uri string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "q-agent/1.0")
		usage.Requests++
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("research source request failed")
		}
		return resp, nil
	}
	robots, err := request((&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}).String())
	if err != nil {
		return doc, usage, err
	}
	allowed := allowsLearningRobots(robots)
	robots.Body.Close()
	if !allowed {
		return doc, usage, learning.ErrDenied
	}

	resp, err := request(uri)
	if err != nil {
		return doc, usage, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || learningRobotsOptOut(strings.Join(resp.Header.Values("X-Robots-Tag"), ",")) {
		return doc, usage, learning.ErrDenied
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (media != "text/html" && media != "text/plain") {
		return doc, usage, learning.ErrDenied
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maxBytes || !utf8.Valid(body) {
		return doc, usage, learning.ErrDenied
	}
	usage.Documents = 1
	title := ""
	if media == "text/html" {
		z := html.NewTokenizer(bytes.NewReader(body))
		for kind := z.Next(); kind != html.ErrorToken; kind = z.Next() {
			if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
				if tag := z.Token(); tag.Data == "meta" && learningRobotsOptOut(attr(tag, "content")) &&
					(strings.EqualFold(attr(tag, "name"), "robots") || strings.EqualFold(attr(tag, "name"), "q-agent")) {
					return doc, usage, learning.ErrDenied
				}
			}
		}
		title, _ = readableHTML(body, uri)
		if len(title) > 256 {
			title = ""
		}
	}
	now := time.Now().UTC()
	hash := sha256.Sum256(body)
	contentHash := hex.EncodeToString(hash[:])
	id := sha256.Sum256([]byte(uri + ":" + contentHash))
	return learning.FetchedDocument{
		MediaType: media, Body: body,
		Source: learning.Source{
			Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
			ID:     hex.EncodeToString(id[:]), CanonicalURI: uri, Title: title, Publisher: u.Hostname(),
			FetchedAt: now, ContentHash: contentHash, TrustClass: "public_reference",
		},
	}, usage, nil
}

// An absent robots file is allowed. A present file is supported only when it
// has no exclusions or crawl delays. Ambiguous files fail closed.
func allowsLearningRobots(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return true
	case http.StatusOK:
	default:
		return false
	}
	const limit = 16 << 10
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/plain" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit || !utf8.Valid(body) {
		return false
	}
	for line := range strings.SplitSeq(strings.TrimPrefix(string(body), "\uFEFF"), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "user-agent", "allow", "sitemap":
		case "disallow":
			if strings.TrimSpace(value) != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func learningRobotsOptOut(value string) bool {
	directives := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return r == ',' || r == ':' || unicode.IsSpace(r)
	})
	for i, directive := range directives {
		switch directive {
		case "none", "noindex", "noarchive", "nosnippet":
			return true
		case "max-snippet":
			if i+1 == len(directives) || directives[i+1] != "-1" {
				return true
			}
		}
	}
	return false
}

func dialPublicLearning(ctx context.Context, _ string, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port != "443" {
		return nil, learning.ErrDenied
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if publicLearningIP(ip) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
		}
	}
	return nil, learning.ErrDenied
}

var specialLearningIPs = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func publicLearningIP(ip netip.Addr) bool {
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, prefix := range specialLearningIPs {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
