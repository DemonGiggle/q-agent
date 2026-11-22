package state

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningClaimEvolutionAndHistory(t *testing.T) {
	ctx := context.Background()
	public := learning.Partition{Public: true}
	repo := NewLearningRepository(t.TempDir(), func(_ context.Context, _ learning.Partition, _ learning.Lease, _ string, write func() error) error {
		return write()
	})
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Permissions.AllowDomains = []string{"example.org", "example.net", "example.com", "example.edu"}
	allow := func(string) bool { return true }
	now := time.Now().UTC()
	lease := learning.Lease{RunID: rand.Text(), Fence: 1, ExpiresAt: now.Add(time.Minute)}
	snapshot := func() learning.KnowledgeSnapshot {
		s, err := repo.Snapshot(ctx, public)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	commit := func(s learning.KnowledgeSnapshot) {
		if _, err := repo.Commit(ctx, public, s.Version, lease, rand.Text(), s); err != nil {
			t.Fatal(err)
		}
	}
	appendEvidence := func(s *learning.KnowledgeSnapshot, uri, text string) string {
		doc := researchTestDocument(uri, text)
		spans, err := (localtools.LearningDocumentReader{}).Read(ctx, doc)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := learning.SelectSourceExcerpt(doc, spans, "archive", allow)
		if err != nil || ev == nil {
			t.Fatalf("test evidence: %+v, %v", ev, err)
		}
		s.Sources = append(s.Sources, doc.Source)
		s.Evidence = append(s.Evidence, *ev)
		return ev.ID
	}
	newClaim := func(id, subject, object string, refs ...string) learning.Claim {
		result := learning.Claim{Record: learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now},
			ID: id, Subject: subject, Predicate: "opened in", Object: object, Status: "hypothesis", Revision: 1, RevisedAt: now}
		for _, ref := range refs {
			result.Evidence = append(result.Evidence, learning.EvidenceLink{EvidenceID: ref, Relation: "supports"})
		}
		return result
	}
	edit := func(id, action string, revision uint64, configure func(*ClaimEdit)) learning.Claim {
		request := ClaimEdit{ClaimID: id, Action: action, ExpectedRevision: revision, Reason: "Operator verified sources"}
		if configure != nil {
			configure(&request)
		}
		claim, err := repo.EditPublicClaim(ctx, cfg, request, allow, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return claim
	}
	s := snapshot()
	first := appendEvidence(&s, "https://example.org/a", "City archive records show the opening was held in 1950.")
	second := appendEvidence(&s, "https://example.net/b", "The district archive lists the opening ceremony in 1950.")
	base := newClaim("original", "Municipal archive", "1950", first, second)
	if got := evolveResearchClaim(&s, cfg, allow, base, now); got != base.ID {
		t.Fatalf("initial claim ID = %q", got)
	}
	commit(s)
	reviewed := edit(base.ID, "review", 1, nil)
	if reviewed.Status != "supported" || reviewed.Confidence <= 0 || reviewed.Revision != 2 {
		t.Fatalf("review = %+v", reviewed)
	}
	edit(base.ID, "qualify", 2, func(e *ClaimEdit) { e.Aliases = []string{"City archive"} })
	s = snapshot()
	copied := appendEvidence(&s, "https://example.edu/reprint", "City archive records show the opening was held in 1950.")
	newDraft := newClaim("copy", "City Archive", "1950", copied)
	if id := evolveResearchClaim(&s, cfg, allow, newDraft, now); id != base.ID || len(s.Revisions) != 3 {
		t.Fatalf("copied evidence created a revision: ID %q, revisions %d", id, len(s.Revisions))
	}
	refuting := appendEvidence(&s, "https://example.com/c", "Local archive reports record an opening date in 1951.")
	newDraft = newClaim("opposition", "City archive", "1950", refuting)
	newDraft.Evidence[0].Relation = "refutes"
	if id := evolveResearchClaim(&s, cfg, allow, newDraft, now); id != base.ID {
		t.Fatalf("opposition created a new claim %q", id)
	}
	commit(s)
	got := snapshot().Claims[0]
	if got.Status != "disputed" || got.Qualifiers[conflictQualifier] == "" || len(got.Evidence) != 3 || got.Confidence != 0 {
		t.Fatalf("conflict lost provenance: %+v", got)
	}
	if rolled := edit(base.ID, "rollback", 4, nil); rolled.Status != "disputed" || len(rolled.Evidence) != 3 {
		t.Fatalf("rollback hid opposition: %+v", rolled)
	}
	history, err := repo.ClaimHistory(ctx, cfg, base.ID, allow)
	if err != nil || len(history) != 1 || len(history[0].Revisions) != 4 ||
		history[0].Revisions[3].Decision != "CONTRADICT" || history[0].Revisions[3].Before.Status != "supported" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	s = snapshot()
	s.Claims, s.Revisions = nil, nil
	if _, err := repo.Commit(ctx, public, s.Version, lease, rand.Text(), s); err == nil {
		t.Fatal("claim and history were hard-deleted")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, scope := range []string{"city", "county"} {
		wg.Go(func() {
			_, err := repo.EditPublicClaim(ctx, cfg, ClaimEdit{ClaimID: base.ID, Action: "qualify",
				ExpectedRevision: 4, Reason: "Documented extent", Qualifiers: map[string]string{"scope": scope}}, allow, now)
			results <- err
		})
	}
	wg.Wait()
	a, b := <-results, <-results
	if !(a == nil && errors.Is(b, learning.ErrVersionConflict) || b == nil && errors.Is(a, learning.ErrVersionConflict)) {
		t.Fatalf("concurrent claim edits = %v, %v", a, b)
	}
	s = snapshot()
	revised := appendEvidence(&s, "https://example.edu/updated", "An archive records that the revised opening date was 1951.")
	old, replacement := newClaim("old", "District archive", "1950", first, second),
		newClaim("replacement", "District archive", "1951", refuting, revised)
	evolveResearchClaim(&s, cfg, allow, old, now)
	evolveResearchClaim(&s, cfg, allow, replacement, now)
	commit(s)
	edit(old.ID, "review", 1, nil)
	edit(replacement.ID, "review", 1, nil)
	edit(old.ID, "stale", 2, nil)
	edit(old.ID, "supersede", 3, func(e *ClaimEdit) { e.ReplacementID = replacement.ID })
	history, err = repo.ClaimHistory(ctx, cfg, old.ID, allow)
	if err != nil || len(history) != 2 || history[0].Current.Status != "superseded" ||
		history[1].Current.ID != replacement.ID || len(history[0].Revisions) != 4 ||
		history[0].Revisions[3].Before.Status != "stale" {
		t.Fatalf("replacement lineage = %+v, %v", history, err)
	}
}
