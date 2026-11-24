package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"q-agent/internal/learning"
)

func TestLearningRecallCurrentPolicy(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	r := NewLearningRepository(dir, nil)
	now := time.Now().UTC()
	source := learning.Source{ID: "source", CanonicalURI: "https://allowed.example.org/history", FetchedAt: now}
	note := learning.Note{ID: "note", Title: "關白制度", SourceIDs: []string{source.ID}}
	record := learningPartition{Schema: learning.SchemaVersion, Snapshot: learning.KnowledgeSnapshot{
		Version: 1, Notes: []learning.Note{note}, Sources: []learning.Source{source},
	}}
	if err := Atomic(r.dir+"/public.json", record); err != nil {
		t.Fatal(err)
	}
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Permissions.AllowDomains = []string{"example.org"}
	allow := func(string) bool { return true }
	hits, err := r.RecallPublic(ctx, cfg, "關白", allow, 5)
	if err != nil || len(hits) != 1 || len(hits[0].Sources) != 1 || !hits[0].Sources[0].FetchedAt.Equal(now) {
		t.Fatalf("recall = %+v, %v", hits, err)
	}
	if hits, err := r.RecallPublic(ctx, cfg, "site:example.org after:"+now.Format(time.DateOnly), allow, 5); err != nil || len(hits) != 1 {
		t.Fatalf("filtered listing = %+v, %v", hits, err)
	}
	if hits, err := r.LatestPublicLearning(ctx, cfg, allow, 5); err != nil || len(hits) != 1 || hits[0].Note.ID != note.ID {
		t.Fatalf("latest learning = %+v, %v", hits, err)
	}
	if parsed, err := learning.ParseRecallQuery("what did you research?"); err != nil || parsed.Recent {
		t.Fatalf("hardcoded phrase should not set Recent: %+v, %v", parsed, err)
	}
	for _, query := range []string{"關白 site:other.example.org", "關白 before:" + now.Format(time.DateOnly), "Linux 驅動"} {
		if hits, err := r.RecallPublic(ctx, cfg, query, allow, 5); err != nil || len(hits) != 0 {
			t.Fatalf("filtered recall for %q = %+v, %v", query, hits, err)
		}
	}
	cfg.Permissions.BlockDomains = []string{"allowed.example.org"}
	if hits, err := r.RecallPublic(ctx, cfg, "關白", allow, 5); err != nil || len(hits) != 0 {
		t.Fatalf("blocked recall = %+v, %v", hits, err)
	}
	var index LearningKeywordIndex
	if err := index.Rebuild(ctx, learning.Partition{Public: true}, record.Snapshot); err != nil {
		t.Fatal(err)
	}
	private := learning.Partition{Scope: &learning.Scope{Platform: "telegram", ChatID: 1, Visibility: learning.PrivateChat}}
	if _, err := index.Lookup(ctx, private, "關白", 5); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("wrong partition: %v", err)
	}
}
