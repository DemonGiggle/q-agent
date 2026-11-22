package state

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningPublicFrontierHandoff(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	cfg := learning.DefaultConfig()
	cfg.Enabled = true
	cfg.Permissions.AllowDomains = []string{"example.org"}
	now := time.Now().UTC()
	allow := func(string) bool { return true }
	public := learning.Partition{Public: true}
	doc := researchTestDocument("https://example.org/records", "Court records describe the regency and cabinet.")
	reader := localtools.LearningDocumentReader{}
	spans, err := reader.Read(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	citation, err := learning.SelectSourceExcerpt(doc, spans, "regency cabinet", allow)
	if err != nil || citation == nil {
		t.Fatalf("fixture citation: %+v, %v", citation, err)
	}
	record := doc.Source.Record
	claim := learning.Claim{Record: record, ID: "claim", Subject: "Regency", Predicate: "influenced", Object: "cabinet",
		Status: "unresolved", Evidence: []learning.EvidenceLink{{EvidenceID: citation.ID, Relation: "refutes"}},
		RevisedAt: now, Revision: 1}
	data := learningPartition{Schema: learning.SchemaVersion, Snapshot: learning.KnowledgeSnapshot{
		Version: 1, Sources: []learning.Source{doc.Source}, Evidence: []learning.Evidence{*citation}, Claims: []learning.Claim{claim},
		Topics: []learning.Topic{
			{Record: record, ID: "regency", Title: "Regency"},
			{Record: record, ID: "cabinet", Title: "Cabinet"},
		},
		Relations: []learning.Relation{
			{Record: record, ID: "relation", FromID: "regency", ToID: "cabinet", Predicate: "influence", EvidenceIDs: []string{citation.ID}},
			{Record: record, ID: "uncited", FromID: "cabinet", ToID: "regency", Predicate: "related"},
		},
	}}
	if err := Atomic(filepath.Join(c.repo.dir, "public.json"), data); err != nil {
		t.Fatal(err)
	}
	entries, err := c.repo.PublicFrontier(ctx, cfg, now, allow)
	if err != nil || len(entries) != 2 || entries[0].Question.OriginType != "gap" ||
		entries[1].Question.OriginType != "association" ||
		!slices.Equal(entries[0].Question.OriginRefs, []string{claim.ID, citation.ID}) ||
		!slices.Equal(entries[1].Question.OriginRefs, []string{"relation", citation.ID}) {
		t.Fatalf("generated frontier: %+v, %v", entries, err)
	}
	id := entries[0].Question.ID
	selected := learning.SelectFrontier(entries, cfg.Selection, now, allow)
	if len(selected) != 2 || selected[0].Question.ID != id || selected[0].Reason != "weekly_gap" {
		t.Fatalf("selection = %+v", selected)
	}
	// Simulate a restart between durable run creation and the frontier write.
	runID := frontierResearchID("run", "public", id)
	planID := frontierResearchID("plan", "public", id)
	if err := c.create(ctx, public, runID, planID, entries[0].Question.Text, 2); err != nil {
		t.Fatal(err)
	}
	c = NewLearningCoordinator(dir)
	queued, err := c.QueueFrontier(ctx, cfg, public, entries, "next", "", now, allow)
	if err != nil || queued.RunID != runID || queued.CandidateID != id {
		t.Fatalf("handoff was not recovered: %+v, %v", queued, err)
	}
	entries, err = c.repo.PublicFrontier(ctx, cfg, now, allow)
	if err != nil || len(entries) != 2 || entries[0].Question.Status != "queued" ||
		len(entries[0].Transitions) != 2 {
		t.Fatalf("saved frontier: %+v, %v", entries, err)
	}
	if replay, err := c.QueueFrontier(ctx, cfg, public, entries, id, "", now, allow); err != nil || replay != queued {
		t.Fatalf("repeated handoff: %+v, %v", replay, err)
	}
	search := discoverySearch(func(_ context.Context, query string, _ int) ([]learning.SearchHit, learning.Budget, error) {
		if query != entries[0].Question.Text {
			t.Errorf("worker received a different query: %q", query)
		}
		return nil, learning.Budget{Requests: 1}, nil
	})
	fetch := researchFetcher(func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error) {
		t.Fatal("fetch without a source")
		return learning.FetchedDocument{}, learning.Budget{}, nil
	})
	run, err := c.WorkResearch(ctx, cfg, runID, allow, search, fetch, reader, nil)
	if err != nil || run.State != learning.RunUnresolved {
		t.Fatalf("worker handoff = %+v, %v", run, err)
	}
	entries, err = c.RefreshFrontier(ctx, public, entries)
	if err != nil || entries[0].Question.Status != "unresolved" || entries[0].Question.CooldownUntil == nil ||
		!entries[0].Question.CooldownUntil.After(now) || len(entries[0].Transitions) != 3 {
		t.Fatalf("frontier history: %+v, %v", entries, err)
	}
	data, err = c.repo.load("public")
	if err != nil || len(data.Snapshot.Claims) != 1 || data.Snapshot.Claims[0].Status != "unresolved" {
		t.Fatalf("worker promoted a claim without sources: %+v, %v", data.Snapshot.Claims, err)
	}
	// A new revision can generate a new candidate ID, but the same
	// proposition stays suppressed while the previous question cools down.
	data.Snapshot.Claims[0].Revision++
	if err := Atomic(filepath.Join(c.repo.dir, "public.json"), data); err != nil {
		t.Fatal(err)
	}
	refreshed, err := c.repo.PublicFrontier(ctx, cfg, now.Add(time.Minute), allow)
	if err != nil || slices.ContainsFunc(refreshed, func(entry learning.FrontierEntry) bool {
		return entry.Question.OriginType == "gap"
	}) {
		t.Fatalf("equivalent question escaped cooldown: %+v, %v", refreshed, err)
	}
	cfg.Permissions.BlockDomains = []string{"example.org"}
	entries, err = c.repo.PublicFrontier(ctx, cfg, now, allow)
	if err != nil || len(entries) != 0 {
		t.Fatalf("updated source policy did not hide candidates: %+v, %v", entries, err)
	}
	if _, err := c.QueueFrontier(ctx, cfg, public, entries, id, "", now, allow); !errors.Is(err, learning.ErrDenied) {
		t.Fatalf("policy removed lead but it can still be queued: %v", err)
	}
}
