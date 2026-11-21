package localtools

import (
	"context"
	"strings"
	"testing"

	"q-agent/internal/learning"
)

func TestLearningReaderExactByteOffsets(t *testing.T) {
	const valid = "藤原氏在平安時代掌握攝關政治。"
	for _, tc := range []struct {
		media string
		body  string
	}{
		{"text/plain", "   " + valid + "\r\n"},
		{"text/html", "<head><title>Ignored title</title></head><nav><p>Ignored navigation paragraph.</p></nav>" +
			"<div data-nosnippet><p>Ignored opted out paragraph.</p></div>" +
			"<p> &amp; entity content is skipped. </p><p> " + valid + " </p>"},
	} {
		spans, err := (LearningDocumentReader{}).Read(context.Background(), learning.FetchedDocument{
			MediaType: tc.media, Body: []byte(tc.body),
		})
		if err != nil || len(spans) != 1 {
			t.Fatalf("%s got %+v, %v", tc.media, spans, err)
		}
		span := spans[0]
		start := strings.Index(tc.body, valid)
		if span.Text != valid || span.Locator.Selector != learning.SourceBytesSelector ||
			span.Locator.Start != start || span.Locator.End != start+len(valid) {
			t.Fatalf("incorrect byte anchor for %s: %+v", tc.media, span)
		}
	}
}
