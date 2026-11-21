package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/state"
)

func TestLearningRecallInInteractiveChat(t *testing.T) {
	r, _, provider := fixture(t)
	r.Learning = learning.DefaultConfig()
	r.Learning.Enabled = true
	r.Learning.Recall = learning.RecallConfig{ChatEnabled: true, AuditEnabled: true}
	r.Learning.Permissions.AllowDomains = []string{"example.org"}
	r.Config.Memory.MaxContextBytes = 3600

	const uri = "https://history.example.org/regency"
	const quote = "藤原氏的關白制度，見古文書第二卷。"
	const query = "請說明關白制度"
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	published := now.AddDate(0, -1, 0)
	record := learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now}
	source := learning.Source{Record: record, ID: "source", CanonicalURI: uri, ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(quote))),
		PublishedAt: &published, FetchedAt: now}
	evidence, err := learning.SelectSourceExcerpt(learning.FetchedDocument{Source: source, Body: []byte(quote)}, []learning.DocumentSpan{{
		Locator: learning.Locator{Selector: learning.SourceBytesSelector, End: len(quote)},
		Text:    quote,
	}}, query, func(string) bool { return true })
	if err != nil || evidence == nil {
		t.Fatalf("evidence fixture = %+v, %v", evidence, err)
	}
	claim := learning.Claim{Record: record, ID: rand.Text(), Subject: "藤原氏", Predicate: "參與", Object: "關白制度",
		Revision: 1, RevisedAt: now, Status: "unresolved", Evidence: []learning.EvidenceLink{{EvidenceID: evidence.ID, Relation: "refutes"}}}
	note := learning.Note{Record: record, ID: rand.Text(), Title: "關白制度", Summary: "整理古文書。",
		SourceIDs: []string{source.ID}, EvidenceIDs: []string{evidence.ID}, ClaimIDs: []string{claim.ID}, TopicIDs: []string{"regency"}, Revision: 1}
	snapshot := learning.KnowledgeSnapshot{
		Version: 1, Sources: []learning.Source{source}, Evidence: []learning.Evidence{*evidence},
		Claims: []learning.Claim{claim}, Notes: []learning.Note{note},
		Topics: []learning.Topic{
			{Record: record, ID: "regency", Title: "攝政制度", TaxonomyPath: []string{"東亞史"}, Aliases: []string{"regency"}},
			{Record: record, ID: "heian", Title: "平安時代", Aliases: []string{"heian"}},
		},
		Relations: []learning.Relation{{
			Record: record, ID: "historical-period", FromID: "regency", Predicate: "period", ToID: "heian", EvidenceIDs: []string{evidence.ID},
		}},
	}
	dir := filepath.Join(r.Config.StateDir, "learning")
	path := filepath.Join(dir, "knowledge", "public.json")
	if err := state.Atomic(path, struct {
		Schema   string                     `json:"schema_version"`
		Snapshot learning.KnowledgeSnapshot `json:"snapshot"`
	}{learning.SchemaVersion, snapshot}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := state.NewLearningRepository(r.Config.StateDir, nil)
	allow := func(string) bool { return true }
	for _, query := range []string{"東亞史", "regency", "heian"} {
		hits, err := repo.RecallPublic(ctx, r.Learning, query, allow, 5)
		if err != nil || len(hits) != 1 || hits[0].Note.BodyRef != "note:"+note.ID {
			t.Fatalf("recall for %q = %+v, %v", query, hits, err)
		}
	}
	read, err := repo.ReadPublicNote(ctx, r.Learning, "note:"+note.ID, allow)
	if err != nil {
		t.Fatal(err)
	}
	body := read.Markdown(nil, now, 32<<10)
	if !strings.Contains(body, uri) || !strings.Contains(body, quote) || !strings.Contains(body, "refutes") {
		t.Fatalf("note lost provenance: %q", body)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("rendering changed the saved snapshot: %v", err)
	}
	blocked := r.Learning
	blocked.Permissions.BlockDomains = []string{"history.example.org"}
	if _, err := repo.ReadPublicNote(ctx, blocked, note.ID, allow); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("note view did not apply its source policy: %v", err)
	}

	for _, tc := range []struct {
		chat  int64
		query string
	}{
		{1, query},
		{1, "如何編譯 Linux 驅動？"},
		{-10, query},
	} {
		if err := r.Handle(ctx, input(int64(len(provider.requests)+1), tc.chat, 0, 1, tc.query)); err != nil {
			t.Fatal(err)
		}
		r.Wait()
	}
	if len(provider.requests) != 3 {
		t.Fatalf("provider request count = %d", len(provider.requests))
	}
	for i, req := range provider.requests {
		hits, size := 0, 0
		for _, m := range req.Messages {
			if m.Origin != priorResearchOrigin {
				continue
			}
			hits++
			size += len(m.Content)
			if !m.Untrusted || m.Role != "user" {
				t.Fatalf("research reference lacks a trust boundary: %+v", m)
			}
			for _, need := range []string{uri, "read 2026-11-20", "published 2026-10-20", quote, "bytes 0–", "unresolved", "refutes"} {
				if !strings.Contains(m.Content, need) {
					t.Fatalf("missing %q in %s", need, m.Content)
				}
			}
		}
		if i == 0 && (hits != 1 || size+len(priorResearchInstruction) > r.Config.Memory.MaxContextBytes/3) {
			t.Fatalf("relevant chat hits=%d, bytes=%d", hits, size)
		}
		if i > 0 && hits != 0 {
			t.Fatalf("unexpected research context in chat request %d", i)
		}
	}
	auditDir := filepath.Join(dir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("recall events = %v, %v", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(auditDir, files[0].Name()))
	var event learning.Event
	if err != nil || json.Unmarshal(raw, &event) != nil || event.Type != learning.RecallUsed ||
		len(event.ObjectRefs) != 1 || event.ObjectRefs[0] != note.ID ||
		len(event.ClaimRefs) != 1 || event.ClaimRefs[0] != claim.ID ||
		strings.Contains(string(raw), query) || strings.Contains(string(raw), quote) || strings.Contains(string(raw), uri) {
		t.Fatalf("recall event = %s, %v", raw, err)
	}
}
