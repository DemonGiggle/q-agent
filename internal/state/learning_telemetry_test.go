package state

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"q-agent/internal/learning"
	"q-agent/internal/localtools"
)

// The two identical questions are run at the same per-question limits. The
// second run observes the same sources and must reuse the retained claim.
func TestLearningTelemetryResumesAndCountsClaimOutcomes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := learning.DefaultConfig()
	cfg.Enabled, cfg.TelemetryEnabled = true, true
	cfg.PerQuestion.Requests, cfg.PerRun.Requests = 16, 16
	cfg.Permissions.AllowDomains = []string{"example.org", "example.net"}
	allow := func(string) bool { return true }
	const question = "Do the public archives confirm the date of the fictional assembly?"
	const first = "https://example.org/assembly"
	const second = "https://example.net/assembly"
	const quote = "The public archives record the fictional assembly in 1950."
	search := discoverySearch(func(_ context.Context, query string, _ int) ([]learning.SearchHit, learning.Budget, error) {
		if query != question {
			t.Fatalf("unexpected query sent: %q", query)
		}
		return []learning.SearchHit{{URI: first}, {URI: second}}, learning.Budget{Requests: 1}, nil
	})
	fetch := researchFetcher(func(_ context.Context, uri string, _ int64) (learning.FetchedDocument, learning.Budget, error) {
		text := quote
		if uri == second {
			text = "The local archives date the fictional assembly to 1951."
		}
		return researchTestDocument(uri, text), learning.Budget{Requests: 2, Documents: 1}, nil
	})
	model := learningTestModel(func(context.Context, learning.StructuredRequest) ([]byte, learning.Budget, error) {
		return []byte(`{"claim":{"subject":"The fictional assembly","predicate":"occurred in","object":"1950","relations":["supports","refutes"]}}`),
			learning.Budget{Requests: 1, Tokens: 70}, nil
	})
	crash := errors.New("test interruption after durable commit")
	c := NewLearningCoordinator(dir)
	var firstClaim string
	for i := range 2 {
		id, err := c.EnqueueResearch(ctx, cfg, question, 2)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			guard := c.repo.guard
			c.repo.guard = func(ctx context.Context, p learning.Partition, lease learning.Lease, op string, write func() error) error {
				if err := guard(ctx, p, lease, op, write); err != nil {
					return err
				}
				return crash
			}
		}
		result, err := c.WorkResearchWithClaims(ctx, cfg, id, allow, search, fetch, localtools.LearningDocumentReader{}, model)
		if i == 0 {
			if !errors.Is(err, crash) || result.State != learning.RunCommitting {
				t.Fatalf("interrupted commit: %+v, %v", result, err)
			}
			c = NewLearningCoordinator(dir)
			result, err = c.WorkResearch(ctx, cfg, id, allow, search, fetch, localtools.LearningDocumentReader{}, nil)
			firstClaim = result.ClaimID
		}
		if err != nil || result.State != learning.RunCompleted || result.ClaimID == "" || result.ClaimID != firstClaim ||
			result.Spent.Requests != 6 {
			t.Fatalf("research completed incorrectly: %+v, %v", result, err)
		}
		for range 2 {
			if _, err := c.ExportResearchAudit(ctx, cfg, id, allow); err != nil {
				t.Fatal(err)
			}
		}
	}
	knowledge, err := c.repo.Snapshot(ctx, learning.Partition{Public: true})
	if err != nil || len(knowledge.Claims) != 1 || knowledge.Claims[0].Status != "disputed" ||
		len(knowledge.Claims[0].Evidence) != 2 || len(knowledge.Revisions) != 1 {
		t.Fatalf("conflict lineage incorrect: %+v, %v", knowledge.Claims, err)
	}
	control := cfg
	control.Enabled = false
	if hits, err := c.repo.RecallPublic(ctx, control, question, allow, 2); !errors.Is(err, learning.ErrDenied) || len(hits) != 0 {
		t.Fatalf("control received knowledge: %d hits, %v", len(hits), err)
	}
	hits, err := c.repo.RecallPublic(ctx, cfg, question, allow, 2)
	if err != nil || len(hits) == 0 || len(hits[0].Claims) != 1 || hits[0].Claims[0].Status != "disputed" ||
		len(hits[0].Sources) != 2 || len(hits[0].Evidence) != 2 {
		t.Fatalf("learning did not recall both sides of the dispute: %+v, %v", hits, err)
	}
	audit := LearningAudit{Dir: filepath.Join(dir, "learning", "audit")}
	now := time.Now().UTC()
	report, err := audit.Metrics(ctx, now.Add(-time.Hour), now.Add(time.Second))
	if err != nil || report.InvalidFiles != 0 || report.ResearchOutcomes[learning.RunCompleted] != 2 ||
		report.Events[learning.SourceFetched] != 4 || report.Events[learning.EvidenceValidated] != 4 ||
		report.Events[learning.ClaimProposed] != 2 || report.Events[learning.KnowledgeCommitted] != 2 ||
		report.Events[learning.ConflictDetected] != 1 || report.CommitOutcomes["new_claim"] != 1 ||
		report.CommitOutcomes["reused_claim"] != 1 || report.TerminalReservations.Requests != 12 ||
		report.TerminalReservations.Documents != 4 || report.TerminalReservations.Tokens != 4864 {
		t.Fatalf("unexpected metrics: %+v, %v", report, err)
	}
	t.Logf("M2 fixture: completed=%d, verified excerpts=%d, new claims=%d, reused claims=%d, disputes=%d, reserved requests=%d",
		report.ResearchOutcomes[learning.RunCompleted], report.Events[learning.EvidenceValidated], report.CommitOutcomes["new_claim"],
		report.CommitOutcomes["reused_claim"], report.Events[learning.ConflictDetected], report.TerminalReservations.Requests)
	files, err := os.ReadDir(audit.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range files {
		raw, err := os.ReadFile(filepath.Join(audit.Dir, item.Name()))
		if err != nil || strings.Contains(string(raw), question) || strings.Contains(string(raw), quote) ||
			strings.Contains(string(raw), first) || strings.Contains(string(raw), knowledge.Sources[0].ContentHash) {
			t.Fatalf("audit file contains source material or failed to load: %v", err)
		}
	}
}

func TestLearningMetricsWindowAndReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	audit := LearningAudit{Dir: dir}
	now := time.Now().UTC()
	events := []learning.Event{
		{Type: learning.RecallUsed, At: now.Add(-time.Second), Outcome: "included", ClaimRefs: []string{rand.Text()}},
		{Type: learning.RecallUsed, At: now, Outcome: "included"},
		{Type: learning.ResearchTransition, At: now.Add(-time.Second), Outcome: string(learning.RunBudgetExhausted),
			Reason: "provider_budget", Spent: &learning.Budget{Requests: 3}},
		{Type: learning.BudgetExceeded, At: now.Add(-time.Second), Outcome: "exhausted"},
		{Type: learning.SafetyBlocked, At: now.Add(-time.Second), Outcome: "model_tool_request"},
	}
	for _, event := range events {
		event.SchemaVersion, event.ID, event.RunID = learning.SchemaVersion, rand.Text(), rand.Text()
		event.ObjectRefs = []string{rand.Text()}
		for range 2 {
			var err error
			if event.Type == learning.RecallUsed {
				err = audit.AppendRecall(ctx, event, true)
			} else {
				err = audit.Append(ctx, event)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, rand.Text()+".json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := audit.Metrics(ctx, now.Add(-time.Second), now)
	if err != nil || report.InvalidFiles != 1 || report.Events[learning.RecallUsed] != 1 ||
		report.RecallNoteRefs != 1 || report.RecallClaimRefs != 1 || report.Events[learning.SafetyBlocked] != 1 ||
		report.ResearchOutcomes[learning.RunBudgetExhausted] != 1 || report.StopReasons["provider_budget"] != 1 ||
		report.TerminalReservations.Requests != 3 {
		t.Fatalf("metrics miscounted replays, feedback or an interval boundary: %+v, %v", report, err)
	}
}
