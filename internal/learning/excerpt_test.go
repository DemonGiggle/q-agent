package learning

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLearningExcerptVerifiesBytesAndPolicy(t *testing.T) {
	const withdrawn = "This paragraph about the Fujiwara regency is excluded."
	const selected = "藤原氏在平安時代掌握攝關政治。"
	const raw = withdrawn + "\n" + selected
	doc := FetchedDocument{
		Body: []byte(raw), Source: Source{ID: "source", ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))},
	}
	candidates := []DocumentSpan{
		{Locator: Locator{Selector: SourceBytesSelector, Start: 0, End: len(withdrawn)}, Text: withdrawn},
		{Locator: Locator{Selector: SourceBytesSelector, Start: len(withdrawn) + 1, End: len(raw)}, Text: selected},
	}
	policy := func(s string) bool { return !strings.Contains(s, "excluded") }
	evidence, err := SelectSourceExcerpt(doc, candidates, "Fujiwara regency 藤原氏如何掌權？", policy)
	if err != nil || evidence == nil || evidence.ExcerptOrSummary != selected || !evidence.MatchesSourceExcerpt(doc.Source) {
		t.Fatalf("selected %+v, err=%v", evidence, err)
	}
	evidence, err = SelectSourceExcerpt(doc, candidates, "unrelated topic", policy)
	if err != nil || evidence != nil {
		t.Fatalf("irrelevant excerpt selected: %+v, %v", evidence, err)
	}
	candidates[0].Text = strings.Replace(withdrawn, "excluded", "approved", 1)
	if _, err := SelectSourceExcerpt(doc, candidates, "Fujiwara regency", policy); !errors.Is(err, ErrInvalidSourceContent) {
		t.Fatalf("forged span returned %v", err)
	}
}
