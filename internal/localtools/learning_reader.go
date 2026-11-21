package localtools

import (
	"bytes"
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"q-agent/internal/learning"
)

type LearningDocumentReader struct{}

var _ learning.DocumentReader = LearningDocumentReader{}

// Read returns contiguous text passages with offsets into the fetched bytes.
// HTML markup, decoded entities and link destinations cannot enter a passage.
func (LearningDocumentReader) Read(ctx context.Context, doc learning.FetchedDocument) ([]learning.DocumentSpan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.Valid(doc.Body) {
		return nil, learning.ErrInvalidSourceContent
	}
	var spans []learning.DocumentSpan
	switch doc.MediaType {
	case "text/plain":
		spans = appendLearningSpans(nil, string(doc.Body), 0)
	case "text/html":
		rawBody := bytes.NewReader(doc.Body)
		z := html.NewTokenizer(rawBody)
		z.SetMaxBuf(len(doc.Body) + 1)
		block := ""
		for kind := z.Next(); kind != html.ErrorToken && len(spans) < learning.MaxDocumentSpans; kind = z.Next() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			switch kind {
			case html.StartTagToken:
				tag := z.Token()
				if learningSkipElement(tag) {
					skipElement(z, tag.Data)
					continue
				}
				switch tag.Data {
				case "p", "li", "blockquote":
					block = tag.Data
				}
			case html.EndTagToken:
				if z.Token().Data == block {
					block = ""
				}
			case html.TextToken:
				if block != "" {
					// The unread bytes are split between the tokenizer
					// buffer and the underlying reader.
					end := len(doc.Body) - rawBody.Len() - len(z.Buffered())
					start := end - len(z.Raw())
					raw := string(doc.Body[start:end])
					if raw == string(z.Text()) {
						spans = appendLearningSpans(spans, raw, start)
					}
				}
			}
		}
	default:
		return nil, learning.ErrInvalidSourceContent
	}
	return spans, ctx.Err()
}

func learningSkipElement(tag html.Token) bool {
	if hiddenHTML(tag.Data) {
		return true
	}
	switch tag.Data {
	case "head", "nav", "aside", "footer", "button", "select", "textarea":
		return true
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return false
	}
	for _, a := range tag.Attr {
		if a.Key == "data-nosnippet" || a.Key == "hidden" ||
			(a.Key == "aria-hidden" && strings.EqualFold(a.Val, "true")) {
			return true
		}
	}
	return false
}

func appendLearningSpans(spans []learning.DocumentSpan, text string, offset int) []learning.DocumentSpan {
	appendSpan := func(start, end int) {
		chunk := text[start:end]
		trimmed := strings.TrimLeftFunc(chunk, unicode.IsSpace)
		start += len(chunk) - len(trimmed)
		quote := strings.TrimRightFunc(trimmed, unicode.IsSpace)
		if len(quote) < 12 || len(quote) > learning.MaxSourceExcerptBytes ||
			strings.ContainsFunc(quote, unicode.IsControl) {
			return
		}
		spans = append(spans, learning.DocumentSpan{
			Locator: learning.Locator{Selector: learning.SourceBytesSelector, Start: offset + start, End: offset + start + len(quote)},
			Text:    quote,
		})
	}

	start := 0
	for pos, r := range text {
		end := pos + utf8.RuneLen(r)
		switch r {
		case '\r', '\n', '。', '！', '？':
		case '.', '!', '?':
			if end < len(text) {
				next, _ := utf8.DecodeRuneInString(text[end:])
				if !unicode.IsSpace(next) {
					continue
				}
			}
		default:
			continue
		}
		appendSpan(start, end)
		if len(spans) == learning.MaxDocumentSpans {
			return spans
		}
		start = end
	}
	appendSpan(start, len(text))
	return spans
}
