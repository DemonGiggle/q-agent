package localtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"q-agent/internal/llm"
)

const webBodyLimit = 1 << 20
const duckDuckGoURL = "https://lite.duckduckgo.com/lite/"

// Deliberately separate from provider clients: no provider headers, cookie jar,
// environment proxy credentials, or custom authentication reach fetched sites.
func newWebClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("too many web redirects (maximum 5)")
			}
			_, err := webURL(req.URL.String())
			return err
		},
	}
}

func webURL(raw string) (*url.URL, error) {
	if len(raw) > 8192 {
		return nil, fmt.Errorf("url exceeds the 8192-byte limit")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("url must be an absolute HTTP(S) URL without embedded credentials")
	}
	return u, nil
}

func executeWeb(ctx context.Context, call llm.ToolCall) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return "", fmt.Errorf("arguments must be a JSON object")
	}
	var query, rawURL string
	count := 5
	for key, value := range fields {
		switch {
		case call.Name == "web_fetch" && key == "url":
			if json.Unmarshal(value, &rawURL) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return "", fmt.Errorf("url must be a string")
			}
		case call.Name == "web_search" && key == "query":
			if json.Unmarshal(value, &query) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return "", fmt.Errorf("query must be a string")
			}
		case call.Name == "web_search" && key == "count":
			if json.Unmarshal(value, &count) != nil || count < 1 || count > 10 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return "", fmt.Errorf("count must be an integer from 1 to 10")
			}
		default:
			return "", fmt.Errorf("unknown argument %q", key)
		}
	}
	client := newWebClient()
	if call.Name == "web_fetch" {
		return fetchWeb(ctx, client, rawURL)
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 2000 {
		return "", fmt.Errorf("query must contain 1–2000 bytes of non-whitespace text")
	}
	return searchWeb(ctx, client, duckDuckGoURL, query, count)
}

type webPage struct {
	url, mediaType string
	data           []byte
	truncated      bool
}

func getWeb(ctx context.Context, client *http.Client, raw string) (page webPage, err error) {
	defer func() {
		// A site's own 20-second timeout is a recoverable tool failure. Only
		// the caller's cancellation/deadline should abort the whole agent run.
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("web request timed out: %v", err)
		}
	}()
	u, err := webURL(raw)
	if err != nil {
		return page, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return page, err
	}
	req.Header.Set("User-Agent", "q-agent/1.0 (text-only web tools)")
	req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9, text/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return page, fmt.Errorf("web request returned HTTP %d (site may require authentication, rate-limit requests, or present a bot challenge)", resp.StatusCode)
	}
	page.url = resp.Request.URL.String()
	contentType := resp.Header.Get("Content-Type")
	data, err := io.ReadAll(io.LimitReader(resp.Body, webBodyLimit+1))
	if err != nil {
		return page, err
	}
	page.truncated = len(data) > webBodyLimit
	if page.truncated {
		data = data[:webBodyLimit]
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	var params map[string]string
	page.mediaType, params, err = mime.ParseMediaType(contentType)
	if err != nil {
		return page, fmt.Errorf("invalid content type")
	}
	if !strings.HasPrefix(page.mediaType, "text/") && page.mediaType != "application/xhtml+xml" && page.mediaType != "application/json" && !strings.HasSuffix(page.mediaType, "+json") && page.mediaType != "application/xml" {
		return page, fmt.Errorf("unsupported content type %q; web tools read HTML and text, not binary files", page.mediaType)
	}
	// Honor declared and HTML meta charsets without allowing decoded input to
	// grow without bound. The same cap applies after gzip decompression above.
	var reader io.Reader = bytes.NewReader(data)
	if page.mediaType == "text/html" || page.mediaType == "application/xhtml+xml" {
		reader, err = charset.NewReader(reader, contentType)
	} else if label := params["charset"]; label != "" {
		reader, err = charset.NewReaderLabel(label, reader)
	}
	if err != nil {
		return page, err
	}
	page.data, err = io.ReadAll(io.LimitReader(reader, webBodyLimit+1))
	if len(page.data) > webBodyLimit {
		page.data = page.data[:webBodyLimit]
		page.truncated = true
	}
	return page, err
}

func fetchWeb(ctx context.Context, client *http.Client, raw string) (string, error) {
	page, err := getWeb(ctx, client, raw)
	if err != nil {
		return "", err
	}
	text := string(page.data)
	title := ""
	if page.mediaType == "text/html" || page.mediaType == "application/xhtml+xml" {
		title, text = readableHTML(page.data, page.url)
	}
	out := "URL: " + page.url + "\n"
	if title != "" {
		out += "Title: " + title + "\n"
	}
	out += "\n" + text
	if page.truncated {
		out += Truncation
	}
	return Limit(out), nil
}

func attr(t html.Token, name string) string {
	for _, a := range t.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func hasClass(t html.Token, names ...string) bool {
	for _, class := range strings.Fields(attr(t, "class")) {
		for _, name := range names {
			if class == name {
				return true
			}
		}
	}
	return false
}

func hiddenHTML(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "template", "svg", "canvas":
		return true
	}
	return false
}

func skipElement(z *html.Tokenizer, tag string) {
	depth := 1
	for depth > 0 {
		kind := z.Next()
		if kind == html.ErrorToken {
			return
		}
		t := z.Token()
		if t.Data == tag {
			if kind == html.StartTagToken {
				depth++
			}
			if kind == html.EndTagToken {
				depth--
			}
		}
	}
}

// Tokenization, rather than building a DOM, keeps malformed/deep pages bounded.
func elementText(z *html.Tokenizer, tag string) string {
	var out strings.Builder
	depth := 1
	for depth > 0 {
		kind := z.Next()
		t := z.Token()
		switch kind {
		case html.ErrorToken:
			return strings.Join(strings.Fields(out.String()), " ")
		case html.StartTagToken:
			if hiddenHTML(t.Data) {
				skipElement(z, t.Data)
				continue
			}
			if t.Data == tag {
				depth++
			}
		case html.EndTagToken:
			if t.Data == tag {
				depth--
			}
		case html.TextToken:
			out.WriteString(t.Data)
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func readableHTML(data []byte, rawURL string) (string, string) {
	z := html.NewTokenizer(bytes.NewReader(data))
	z.SetMaxBuf(webBodyLimit + 1)
	base, _ := url.Parse(rawURL)
	var out strings.Builder
	title := ""
	truncated := false
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		t := z.Token()
		if kind == html.StartTagToken {
			if t.Data == "title" {
				title = elementText(z, t.Data)
				continue
			}
			if hiddenHTML(t.Data) {
				skipElement(z, t.Data)
				continue
			}
		}
		switch kind {
		case html.TextToken:
			out.WriteString(t.Data)
		case html.EndTagToken, html.StartTagToken, html.SelfClosingTagToken:
			switch t.Data {
			case "p", "div", "article", "section", "header", "footer", "nav", "h1", "h2", "h3", "h4", "h5", "h6", "li", "ul", "ol", "br", "tr", "pre", "blockquote":
				out.WriteByte('\n')
			case "td", "th":
				out.WriteByte(' ')
			case "a":
				if kind == html.StartTagToken {
					text := elementText(z, "a")
					out.WriteString(text)
					if u, err := url.Parse(attr(t, "href")); err == nil && attr(t, "href") != "" {
						u = base.ResolveReference(u)
						if _, err := webURL(u.String()); err == nil {
							out.WriteString(" (" + u.String() + ")")
						}
					}
				}
			}
		}
		// Resolving many short relative links against a long base URL can
		// expand beyond the bounded input. Bound intermediate text as well.
		if out.Len() > webBodyLimit {
			truncated = true
			break
		}
	}
	var lines []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	text := strings.Join(lines, "\n")
	if truncated {
		text += Truncation
	}
	return title, text
}

type searchResult struct{ title, url, snippet string }

func searchWeb(ctx context.Context, client *http.Client, endpoint, query string, count int) (string, error) {
	u, err := webURL(endpoint)
	if err != nil {
		return "", err
	}
	params := u.Query()
	params.Set("q", query)
	u.RawQuery = params.Encode()
	page, err := getWeb(ctx, client, u.String())
	if err != nil {
		return "", err
	}
	if page.truncated {
		return "", fmt.Errorf("DuckDuckGo response exceeded the 1 MiB input limit")
	}
	if page.mediaType != "text/html" && page.mediaType != "application/xhtml+xml" {
		return "", fmt.Errorf("DuckDuckGo returned a non-HTML response")
	}
	results, err := parseSearch(page.data, count)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "DuckDuckGo search: %s\n", query)
	if len(results) == 0 {
		out.WriteString("No results found.\n")
	}
	for i, result := range results {
		fmt.Fprintf(&out, "\n%d. %s\n%s\n%s\n", i+1, result.title, result.url, result.snippet)
	}
	return Limit(out.String()), nil
}

func parseSearch(data []byte, count int) ([]searchResult, error) {
	z := html.NewTokenizer(bytes.NewReader(data))
	z.SetMaxBuf(webBodyLimit + 1)
	results := []searchResult{}
	seen := map[string]bool{}
	noResults := false
	current := -1
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if z.Err() != io.EOF {
				return nil, fmt.Errorf("invalid DuckDuckGo response: %w", z.Err())
			}
			break
		}
		t := z.Token()
		if kind != html.StartTagToken {
			continue
		}
		if attr(t, "id") == "challenge-form" || hasClass(t, "anomaly-modal", "anomaly-modal__modal") || strings.Contains(attr(t, "action"), "/anomaly.js") {
			return nil, fmt.Errorf("DuckDuckGo requires a bot challenge; search is temporarily unavailable")
		}
		if hasClass(t, "no-results", "no-results__message", "result--no-result") {
			noResults = true
		}
		if t.Data == "a" && hasClass(t, "result-link", "result__a") {
			title := elementText(z, "a")
			current = -1
			u, err := url.Parse(attr(t, "href"))
			if err != nil {
				continue
			}
			base, _ := url.Parse(duckDuckGoURL)
			u = base.ResolveReference(u)
			if (u.Hostname() == "duckduckgo.com" || strings.HasSuffix(u.Hostname(), ".duckduckgo.com")) && u.Path == "/l/" {
				u, err = url.Parse(u.Query().Get("uddg"))
				if err != nil {
					continue
				}
			}
			if _, err := webURL(u.String()); err != nil || title == "" || seen[u.String()] || len(results) >= count {
				continue
			}
			seen[u.String()] = true
			results = append(results, searchResult{title: title, url: u.String()})
			current = len(results) - 1
		} else if hasClass(t, "result-snippet", "result__snippet") && current >= 0 {
			results[current].snippet = elementText(z, t.Data)
		}
	}
	if len(results) == 0 && !noResults {
		return nil, fmt.Errorf("DuckDuckGo returned no recognizable results (possible bot challenge or page format change)")
	}
	return results, nil
}
