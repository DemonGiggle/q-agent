package state

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"q-agent/internal/learning"
)

func TestLearningRepositoryCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	allowed := true
	guard := func(_ context.Context, _ learning.Partition, _ learning.Lease, fn func() error) error {
		if !allowed {
			return learning.ErrDenied
		}
		return fn()
	}
	public := learning.Partition{Public: true}
	r := NewLearningRepository(dir, guard)
	if _, err := r.Snapshot(ctx, public); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "learning")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty snapshot touched disk: %v", err)
	}
	now := time.Now().UTC()
	record := learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: now, UpdatedAt: now}
	lease := learning.Lease{RunID: rand.Text(), Fence: 1, ExpiresAt: now.Add(time.Minute)}
	operation := rand.Text()
	next := learning.KnowledgeSnapshot{
		Claims: []learning.Claim{
			{Record: record, ID: "claim", Subject: "s", Predicate: "p", Object: "o", Status: "supported",
				Evidence: []learning.EvidenceLink{{EvidenceID: "evidence", Relation: "supports"}}, Revision: 1, RevisedAt: now},
		},
		Revisions: []learning.RevisionEvent{
			{Record: record, ID: "revision", EntityType: "claim", EntityID: "claim",
				NewRevision: 1, Reason: "add claim", OccurredAt: now, EvidenceIDs: []string{"evidence"}},
		},
	}
	if _, err := r.Commit(ctx, public, 0, lease, operation, next); err == nil {
		t.Fatal("accepted missing citation")
	}
	next.Sources = []learning.Source{
		{Record: record, ID: "source", CanonicalURI: "https://example.org/doc", FetchedAt: now, ContentHash: "hash"},
	}
	next.Evidence = []learning.Evidence{
		{Record: record, ID: "evidence", SourceID: "source", Locator: learning.Locator{Start: 10, End: 20},
			ExcerptOrSummary: "summary", ExtractedAt: now},
	}
	if v, err := r.Commit(ctx, public, 0, lease, operation, next); v != 1 || err != nil {
		t.Fatalf("commit = %d, %v", v, err)
	}
	r = NewLearningRepository(dir, guard)
	if v, err := r.Commit(ctx, public, 0, lease, operation, next); v != 1 || err != nil {
		t.Fatalf("retry after restart = %d, %v", v, err)
	}
	next.Claims[0].Object = "different"
	if _, err := r.Commit(ctx, public, 0, lease, operation, next); !errors.Is(err, learning.ErrVersionConflict) {
		t.Fatalf("different retry error = %v", err)
	}
	if _, err := r.Commit(ctx, public, 0, lease, rand.Text(), next); !errors.Is(err, learning.ErrVersionConflict) {
		t.Fatalf("stale version error = %v", err)
	}
	got, err := r.Snapshot(ctx, public)
	if err != nil || got.Version != 1 || len(got.Claims) != 1 || len(got.Revisions) != 1 || got.Claims[0].Object != "o" {
		t.Fatalf("saved snapshot = %+v, %v", got, err)
	}
	private := learning.Partition{Scope: &learning.Scope{Platform: "telegram", ChatID: 1, Visibility: learning.PrivateChat}}
	if other, err := r.Snapshot(ctx, private); other.Version != 0 || err != nil {
		t.Fatalf("private partition was not empty: %+v, %v", other, err)
	}
	allowed = false
	if _, err := r.Commit(ctx, public, 1, lease, rand.Text(), next); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("revoked lease error = %v", err)
	}
}

func TestLearningRepositoryConcurrentWritersAndLimit(t *testing.T) {
	ctx := context.Background()
	r := NewLearningRepository(t.TempDir(), func(_ context.Context, _ learning.Partition, _ learning.Lease, fn func() error) error {
		return fn()
	})
	public := learning.Partition{Public: true}
	lease := learning.Lease{RunID: rand.Text(), Fence: 1, ExpiresAt: time.Now().Add(time.Minute)}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := r.Commit(ctx, public, 0, lease, rand.Text(), learning.KnowledgeSnapshot{})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var commits, conflicts int
	for err := range results {
		switch {
		case err == nil:
			commits++
		case errors.Is(err, learning.ErrVersionConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if commits != 1 || conflicts != 1 {
		t.Fatalf("commits = %d, conflicts = %d", commits, conflicts)
	}
	oversized := learning.KnowledgeSnapshot{Topics: []learning.Topic{{ID: "oversized", Title: strings.Repeat("x", maxLearningPartitionBytes)}}}
	if _, err := r.Commit(ctx, public, 1, lease, rand.Text(), oversized); !errors.Is(err, ErrLearningPartitionFull) {
		t.Fatalf("storage cap error = %v", err)
	}
	if got, err := r.Snapshot(ctx, public); got.Version != 1 || err != nil {
		t.Fatalf("oversized commit replaced snapshot: %+v, %v", got, err)
	}
}
