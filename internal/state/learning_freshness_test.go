package state

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

func TestLearningFreshnessAndAssociation(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		fetchErr     error
		changed      bool
		aged         bool
		stale        bool
	}{
		{name: "unchanged", result: "unchanged"},
		{name: "changed", result: "changed", changed: true, stale: true},
		{name: "publication cutoff", result: "aged", aged: true, stale: true},
		{name: "gone", result: "gone", fetchErr: learning.ErrSourceGone, stale: true},
		{name: "withdrawn", result: "withdrawn", fetchErr: learning.ErrSourceWithdrawn, stale: true},
		{name: "temporary failure", result: "inconclusive", fetchErr: learning.ErrRetryableHTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			cfg := learning.DefaultConfig()
			cfg.Enabled, cfg.Schedule.Enabled = true, true
			cfg.SourcePolicy = &learning.ResearchSourcePolicy{RevalidateDays: 7}
			cfg.Permissions.AllowDomains = []string{"example.org", "example.net"}
			now := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
			then := now.AddDate(0, 0, -8)
			record := learning.Record{SchemaVersion: learning.SchemaVersion, CreatedAt: then, UpdatedAt: then}
			repo := NewLearningRepository(dir, func(_ context.Context, _ learning.Partition, _ learning.Lease, _ string, write func() error) error {
				return write()
			})
			allow := func(string) bool { return true }
			const first = "https://example.org/archive"
			const excerpt = "The public archive documents the use of the solar calendar."
			public := learning.Partition{Public: true}
			var snapshot learning.KnowledgeSnapshot
			for _, item := range []struct{ uri, excerpt string }{
				{first, excerpt},
				{"https://example.net/case", "Library records confirm a calendar used for agricultural planning."},
			} {
				doc := researchTestDocument(item.uri, item.excerpt)
				doc.Source.Record, doc.Source.FetchedAt = record, then
				doc.Source.PublishedAt = &then
				if item.uri != first {
					doc.Source.FetchedAt = now.Add(-time.Hour)
				}
				spans, err := (localtools.LearningDocumentReader{}).Read(ctx, doc)
				if err != nil {
					t.Fatal(err)
				}
				ev, err := learning.SelectSourceExcerpt(doc, spans, "calendar", allow)
				if err != nil || ev == nil {
					t.Fatalf("select excerpt = %+v, %v", ev, err)
				}
				snapshot.Sources = append(snapshot.Sources, doc.Source)
				snapshot.Evidence = append(snapshot.Evidence, *ev)
			}
			for _, endpoints := range [][2]string{{"Court archive", "Solar calendar"}, {"Solar calendar", "Farm planning"}} {
				claim := learning.Claim{Record: record, ID: rand.Text(), Subject: endpoints[0], Predicate: "documents",
					Object: endpoints[1], Status: "hypothesis", Revision: 1, RevisedAt: then}
				for _, item := range snapshot.Evidence {
					claim.Evidence = append(claim.Evidence, learning.EvidenceLink{EvidenceID: item.ID, Relation: "supports"})
				}
				snapshot.Claims = append(snapshot.Claims, claim)
			}
			lease := learning.Lease{RunID: rand.Text(), Fence: 1, ExpiresAt: time.Now().Add(time.Minute)}
			if _, err := repo.Commit(ctx, public, 0, lease, rand.Text(), snapshot); err != nil {
				t.Fatal(err)
			}
			for _, claim := range snapshot.Claims {
				result, err := repo.EditPublicClaim(ctx, cfg, ClaimEdit{ClaimID: claim.ID, ExpectedRevision: 1,
					Action: "review", Reason: "Verified both public excerpts"}, allow, then)
				if err != nil || result.Status != "supported" {
					t.Fatalf("claim = %+v, %v", result, err)
				}
			}
			frontier := func() []learning.FrontierEntry {
				t.Helper()
				entries, err := repo.PublicFrontier(ctx, cfg, now, allow)
				if err != nil {
					t.Fatal(err)
				}
				return entries
			}
			entries := frontier()
			view, err := repo.Snapshot(ctx, public)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 3 || len(view.Topics) != 3 || len(view.Relations) != 2 ||
				len(entries[2].Question.TopicIDs) != 3 || !slices.Equal(entries[2].Question.OriginRefs,
				[]string{view.Relations[0].ID, view.Relations[1].ID, snapshot.Evidence[0].ID, snapshot.Evidence[1].ID}) {
				t.Fatalf("association does not trace both edges and their evidence: %+v", entries)
			}
			if !tc.stale && tc.result == "unchanged" {
				claimID := snapshot.Claims[0].ID
				id := rand.Text()
				cfg.Recall.AuditEnabled, cfg.Recall.FrontierFeedback = true, true
				log := LearningAudit{Dir: filepath.Join(dir, "learning", "audit")}
				if err := log.AppendRecall(ctx, learning.Event{SchemaVersion: learning.SchemaVersion,
					Type: learning.RecallUsed, At: now, ID: id, RunID: id, Outcome: "included",
					ObjectRefs: []string{rand.Text()}, ClaimRefs: []string{claimID}}, true); err != nil {
					t.Fatal(err)
				}
				ranked := frontier()
				if ranked[0].Question.ID != entries[0].Question.ID || ranked[0].Question.Score.Final <= entries[0].Question.Score.Final ||
					ranked[1].Question.Score.Final != entries[1].Question.Score.Final {
					t.Fatalf("unexpected recall feedback: %+v", ranked)
				}
			}
			if tc.aged {
				cfg.SourcePolicy.MaxAgeDays = 7
			}
			fetches := 0
			fetch := researchFetcher(func(_ context.Context, uri string, size int64) (learning.FetchedDocument, learning.Budget, error) {
				fetches++
				if uri != first || size != learningDocumentLimit {
					t.Fatalf("unexpected revalidation target: %s (%d)", uri, size)
				}
				body := excerpt
				if tc.changed {
					body = "The public archive documents a new calendar."
				}
				doc := researchTestDocument(uri, body)
				doc.Source.FetchedAt, doc.Source.PublishedAt = now, &then
				return doc, learning.Budget{Requests: 2, Documents: 1}, tc.fetchErr
			})
			tick := func(config learning.Config, window string) {
				t.Helper()
				// Start with a new coordinator on every tick, as after a restart.
				c := NewLearningCoordinator(dir)
				c.now = func() time.Time { return now }
				if err := c.RunScheduledResearch(ctx, config, window, allow, nil, fetch, nil); err != nil {
					t.Fatal(err)
				}
			}
			disabled := cfg
			disabled.SourcePolicy = nil
			tick(disabled, "2026-11-22")
			denied := cfg
			denied.Permissions.AllowDomains = []string{"example.net"}
			tick(denied, "2026-11-21")
			limited := cfg
			limited.PerQuestion.Requests = 1
			tick(limited, "2026-11-22")
			if fetches != 0 {
				t.Fatalf("disabled, denied or out-of-budget revalidation made %d requests", fetches)
			}
			tick(cfg, "2026-11-23")
			tick(cfg, "2026-11-23")
			if fetches != 1 {
				t.Fatalf("fetches = %d across repeated dispatch", fetches)
			}
			quota, err := loadLearningQuota(filepath.Join(dir, "learning", "quota.json"), now.Format(time.DateOnly))
			if err != nil || quota.Spent.Requests != 2 || quota.Spent.Documents != 1 || quota.Spent.Tokens != 0 {
				t.Fatalf("freshness quota = %+v, %v", quota, err)
			}
			view, err = repo.Snapshot(ctx, public)
			if err != nil {
				t.Fatal(err)
			}
			source := view.Sources[0]
			if source.Check == nil || source.Check.Result != tc.result || source.Available() == tc.stale ||
				source.ContentHash != snapshot.Sources[0].ContentHash || !source.FetchedAt.Equal(then) ||
				len(view.Evidence) != 2 || len(view.Sources) != 2 {
				t.Fatalf("source changed original evidence or lost status: %+v", source)
			}
			entries = frontier()
			if tc.stale {
				if len(entries) != 2 || entries[0].Question.OriginType != "gap" || len(view.Revisions) != 4 {
					t.Fatalf("affected claims must produce gaps with history: %+v", entries)
				}
				for _, claim := range view.Claims {
					if claim.Status != "stale" || claim.Revision != 3 || claim.Confidence != 0 ||
						!strings.Contains(claim.ConfidenceReason, tc.result) || len(claim.Evidence) != 2 {
						t.Fatalf("incorrect downgrade: %+v", claim)
					}
				}
				reviewed, err := repo.EditPublicClaim(ctx, cfg, ClaimEdit{ClaimID: view.Claims[0].ID, Action: "review",
					ExpectedRevision: 3, Reason: "Review after source loss"}, allow, now)
				if err != nil || reviewed.Status != "stale" {
					t.Fatalf("invalid source restored claim: %+v, %v", reviewed, err)
				}
				// A new, cited version from the same origin may fill the
				// gap. The original source version remains in history.
				ref := learning.ResearchReference{Source: researchTestDocument(first, "Updated material").Source}
				replacement := snapshot.Evidence[0]
				replacement.ID = "new-evidence"
				ref.Evidence = &replacement
				refs := claimReferences(view, cfg, allow)
				refs[replacement.ID] = ref
				if links := mergeClaimEvidence(refs, view.Claims[0].Evidence,
					[]learning.EvidenceLink{{EvidenceID: replacement.ID, Relation: "supports"}}); len(links) != 3 {
					t.Fatalf("updated source could not replace a stale citation: %+v", links)
				}
			} else if len(entries) != 3 || len(view.Revisions) != 2 {
				t.Fatalf("no source loss should keep both claims and all leads: %+v", entries)
			}
		})
	}
}

func TestLearningNightCarriesInterruptedReservation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := NewLearningCoordinator(dir)
	cfg := learning.DefaultConfig()
	cfg.Enabled, cfg.Schedule.Enabled = true, true
	allow := func(string) bool { return true }
	id, err := c.EnqueueResearch(ctx, cfg, "Public archive records", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ScheduleResearch(ctx, cfg, id, allow); err != nil {
		t.Fatal(err)
	}
	searches := 0
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	search := discoverySearch(func(context.Context, string, int) ([]learning.SearchHit, learning.Budget, error) {
		searches++
		cancel()
		return nil, learning.Budget{Requests: 1}, context.Canceled
	})
	fetch := researchFetcher(func(context.Context, string, int64) (learning.FetchedDocument, learning.Budget, error) {
		t.Fatal("uncertain search was replayed")
		return learning.FetchedDocument{}, learning.Budget{}, nil
	})
	reader := localtools.LearningDocumentReader{}
	if err := c.RunScheduledResearch(workCtx, cfg, "2026-11-21", allow, search, fetch, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted night: %v", err)
	}
	c = NewLearningCoordinator(dir)
	retry := func(window string) {
		t.Helper()
		if err := c.RunScheduledResearch(ctx, cfg, window, allow, search, fetch, reader); err != nil {
			t.Fatal(err)
		}
	}
	retry("2026-11-21")
	prior, err := c.Get(ctx, id)
	if err != nil || prior.Run.State != learning.RunRunning || prior.Run.Spent.Requests != 1 ||
		len(prior.Run.Checkpoint.OpenQuestions) != 1 || prior.Run.Checkpoint.NextIntent != "search" {
		t.Fatalf("overnight checkpoint = %+v, %v", prior, err)
	}
	retry("2026-11-22")
	result, err := c.ResearchStatus(ctx, cfg, id, allow)
	if err != nil || result.State != learning.RunUnresolved || result.StopReason != "interrupted_provider_call" ||
		result.Spent.Requests != 1 || searches != 1 {
		t.Fatalf("next night = %+v, searches = %d, %v", result, searches, err)
	}
}
