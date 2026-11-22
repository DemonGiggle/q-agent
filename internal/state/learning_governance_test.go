package state

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningCancelActiveWorker(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	cfg := learning.DefaultConfig()
	cfg.Enabled, cfg.Schedule.Enabled = true, true
	allow := func(string) bool { return true }
	id, err := c.EnqueueResearch(ctx, cfg, "A public research question", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ScheduleResearch(ctx, cfg, id, allow); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(filepath.Join(dir, "learning"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	started := make(chan struct{})
	finished := make(chan error, 1)
	search := discoverySearch(func(ctx context.Context, _ string, _ int) ([]learning.SearchHit, learning.Budget, error) {
		close(started)
		<-ctx.Done()
		return nil, learning.Budget{Requests: 1}, ctx.Err()
	})
	fetch := researchFetcher(func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error) {
		t.Error("cancelled research reached fetch")
		return learning.FetchedDocument{}, learning.Budget{}, nil
	})
	go func() {
		finished <- c.RunScheduledResearch(ctx, cfg, "2026-11-22", allow, search, fetch, localtools.LearningDocumentReader{})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	operator := NewLearningCoordinator(dir)
	request, err := operator.RequestResearchCancel(ctx, cfg, id, allow)
	if err != nil || !request.Requested {
		t.Fatalf("operator cancellation: %+v, %v", request, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	record, err := operator.load(id)
	if err != nil || record.Run.State != learning.RunCancelled || record.ExpiresAt != nil || record.Run.Spent.Requests != 1 ||
		operator.cancelRequested(id) {
		t.Fatalf("cancelled run: %+v, %v", record, err)
	}
	night, err := loadLearningNight(filepath.Join(dir, "learning", "night.json"))
	if err != nil || night.Active != "" || night.Pending != "" {
		t.Fatalf("cancelled run still scheduled: %+v, %v", night, err)
	}
	again, err := operator.RequestResearchCancel(ctx, cfg, id, allow)
	if err != nil || again.Requested || again.State != learning.RunCancelled {
		t.Fatalf("repeat cancellation: %+v, %v", again, err)
	}
	snapshot, err := operator.repo.Snapshot(ctx, learning.Partition{Public: true})
	if err != nil || snapshot.Version != 0 {
		t.Fatalf("cancelled research published knowledge: %+v, %v", snapshot, err)
	}
}

func TestLearningArchiveAndExport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := NewLearningRepository(dir, nil)
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Permissions.AllowDomains = []string{"example.org"}
	allow := func(string) bool { return true }
	source := researchTestDocument("https://example.org/archive", "Verified public archive").Source
	note := learning.Note{Record: source.Record, ID: rand.Text(), Title: "Public archive", SourceIDs: []string{source.ID}, Revision: 1}
	data := learningPartition{Schema: learning.SchemaVersion, Snapshot: learning.KnowledgeSnapshot{
		Version: 1, Sources: []learning.Source{source}, Notes: []learning.Note{note},
	}}
	if err := Atomic(filepath.Join(r.dir, "public.json"), data); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := r.SetLearningNoteArchived(ctx, cfg, note.ID, true, allow, time.Now())
		if err != nil || result.ArchivedAt == nil || result.Revision != 2 {
			t.Fatalf("archived note: %+v, %v", result, err)
		}
	}
	hits, err := r.RecallPublic(ctx, cfg, "archive", allow, 4)
	if err != nil || len(hits) != 0 {
		t.Fatalf("archive in recall: %+v, %v", hits, err)
	}
	direct, err := r.ReadPublicNote(ctx, cfg, note.ID, allow)
	if err != nil || direct.Note.ArchivedAt == nil {
		t.Fatalf("archived note direct read: %+v, %v", direct, err)
	}
	exported, err := r.ExportPublicLearning(ctx, cfg, allow)
	if err != nil || len(exported.Notes) != 1 || len(exported.Sources) != 1 || len(exported.Revisions) != 1 {
		t.Fatalf("export omitted provenance: %+v, %v", exported, err)
	}
	blocked := cfg
	blocked.Permissions.BlockDomains = []string{"example.org"}
	exported, err = r.ExportPublicLearning(ctx, blocked, allow)
	if err != nil || len(exported.Notes)+len(exported.Sources)+len(exported.Revisions) != 0 {
		t.Fatalf("blocked archive exported: %+v, %v", exported, err)
	}
	result, err := r.SetLearningNoteArchived(ctx, cfg, note.ID, false, allow, time.Now())
	if err != nil || result.ArchivedAt != nil || result.Revision != 3 {
		t.Fatalf("restored note: %+v, %v", result, err)
	}
}

func TestLearningNightRejectsNewlyExcludedQuestion(t *testing.T) {
	ctx := context.Background()
	c := NewLearningCoordinator(t.TempDir())
	cfg := learning.DefaultConfig()
	cfg.Enabled, cfg.Schedule.Enabled = true, true
	allow := func(string) bool { return true }
	id, err := c.EnqueueResearch(ctx, cfg, "Example restricted topic", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ScheduleResearch(ctx, cfg, id, allow); err != nil {
		t.Fatal(err)
	}
	next := cfg
	next.Permissions.ExcludedTerms = []string{"restricted topic"}
	search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
		t.Fatal("the revised policy allowed excluded text to be sent")
		return nil, learning.Budget{}, nil
	})
	fetch := researchFetcher(func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error) {
		t.Fatal("the revised policy allowed excluded research to fetch")
		return learning.FetchedDocument{}, learning.Budget{}, nil
	})
	if err := c.RunScheduledResearch(ctx, next, "2026-11-22", allow, search, fetch, localtools.LearningDocumentReader{}); err != nil {
		t.Fatal(err)
	}
	record, err := c.load(id)
	if err != nil || record.Run.State != learning.RunCancelled || record.Run.StopReason != "policy_changed" ||
		record.Run.Spent != (learning.Budget{}) {
		t.Fatalf("excluded research state: %+v, %v", record, err)
	}
	night, err := loadLearningNight(filepath.Join(filepath.Dir(c.dir), "night.json"))
	if err != nil || night.Pending != "" || night.Active != "" {
		t.Fatalf("excluded research still approved: %+v, %v", night, err)
	}
}
